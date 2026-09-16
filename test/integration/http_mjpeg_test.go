package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
)

func TestHTTPMJPEGCameraPublishesSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		mqttPort    int
		gqlPort     int
	}{
		{"multipart header", "multipart/x-mixed-replace; boundary=frame", 23191, 28191},
		{"FFmpeg octet stream header", "application/octet-stream", 23192, 28192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testHTTPMJPEGCameraPublishesSnapshots(t, tc.contentType, tc.mqttPort, tc.gqlPort)
		})
	}
}

func testHTTPMJPEGCameraPublishesSnapshots(t *testing.T, contentType string, mqttPort, gqlPort int) {
	frame := []byte{0xff, 0xd8, 0x01, 0x02, 0xff, 0xd9}
	stream := newTestMJPEGStream(t, contentType, frame)
	defer stream.Close()

	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) { c.Features.RtspCamera = true })
	defer srv.Close()

	client := mqtt.NewClient(mqttOpts(mqttPort, "http-mjpeg-test"))
	if tok := client.Connect(); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("connect: %v", tok.Error())
	}
	defer client.Disconnect(100)
	type received struct {
		topic string
		data  []byte
	}
	messages := make(chan received, 64)
	if tok := client.Subscribe("camera/http/capture/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		select {
		case messages <- received{m.Topic(), append([]byte(nil), m.Payload()...)}:
		default:
		}
	}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe: %v", tok.Error())
	}

	res := gqlQuery(t, gqlURL, `mutation Create($input: RtspCameraInput!) {
		rtspCamera { create(input: $input) { success errors } }
	}`, map[string]any{"input": map[string]any{
		"name": "http_cam", "nodeId": fmt.Sprintf("g-%d", gqlPort), "enabled": true,
		"config": map[string]any{
			"url": stream.URL, "topicPrefix": "camera/http", "mode": "CONTINUOUS",
			"intervalMs": 50, "slots": 2,
		},
	}})
	created := res["rtspCamera"].(map[string]any)["create"].(map[string]any)
	if created["success"] != true {
		t.Fatalf("camera create failed: %v", created["errors"])
	}

	seen := map[string]bool{}
	deadline := time.After(4 * time.Second)
	for !(seen["1/pic"] && seen["2/pic"] && seen["latest"] && seen["meta"] && seen["latest/pic"] && seen["latest/meta"]) {
		select {
		case msg := <-messages:
			if strings.HasPrefix(msg.topic, "camera/http/capture/snapshot/") {
				t.Fatalf("continuous capture published topic-triggered snapshot %s", msg.topic)
			}
			if strings.HasSuffix(msg.topic, "/pic") {
				if !bytes.Equal(msg.data, frame) {
					t.Fatalf("unexpected JPEG payload on %s: %x", msg.topic, msg.data)
				}
				if strings.Contains(msg.topic, "/1/") {
					seen["1/pic"] = true
				}
				if strings.Contains(msg.topic, "/2/") {
					seen["2/pic"] = true
				}
				if strings.HasSuffix(msg.topic, "/latest/pic") {
					seen["latest/pic"] = true
				}
			}
			if strings.HasSuffix(msg.topic, "/meta") {
				var meta map[string]any
				if err := json.Unmarshal(msg.data, &meta); err != nil || meta["contentType"] != "image/jpeg" {
					t.Fatalf("invalid metadata: %s (%v)", msg.data, err)
				}
				seen["meta"] = true
				if strings.HasSuffix(msg.topic, "/latest/meta") {
					if meta["topic"] != "camera/http/capture/latest/pic" {
						t.Fatalf("latest metadata points to %v", meta["topic"])
					}
					seen["latest/meta"] = true
				}
			}
			if strings.HasSuffix(msg.topic, "/latest") {
				seen["latest"] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for HTTP MJPEG snapshots: %v", seen)
		}
	}
}

func newTestMJPEGStream(t *testing.T, contentType string, frame []byte) *httptest.Server {
	t.Helper()
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		flusher := w.(http.Flusher)
		for {
			if _, err := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame)); err != nil {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if _, err := w.Write([]byte("\r\n")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	return stream
}

func TestMQTTTriggeredCameraPublishesSnapshotPair(t *testing.T) {
	frame := []byte{0xff, 0xd8, 0x12, 0x34, 0xff, 0xd9}
	stream := newTestMJPEGStream(t, "application/octet-stream", frame)
	defer stream.Close()
	srv, gqlURL := startWithGraphQL(t, 23193, 28193, func(c *config.Config) { c.Features.RtspCamera = true })
	defer srv.Close()

	client := mqtt.NewClient(mqttOpts(23193, "http-mjpeg-trigger-test"))
	if tok := client.Connect(); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("connect: %v", tok.Error())
	}
	defer client.Disconnect(100)
	type received struct {
		topic string
		data  []byte
	}
	messages := make(chan received, 16)
	if tok := client.Subscribe("camera/triggered/capture/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		select {
		case messages <- received{m.Topic(), append([]byte(nil), m.Payload()...)}:
		default:
		}
	}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe: %v", tok.Error())
	}

	res := gqlQuery(t, gqlURL, `mutation Create($input: RtspCameraInput!) {
		rtspCamera { create(input: $input) { success errors } }
	}`, map[string]any{"input": map[string]any{
		"name": "trigger_cam", "nodeId": "g-28193", "enabled": true,
		"config": map[string]any{
			"url": stream.URL, "topicPrefix": "camera/triggered", "mode": "TRIGGERED",
			"triggerTopic": "camera/triggered/trigger", "slots": 2,
		},
	}})
	created := res["rtspCamera"].(map[string]any)["create"].(map[string]any)
	if created["success"] != true {
		t.Fatalf("camera create failed: %v", created["errors"])
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		data := gqlQuery(t, gqlURL, `{ rtspCamera(name: "trigger_cam") { metrics { framesReceived } } }`, nil)
		metrics := data["rtspCamera"].(map[string]any)["metrics"].([]any)
		if metrics[0].(map[string]any)["framesReceived"].(float64) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("camera did not receive a source frame")
	}
	if tok := client.Publish("camera/triggered/trigger", 0, false, "snap"); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("publish trigger: %v", tok.Error())
	}

	seenPic, seenMeta := false, false
	wait := time.After(4 * time.Second)
	for !seenPic || !seenMeta {
		select {
		case msg := <-messages:
			switch msg.topic {
			case "camera/triggered/capture/snapshot/pic":
				if !bytes.Equal(msg.data, frame) {
					t.Fatalf("unexpected snapshot picture: %x", msg.data)
				}
				seenPic = true
			case "camera/triggered/capture/snapshot/meta":
				var meta map[string]any
				if err := json.Unmarshal(msg.data, &meta); err != nil {
					t.Fatalf("decode snapshot metadata: %v", err)
				}
				if meta["topic"] != "camera/triggered/capture/snapshot/pic" || meta["trigger"] != "topic_trigger" {
					t.Fatalf("unexpected snapshot metadata: %v", meta)
				}
				seenMeta = true
			}
		case <-wait:
			t.Fatalf("timed out waiting for triggered snapshot pair: pic=%v meta=%v", seenPic, seenMeta)
		}
	}
}
