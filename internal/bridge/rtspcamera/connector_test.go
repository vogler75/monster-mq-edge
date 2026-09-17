package rtspcamera

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
)

type mockSubscriber struct {
	mu   sync.Mutex
	subs map[int]chan LocalMessage
	next int
}

func newMockSubscriber() *mockSubscriber {
	return &mockSubscriber{subs: make(map[int]chan LocalMessage)}
}

func (s *mockSubscriber) Subscribe(filters []string, buffer int) (int, <-chan LocalMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ch := make(chan LocalMessage, buffer)
	s.subs[s.next] = ch
	return s.next, ch
}

func (s *mockSubscriber) Unsubscribe(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[id]; ok {
		close(ch)
		delete(s.subs, id)
	}
}

func (s *mockSubscriber) emit(msg LocalMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantErrs  int
		errSubstr string
	}{
		{
			name:     "valid default config",
			modify:   func(c *Config) {},
			wantErrs: 0,
		},
		{
			name: "missing url",
			modify: func(c *Config) {
				c.URL = ""
			},
			wantErrs:  1,
			errSubstr: "url is required",
		},
		{
			name:     "valid HTTP MJPEG URL",
			modify:   func(c *Config) { c.URL = "http://example.com/stream" },
			wantErrs: 0,
		},
		{
			name:     "valid WebSocket MJPEG URL",
			modify:   func(c *Config) { c.URL = "ws://example.com/stream" },
			wantErrs: 0,
		},
		{
			name:     "valid secure WebSocket MJPEG URL",
			modify:   func(c *Config) { c.URL = "wss://example.com/stream" },
			wantErrs: 0,
		},
		{
			name: "invalid url scheme",
			modify: func(c *Config) {
				c.URL = "ftp://example.com"
			},
			wantErrs:  1,
			errSubstr: "url must be",
		},
		{
			name: "invalid mode",
			modify: func(c *Config) {
				c.Mode = "INVALID"
			},
			wantErrs:  1,
			errSubstr: "invalid mode",
		},
		{
			name: "zero slots",
			modify: func(c *Config) {
				c.Slots = 0
			},
			wantErrs:  1,
			errSubstr: "slots must be at least 1",
		},
		{
			name: "too low intervalMs",
			modify: func(c *Config) {
				c.IntervalMs = 10
			},
			wantErrs:  1,
			errSubstr: "intervalMs must be at least 50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.URL = "rtsp://127.0.0.1:8554/live"
			tt.modify(&cfg)
			errs := cfg.Validate()
			if len(errs) != tt.wantErrs {
				t.Fatalf("expected %d errors, got %d: %v", tt.wantErrs, len(errs), errs)
			}
			if tt.errSubstr != "" && len(errs) > 0 {
				found := false
				for _, e := range errs {
					if len(e) >= len(tt.errSubstr) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error substring %q in %v", tt.errSubstr, errs)
				}
			}
		})
	}
}

func TestRoundRobinSnapshotPublishing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var mu sync.Mutex
	published := make(map[string][]byte)

	pubFn := func(topic string, payload []byte, retain bool, qos byte) error {
		mu.Lock()
		defer mu.Unlock()
		published[topic] = payload
		return nil
	}

	cfg := Config{
		URL:             "rtsp://127.0.0.1:8554/cam",
		TopicPrefix:     "cameras/test_cam",
		Mode:            ModeContinuous,
		IntervalMs:      1000,
		Slots:           3,
		Retain:          true,
		QoS:             0,
		PublishMetadata: true,
	}

	c := NewConnector("test_cam", "node1", cfg, pubFn, nil, logger)

	// Fake JPEG bytes (starts with SOI 0xFF, 0xD8 and ends with EOI 0xFF, 0xD9)
	fakeFrame := []byte{0xFF, 0xD8, 0x01, 0x02, 0x03, 0xFF, 0xD9}

	// Capture 1 -> Slot 1
	if err := c.publishSnapshot(fakeFrame, "test"); err != nil {
		t.Fatalf("publish 1 failed: %v", err)
	}

	mu.Lock()
	pic1, hasPic1 := published["cameras/test_cam/capture/frames/1"]
	meta1Bytes, hasMeta1 := published["cameras/test_cam/capture/frames/1/meta"]
	latestBytes, hasLatest := published["cameras/test_cam/capture/latest"]
	latestPic, hasLatestPic := published["cameras/test_cam/capture/latest/pic"]
	latestMetaBytes, hasLatestMeta := published["cameras/test_cam/capture/latest/meta"]
	mu.Unlock()

	if !hasPic1 || len(pic1) != len(fakeFrame) {
		t.Errorf("expected pic on slot 1, got has=%v, len=%d", hasPic1, len(pic1))
	}
	if !hasMeta1 {
		t.Fatalf("expected metadata on slot 1")
	}

	var meta1 SnapshotMeta
	if err := json.Unmarshal(meta1Bytes, &meta1); err != nil {
		t.Fatalf("failed to unmarshal slot 1 meta: %v", err)
	}
	if meta1.Slot != 1 {
		t.Errorf("expected slot 1, got %d", meta1.Slot)
	}
	if meta1.Timestamp == "" || meta1.TimestampMs == 0 {
		t.Errorf("expected non-empty timestamp, got %q, %d", meta1.Timestamp, meta1.TimestampMs)
	}
	if meta1.Bytes != len(fakeFrame) {
		t.Errorf("expected bytes=%d, got %d", len(fakeFrame), meta1.Bytes)
	}
	if meta1.ContentType != "image/jpeg" {
		t.Errorf("expected contentType=image/jpeg, got %q", meta1.ContentType)
	}

	if !hasLatest {
		t.Fatalf("expected latest pointer topic published")
	}
	if !hasLatestPic || string(latestPic) != string(fakeFrame) {
		t.Fatalf("expected latest/pic to contain the current JPEG")
	}
	if !hasLatestMeta {
		t.Fatalf("expected latest/meta topic published")
	}
	var latestMeta SnapshotMeta
	if err := json.Unmarshal(latestMetaBytes, &latestMeta); err != nil {
		t.Fatalf("failed to unmarshal latest metadata: %v", err)
	}
	if latestMeta.Topic != "cameras/test_cam/capture/latest/pic" || latestMeta.Slot != 1 {
		t.Errorf("unexpected latest metadata: %+v", latestMeta)
	}
	var latest LatestPointer
	if err := json.Unmarshal(latestBytes, &latest); err != nil {
		t.Fatalf("failed to unmarshal latest pointer: %v", err)
	}
	if latest.Slot != 1 {
		t.Errorf("expected latest.Slot = 1, got %d", latest.Slot)
	}
	if latest.PicTopic != "cameras/test_cam/capture/frames/1" || latest.MetaTopic != "cameras/test_cam/capture/frames/1/meta" {
		t.Errorf("latest pointer references unexpected topics: %+v", latest)
	}

	// Capture 2 -> Slot 2
	if err := c.publishSnapshot(fakeFrame, "test"); err != nil {
		t.Fatalf("publish 2 failed: %v", err)
	}
	mu.Lock()
	_, hasPic2 := published["cameras/test_cam/capture/frames/2"]
	mu.Unlock()
	if !hasPic2 {
		t.Errorf("expected pic on slot 2")
	}

	// Capture 3 -> Slot 3
	if err := c.publishSnapshot(fakeFrame, "test"); err != nil {
		t.Fatalf("publish 3 failed: %v", err)
	}
	mu.Lock()
	_, hasPic3 := published["cameras/test_cam/capture/frames/3"]
	mu.Unlock()
	if !hasPic3 {
		t.Errorf("expected pic on slot 3")
	}

	// Capture 4 -> Wraps around to Slot 1!
	updatedFrame := []byte{0xFF, 0xD8, 0xAA, 0xBB, 0xFF, 0xD9}
	if err := c.publishSnapshot(updatedFrame, "test"); err != nil {
		t.Fatalf("publish 4 failed: %v", err)
	}

	mu.Lock()
	pic1Wrap := published["cameras/test_cam/capture/frames/1"]
	meta1WrapBytes := published["cameras/test_cam/capture/frames/1/meta"]
	latestWrapBytes := published["cameras/test_cam/capture/latest"]
	latestWrapPic := published["cameras/test_cam/capture/latest/pic"]
	latestWrapMetaBytes := published["cameras/test_cam/capture/latest/meta"]
	mu.Unlock()

	if len(pic1Wrap) != len(updatedFrame) || pic1Wrap[2] != 0xAA {
		t.Errorf("expected slot 1 overwritten with updated frame, got %v", pic1Wrap)
	}

	var metaWrap SnapshotMeta
	if err := json.Unmarshal(meta1WrapBytes, &metaWrap); err != nil {
		t.Fatalf("failed to unmarshal slot 1 wrap meta: %v", err)
	}
	if metaWrap.Slot != 1 || metaWrap.Bytes != len(updatedFrame) {
		t.Errorf("expected slot 1 with updated bytes, got slot=%d, bytes=%d", metaWrap.Slot, metaWrap.Bytes)
	}

	var latestWrap LatestPointer
	if err := json.Unmarshal(latestWrapBytes, &latestWrap); err != nil {
		t.Fatalf("failed to unmarshal latest wrap pointer: %v", err)
	}
	if latestWrap.Slot != 1 {
		t.Errorf("expected latest slot 1 after wrap, got %d", latestWrap.Slot)
	}
	if string(latestWrapPic) != string(updatedFrame) {
		t.Errorf("expected latest/pic to be overwritten with the newest frame")
	}
	var latestWrapMeta SnapshotMeta
	if err := json.Unmarshal(latestWrapMetaBytes, &latestWrapMeta); err != nil {
		t.Fatalf("failed to unmarshal updated latest metadata: %v", err)
	}
	if latestWrapMeta.Bytes != len(updatedFrame) || latestWrapMeta.Topic != "cameras/test_cam/capture/latest/pic" {
		t.Errorf("expected latest/meta to match the newest frame, got %+v", latestWrapMeta)
	}

	// Verify metrics
	metrics := c.Metrics()
	if metrics.CurrentSlot != 1 {
		t.Errorf("expected metrics.CurrentSlot = 1, got %d", metrics.CurrentSlot)
	}
	if metrics.SnapshotsPublished != 4 {
		t.Errorf("expected 4 snapshots published, got %v", metrics.SnapshotsPublished)
	}
}

func TestTriggerSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var mu sync.Mutex
	published := make(map[string][]byte)

	pubFn := func(topic string, payload []byte, retain bool, qos byte) error {
		mu.Lock()
		defer mu.Unlock()
		published[topic] = payload
		return nil
	}

	cfg := Config{
		URL:             "rtsp://127.0.0.1:8554/cam",
		TopicPrefix:     "cameras/trigger_cam",
		Mode:            ModeTriggered,
		Slots:           2,
		TriggerTopic:    "cameras/trigger_cam/trigger",
		Retain:          false,
		QoS:             0,
		PublishMetadata: true,
	}

	mockSub := newMockSubscriber()
	c := NewConnector("trigger_cam", "node1", cfg, pubFn, mockSub, logger)

	// Trigger without cached frame should return error
	if err := c.TriggerSnapshot(); err == nil {
		t.Fatalf("expected error triggering with no frame, got nil")
	}

	// Set frame and trigger
	fakeFrame := []byte{0xFF, 0xD8, 0x11, 0x22, 0xFF, 0xD9}
	c.setLatestFrame(fakeFrame)

	if err := c.TriggerSnapshot(); err != nil {
		t.Fatalf("trigger snapshot failed: %v", err)
	}

	mu.Lock()
	pic1, hasPic1 := published["cameras/trigger_cam/capture/frames/1"]
	meta1Bytes, hasMeta1 := published["cameras/trigger_cam/capture/frames/1/meta"]
	mu.Unlock()

	if !hasPic1 || len(pic1) != len(fakeFrame) {
		t.Errorf("expected pic on slot 1 from trigger")
	}
	if !hasMeta1 {
		t.Fatalf("expected meta on slot 1 from trigger")
	}

	var meta SnapshotMeta
	_ = json.Unmarshal(meta1Bytes, &meta)
	if meta.Trigger != "manual" {
		t.Errorf("expected trigger source manual, got %q", meta.Trigger)
	}
	mu.Lock()
	_, hasSnapshotPic := published["cameras/trigger_cam/capture/snapshot/pic"]
	_, hasSnapshotMeta := published["cameras/trigger_cam/capture/snapshot/meta"]
	mu.Unlock()
	if hasSnapshotPic || hasSnapshotMeta {
		t.Error("manual capture must not publish topic-triggered snapshot topics")
	}
}

func TestLatestFrameIsPublishedOnlyOnce(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	publishCount := 0
	pubFn := func(string, []byte, bool, byte) error {
		publishCount++
		return nil
	}
	cfg := DefaultConfig()
	cfg.URL = "rtsp://127.0.0.1/cam"
	cfg.PublishMetadata = false
	c := NewConnector("camera", "node1", cfg, pubFn, nil, logger)
	frame := []byte{0xff, 0xd8, 0x01, 0xff, 0xd9}

	c.setLatestFrame(frame)
	if err := c.publishLatestSnapshot("continuous"); err != nil {
		t.Fatal(err)
	}
	firstCount := publishCount
	if err := c.publishLatestSnapshot("continuous"); err != nil {
		t.Fatal(err)
	}
	if publishCount != firstCount {
		t.Fatalf("unchanged cached frame was republished: count %d -> %d", firstCount, publishCount)
	}

	// A newly received frame is publishable even when its JPEG bytes are identical.
	c.setLatestFrame(frame)
	if err := c.publishLatestSnapshot("continuous"); err != nil {
		t.Fatal(err)
	}
	if publishCount != firstCount*2 {
		t.Fatalf("new frame was not published: got %d publishes, want %d", publishCount, firstCount*2)
	}
}

func TestCameraStatusIsRetainedAndIncludesConnectionState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	type publication struct {
		topic   string
		payload []byte
		retain  bool
		qos     byte
	}
	var got publication
	pubFn := func(topic string, payload []byte, retain bool, qos byte) error {
		got = publication{topic: topic, payload: append([]byte(nil), payload...), retain: retain, qos: qos}
		return nil
	}
	cfg := DefaultConfig()
	cfg.URL = "rtsp://127.0.0.1/cam"
	cfg.TopicPrefix = "cameras/gate/"
	cfg.QoS = 1
	c := NewConnector("gate", "edge-1", cfg, pubFn, nil, logger)

	c.setStatus(false, "connection refused")
	if got.topic != "cameras/gate/status" || !got.retain || got.qos != 1 {
		t.Fatalf("unexpected status publication: %+v", got)
	}
	var status CameraStatus
	if err := json.Unmarshal(got.payload, &status); err != nil {
		t.Fatal(err)
	}
	if status.Camera != "gate" || status.NodeID != "edge-1" || status.Connected || status.LastError != "connection refused" || status.Timestamp == "" {
		t.Fatalf("unexpected disconnected status: %+v", status)
	}

	c.setStatus(true, "")
	status = CameraStatus{}
	if err := json.Unmarshal(got.payload, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Connected || status.LastError != "" {
		t.Fatalf("unexpected connected status: %+v", status)
	}
}

func TestSlotPointersAndTopics(t *testing.T) {
	slots := 5
	prefix := "cameras/gate"
	for s := 1; s <= slots; s++ {
		picTopic := fmt.Sprintf("%s/capture/frames/%d", prefix, s)
		metaTopic := fmt.Sprintf("%s/capture/frames/%d/meta", prefix, s)
		if picTopic != fmt.Sprintf("cameras/gate/capture/frames/%d", s) {
			t.Errorf("unexpected picTopic %s", picTopic)
		}
		if metaTopic != fmt.Sprintf("cameras/gate/capture/frames/%d/meta", s) {
			t.Errorf("unexpected metaTopic %s", metaTopic)
		}
	}
}
