package rtspcamera

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestJPEGMessageDecoderHandlesSplitAndMultipleFrames(t *testing.T) {
	decoder := jpegMessageDecoder{}
	frame1 := []byte{0xff, 0xd8, 0x01, 0xff, 0xd9}
	frame2 := []byte{0xff, 0xd8, 0x02, 0xff, 0xd9}
	frames, err := decoder.Push(append([]byte{0x00}, frame1[:3]...))
	if err != nil || len(frames) != 0 {
		t.Fatalf("first chunk: frames=%d err=%v", len(frames), err)
	}
	frames, err = decoder.Push(append(frame1[3:], frame2...))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || !bytes.Equal(frames[0], frame1) || !bytes.Equal(frames[1], frame2) {
		t.Fatalf("unexpected decoded frames: %x", frames)
	}
}

func TestWSSMJPEGStreamReceivesFrame(t *testing.T) {
	frame := []byte{0xff, 0xd8, 0x31, 0x32, 0xff, 0xd9}
	upgrader := websocket.Upgrader{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.BinaryMessage, frame[:3])
		_ = conn.WriteMessage(websocket.BinaryMessage, frame[3:])
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	previousDialer := cameraWebSocketDialer
	cameraWebSocketDialer.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() { cameraWebSocketDialer = previousDialer })

	cfg := DefaultConfig()
	cfg.URL = "wss" + strings.TrimPrefix(server.URL, "https")
	connector := NewConnector("wss-test", "node", cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- connector.connectWebSocketStream(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.Equal(connector.getLatestFrame(), frame) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := connector.getLatestFrame(); !bytes.Equal(got, frame) {
		t.Fatalf("unexpected frame: %x", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WebSocket stream did not stop after cancellation")
	}
}
