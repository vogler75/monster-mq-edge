package rtspcamera

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var cameraWebSocketDialer = websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 10 * time.Second,
}

func (c *Connector) connectWebSocketStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, resp, err := cameraWebSocketDialer.DialContext(streamCtx, c.cfg.URL, nil)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("WebSocket MJPEG handshake returned %s: %w", resp.Status, err)
		}
		return fmt.Errorf("WebSocket MJPEG connection failed: %w", err)
	}
	conn.SetReadLimit(maxMJPEGFrameBytes + 2)

	c.mu.Lock()
	select {
	case <-c.stopCh:
		c.mu.Unlock()
		_ = conn.Close()
		return context.Canceled
	default:
		c.wsConn = conn
	}
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		select {
		case <-streamCtx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer func() {
		close(done)
		_ = conn.Close()
		c.mu.Lock()
		if c.wsConn == conn {
			c.wsConn = nil
		}
		c.connected = false
		c.mu.Unlock()
	}()

	c.setStatus(true, "")
	c.logger.Info("WebSocket MJPEG stream connected")
	decoder := jpegMessageDecoder{}
	for {
		messageType, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			if streamCtx.Err() != nil {
				return streamCtx.Err()
			}
			return fmt.Errorf("WebSocket MJPEG stream ended: %w", readErr)
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		frames, decodeErr := decoder.Push(payload)
		if decodeErr != nil {
			return decodeErr
		}
		for _, frame := range frames {
			atomic.AddUint64(&c.framesReceived, 1)
			c.setLatestFrame(frame)
		}
	}
}

// jpegMessageDecoder accepts either one JPEG per binary message or a JPEG byte
// stream split across messages. Bytes outside SOI/EOI markers are ignored.
type jpegMessageDecoder struct {
	buffer []byte
}

func (d *jpegMessageDecoder) Push(payload []byte) ([][]byte, error) {
	d.buffer = append(d.buffer, payload...)
	var frames [][]byte
	for {
		start := bytes.Index(d.buffer, []byte{0xff, 0xd8})
		if start < 0 {
			if len(d.buffer) > 0 && d.buffer[len(d.buffer)-1] == 0xff {
				d.buffer = d.buffer[len(d.buffer)-1:]
			} else {
				d.buffer = d.buffer[:0]
			}
			return frames, nil
		}
		d.buffer = d.buffer[start:]
		end := bytes.Index(d.buffer[2:], []byte{0xff, 0xd9})
		if end < 0 {
			if len(d.buffer) > maxMJPEGFrameBytes {
				return nil, fmt.Errorf("WebSocket MJPEG frame exceeds %d bytes", maxMJPEGFrameBytes)
			}
			return frames, nil
		}
		end += 4
		frame := append([]byte(nil), d.buffer[:end]...)
		frames = append(frames, frame)
		d.buffer = d.buffer[end:]
	}
}
