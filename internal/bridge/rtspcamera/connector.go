package rtspcamera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"

	"monstermq.io/edge/pkg/h264"
)

// LocalPublisher is the broker's publish function.
type LocalPublisher func(topic string, payload []byte, retain bool, qos byte) error

// LocalMessage is what the bus delivers for subscribed trigger topics.
type LocalMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// LocalSubscriber lets the connector listen to MQTT topics (e.g. trigger topic).
type LocalSubscriber interface {
	Subscribe(filters []string, buffer int) (id int, ch <-chan LocalMessage)
	Unsubscribe(id int)
}

// Connector represents an active RTSP camera connection and snapshot engine.
type Connector struct {
	name      string
	nodeID    string
	cfg       Config
	publisher LocalPublisher
	subBus    LocalSubscriber
	logger    *slog.Logger

	mu                 sync.RWMutex
	publishMu          sync.Mutex
	connected          bool
	framesReceived     uint64
	snapshotsPublished uint64
	currentSlot        uint32
	lastSnapshotAt     time.Time
	lastError          string
	latestFrame        []byte
	latestDecoded      *h264.Frame
	latestFrameVersion uint64
	publishedVersion   uint64

	client     *gortsplib.Client
	httpCancel context.CancelFunc
	wsConn     *websocket.Conn
	stopCh     chan struct{}
	subID      int
}

// NewConnector creates an RTSP camera connector.
func NewConnector(name, nodeID string, cfg Config, publisher LocalPublisher, subBus LocalSubscriber, logger *slog.Logger) *Connector {
	cfg.ApplyDefaults()
	return &Connector{
		name:      name,
		nodeID:    nodeID,
		cfg:       cfg,
		publisher: publisher,
		subBus:    subBus,
		logger:    logger.With("camera", name),
		stopCh:    make(chan struct{}),
	}
}

// Name returns the camera connector name.
func (c *Connector) Name() string { return c.name }

// Config returns the active camera configuration.
func (c *Connector) Config() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// Start launches the background worker goroutine.
func (c *Connector) Start(ctx context.Context) {
	c.logger.Info("starting rtsp camera connector", "url", c.cfg.URL, "mode", c.cfg.Mode, "slots", c.cfg.Slots)
	c.setStatus(false, "")
	go c.run(ctx)
}

// Stop shuts down the RTSP connection, trigger subscriptions, and timers.
func (c *Connector) Stop() {
	c.mu.Lock()
	select {
	case <-c.stopCh:
		c.mu.Unlock()
		return
	default:
		close(c.stopCh)
	}

	if c.subID != 0 && c.subBus != nil {
		c.subBus.Unsubscribe(c.subID)
		c.subID = 0
	}
	client := c.client
	httpCancel := c.httpCancel
	wsConn := c.wsConn
	c.mu.Unlock()

	if client != nil {
		client.Close()
	}
	if httpCancel != nil {
		httpCancel()
	}
	if wsConn != nil {
		_ = wsConn.Close()
	}
	c.setStatus(false, "")
	c.logger.Info("stopped rtsp camera connector")
}

// Metrics returns the latest runtime statistics.
func (c *Connector) Metrics() Metrics {
	c.mu.RLock()
	defer c.mu.RUnlock()

	lastSnapStr := ""
	if !c.lastSnapshotAt.IsZero() {
		lastSnapStr = c.lastSnapshotAt.Format(time.RFC3339)
	}

	currentSlot := 0
	rawSlot := atomic.LoadUint32(&c.currentSlot)
	if rawSlot > 0 && c.cfg.Slots > 0 {
		currentSlot = int((rawSlot-1)%uint32(c.cfg.Slots)) + 1
	}

	return Metrics{
		Connected:          c.connected,
		FramesReceived:     float64(atomic.LoadUint64(&c.framesReceived)),
		SnapshotsPublished: float64(atomic.LoadUint64(&c.snapshotsPublished)),
		CurrentSlot:        currentSlot,
		LastSnapshotAt:     lastSnapStr,
		LastError:          c.lastError,
		Timestamp:          time.Now().UTC().Format(time.RFC3339),
	}
}

// TriggerSnapshot triggers an immediate snapshot from the latest cached frame.
func (c *Connector) TriggerSnapshot() error {
	if !c.hasLatestFrame() {
		return errors.New("no frame received from camera yet")
	}
	return c.publishLatestSnapshot("manual")
}

func (c *Connector) getLatestFrame() []byte {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	frame, _, _ := c.latestFrameWithVersion()
	return frame
}

func (c *Connector) hasLatestFrame() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.latestFrame) > 0 || c.latestDecoded != nil
}

func (c *Connector) setLatestFrame(frame []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latestFrame = append(c.latestFrame[:0], frame...)
	c.latestDecoded = nil
	c.latestFrameVersion++
}

// The decoder owns immutable picture planes, so snapshots can encode the latest
// picture on demand without blocking RTP decoding or retaining a frame queue.
func (c *Connector) setLatestDecoded(frame *h264.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latestDecoded = frame
	c.latestFrame = nil
	c.latestFrameVersion++
}

// Caller holds publishMu to serialize JPEG encoding and publication.
func (c *Connector) latestFrameWithVersion() ([]byte, uint64, error) {
	c.mu.RLock()
	frame, decoded, version := append([]byte(nil), c.latestFrame...), c.latestDecoded, c.latestFrameVersion
	c.mu.RUnlock()
	if len(frame) != 0 || decoded == nil {
		return frame, version, nil
	}
	var buf bytes.Buffer
	if err := decoded.WriteJPEG(&buf, 85); err != nil {
		return nil, version, fmt.Errorf("encode H.264 snapshot: %w", err)
	}
	frame = buf.Bytes()
	c.mu.Lock()
	if c.latestFrameVersion == version && c.latestDecoded == decoded {
		c.latestFrame = append([]byte(nil), frame...)
		c.latestDecoded = nil
	}
	c.mu.Unlock()
	return frame, version, nil
}

func (c *Connector) setStatus(connected bool, errMsg string) {
	c.mu.Lock()
	c.connected = connected
	c.lastError = errMsg
	c.mu.Unlock()
	c.publishStatus()
}

func (c *Connector) publishStatus() {
	if c.publisher == nil {
		return
	}
	c.mu.RLock()
	status := CameraStatus{
		Camera:    c.name,
		NodeID:    c.nodeID,
		Connected: c.connected,
		LastError: c.lastError,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	c.mu.RUnlock()
	payload, err := json.Marshal(status)
	if err != nil {
		c.logger.Warn("failed to encode camera status", "err", err)
		return
	}
	topic := strings.TrimSuffix(c.cfg.TopicPrefix, "/") + "/status"
	c.publishMu.Lock()
	err = c.publisher(topic, payload, true, byte(c.cfg.QoS))
	c.publishMu.Unlock()
	if err != nil {
		c.logger.Warn("failed to publish camera status", "topic", topic, "err", err)
	}
}

func (c *Connector) run(ctx context.Context) {
	// Set up trigger subscription if triggered or both mode.
	var triggerCh <-chan LocalMessage
	if (c.cfg.Mode == ModeTriggered || c.cfg.Mode == ModeBoth) && c.subBus != nil && c.cfg.TriggerTopic != "" {
		subID, ch := c.subBus.Subscribe([]string{c.cfg.TriggerTopic}, 16)
		c.mu.Lock()
		c.subID = subID
		c.mu.Unlock()
		triggerCh = ch
	}

	// Set up continuous capture timer if continuous or both mode.
	var ticker *time.Ticker
	var tickCh <-chan time.Time
	if c.cfg.Mode == ModeContinuous || c.cfg.Mode == ModeBoth {
		interval := time.Duration(c.cfg.IntervalMs) * time.Millisecond
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		tickCh = ticker.C
	}

	// Main processing loop: orchestrates streaming, continuous timer, and topic triggers.
	go c.streamLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-tickCh:
			if c.hasLatestFrame() {
				if err := c.publishLatestSnapshot("continuous"); err != nil {
					c.logger.Warn("failed to publish continuous snapshot", "err", err)
				}
			}
		case _, ok := <-triggerCh:
			if !ok {
				triggerCh = nil
				continue
			}
			if c.hasLatestFrame() {
				if err := c.publishLatestSnapshot("topic_trigger"); err != nil {
					c.logger.Warn("failed to publish triggered snapshot", "err", err)
				}
			} else {
				c.logger.Debug("received snapshot trigger but no frame cached yet")
			}
		}
	}
}

func (c *Connector) streamLoop(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		default:
		}

		parsed, parseErr := url.Parse(c.cfg.URL)
		var err error
		if parseErr != nil {
			err = fmt.Errorf("invalid camera stream URL: %w", parseErr)
		} else {
			switch strings.ToLower(parsed.Scheme) {
			case "http", "https":
				err = c.connectHTTPStream(ctx)
			case "ws", "wss":
				err = c.connectWebSocketStream(ctx)
			default:
				err = c.connectAndStream(ctx)
			}
		}
		if err != nil {
			select {
			case <-ctx.Done():
				c.setStatus(false, "")
				return
			case <-c.stopCh:
				return
			default:
			}
			c.setStatus(false, err.Error())
			c.logger.Warn("camera stream error, reconnecting", "err", err, "backoff", backoff)
		} else {
			backoff = time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}
}

func (c *Connector) connectAndStream(ctx context.Context) error {
	u, err := base.ParseURL(c.cfg.URL)
	if err != nil {
		return fmt.Errorf("invalid rtsp url: %w", err)
	}

	client := &gortsplib.Client{
		Scheme:       u.Scheme,
		Host:         u.Host,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	if strings.ToUpper(c.cfg.Transport) == TransportUDP {
		proto := gortsplib.ProtocolUDP
		client.Protocol = &proto
	} else {
		proto := gortsplib.ProtocolTCP
		client.Protocol = &proto
	}

	c.mu.Lock()
	select {
	case <-c.stopCh:
		c.mu.Unlock()
		return context.Canceled
	default:
	}
	if err := client.Start(); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("rtsp client start failed: %w", err)
	}
	c.client = client
	c.latestFrame = nil
	c.latestDecoded = nil
	c.mu.Unlock()
	watcherStop, watcherDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			client.Close()
		case <-c.stopCh:
			client.Close()
		case <-watcherStop:
		}
	}()
	defer func() {
		close(watcherStop)
		<-watcherDone
		client.Close()
		c.mu.Lock()
		if c.client == client {
			c.client = nil
		}
		c.latestFrame = nil
		c.latestDecoded = nil
		c.mu.Unlock()
	}()

	desc, _, err := client.Describe(u)
	if err != nil {
		return fmt.Errorf("rtsp describe failed: %w", err)
	}

	var jpegFormat *format.MJPEG
	var h264Format *format.H264
	var medi *description.Media
	var selected format.Format
	var handlePacket func(*rtp.Packet)
	decodeErrors := make(chan error, 1)
	if medi = desc.FindFormat(&jpegFormat); medi != nil {
		selected = jpegFormat
		rtpDec, err := jpegFormat.CreateDecoder()
		if err != nil {
			return fmt.Errorf("create mjpeg decoder failed: %w", err)
		}
		handlePacket = func(pkt *rtp.Packet) {
			enc, err := rtpDec.Decode(pkt)
			if err != nil {
				if !errors.Is(err, rtpmjpeg.ErrNonStartingPacketAndNoPrevious) && !errors.Is(err, rtpmjpeg.ErrMorePacketsNeeded) {
					c.logger.Debug("rtp decode packet error", "err", err)
				}
				return
			}
			if len(enc) > 0 {
				atomic.AddUint64(&c.framesReceived, 1)
				c.setLatestFrame(enc)
			}
		}
	} else if medi = desc.FindFormat(&h264Format); medi != nil {
		selected = h264Format
		handlePacket, err = c.h264PacketHandler(h264Format, decodeErrors)
		if err != nil {
			return err
		}
	} else {
		return errors.New("RTSP stream has no supported video track; expected MJPEG or H.264")
	}
	if _, err = client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return fmt.Errorf("rtsp setup failed: %w", err)
	}
	client.OnPacketRTP(medi, selected, handlePacket)

	_, err = client.Play(nil)
	if err != nil {
		return fmt.Errorf("rtsp play failed: %w", err)
	}

	c.setStatus(true, "")
	c.logger.Info("rtsp stream connected and playing", "url", c.cfg.URL)

	waitDone := make(chan error, 1)
	go func() { waitDone <- client.Wait() }()
	select {
	case err := <-decodeErrors:
		client.Close()
		<-waitDone
		return fmt.Errorf("H.264 stream decode failed: %w", err)
	case <-ctx.Done():
		client.Close()
		<-waitDone
		return ctx.Err()
	case <-c.stopCh:
		client.Close()
		<-waitDone
		return context.Canceled
	case err := <-waitDone:
		if err != nil {
			return fmt.Errorf("rtsp connection terminated: %w", err)
		}
		return nil
	}
}

func (c *Connector) publishSnapshot(frame []byte, triggerSource string) error {
	if c.publisher == nil || len(frame) == 0 {
		return nil
	}
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	return c.publishSnapshotLocked(frame, triggerSource)
}

func (c *Connector) publishLatestSnapshot(triggerSource string) error {
	if c.publisher == nil {
		return nil
	}
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	frame, version, err := c.latestFrameWithVersion()
	if err != nil {
		return err
	}
	if len(frame) == 0 {
		return nil
	}
	if version <= c.publishedVersion {
		return nil
	}
	if err := c.publishSnapshotLocked(frame, triggerSource); err != nil {
		return err
	}
	c.publishedVersion = version
	return nil
}

func (c *Connector) publishSnapshotLocked(frame []byte, triggerSource string) error {

	slots := c.cfg.Slots
	if slots <= 0 {
		slots = 1
	}

	// Advance round robin slot (1-indexed: 1..slots).
	next := atomic.AddUint32(&c.currentSlot, 1)
	slot := int((next-1)%uint32(slots)) + 1

	now := time.Now().UTC()
	timestamp := now.Format(time.RFC3339Nano)
	timestampMs := now.UnixMilli()

	prefix := strings.TrimSuffix(c.cfg.TopicPrefix, "/")
	picTopic := fmt.Sprintf("%s/capture/frames/%d", prefix, slot)
	metaTopic := picTopic + "/meta"
	latestTopic := fmt.Sprintf("%s/capture/latest", prefix)
	latestPicTopic := latestTopic + "/pic"
	latestMetaTopic := latestTopic + "/meta"

	// 1. Publish raw JPEG binary to <prefix>/capture/frames/<slot>.
	if err := c.publisher(picTopic, frame, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
		return fmt.Errorf("publish pic topic %s failed: %w", picTopic, err)
	}

	// 2. Publish JSON metadata to <prefix>/capture/frames/<slot>/meta.
	meta := SnapshotMeta{
		Camera:      c.name,
		Slot:        slot,
		Timestamp:   timestamp,
		TimestampMs: timestampMs,
		Bytes:       len(frame),
		ContentType: "image/jpeg",
		Topic:       picTopic,
		Trigger:     triggerSource,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode snapshot metadata: %w", err)
	}
	if err := c.publisher(metaTopic, metaBytes, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
		return fmt.Errorf("publish meta topic %s failed: %w", metaTopic, err)
	}

	// The latest pair is directly consumable without first resolving the slot pointer.
	if err := c.publisher(latestPicTopic, frame, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
		return fmt.Errorf("publish pic topic %s failed: %w", latestPicTopic, err)
	}
	latestMeta := meta
	latestMeta.Topic = latestPicTopic
	latestMetaBytes, err := json.Marshal(latestMeta)
	if err != nil {
		return fmt.Errorf("encode latest snapshot metadata: %w", err)
	}
	if err := c.publisher(latestMetaTopic, latestMetaBytes, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
		return fmt.Errorf("publish meta topic %s failed: %w", latestMetaTopic, err)
	}

	// 3. Publish active pointer to <prefix>/capture/latest.
	if c.cfg.PublishMetadata {
		ptr := LatestPointer{
			Camera:      c.name,
			Slot:        slot,
			PicTopic:    picTopic,
			MetaTopic:   metaTopic,
			Timestamp:   timestamp,
			TimestampMs: timestampMs,
			Bytes:       len(frame),
			Trigger:     triggerSource,
		}
		ptrBytes, err := json.Marshal(ptr)
		if err != nil {
			return fmt.Errorf("encode latest snapshot pointer: %w", err)
		}
		if err := c.publisher(latestTopic, ptrBytes, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
			return fmt.Errorf("publish latest pointer topic %s failed: %w", latestTopic, err)
		}
	}
	if triggerSource == "topic_trigger" {
		snapshotPicTopic := prefix + "/capture/snapshot/pic"
		snapshotMetaTopic := prefix + "/capture/snapshot/meta"
		if err := c.publisher(snapshotPicTopic, frame, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
			return fmt.Errorf("publish pic topic %s failed: %w", snapshotPicTopic, err)
		}
		snapshotMeta := meta
		snapshotMeta.Topic = snapshotPicTopic
		snapshotMetaBytes, err := json.Marshal(snapshotMeta)
		if err != nil {
			return fmt.Errorf("encode triggered snapshot metadata: %w", err)
		}
		if err := c.publisher(snapshotMetaTopic, snapshotMetaBytes, c.cfg.Retain, byte(c.cfg.QoS)); err != nil {
			return fmt.Errorf("publish meta topic %s failed: %w", snapshotMetaTopic, err)
		}
	}

	atomic.AddUint64(&c.snapshotsPublished, 1)
	c.mu.Lock()
	c.lastSnapshotAt = now
	c.mu.Unlock()

	return nil
}
