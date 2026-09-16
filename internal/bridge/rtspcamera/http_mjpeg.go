package rtspcamera

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const maxMJPEGFrameBytes = 16 << 20

func (c *Connector) connectHTTPStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	select {
	case <-c.stopCh:
		c.mu.Unlock()
		cancel()
		return context.Canceled
	default:
		c.httpCancel = cancel
	}
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.httpCancel = nil
		c.connected = false
		c.mu.Unlock()
	}()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, c.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("invalid HTTP MJPEG URL: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Second
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP MJPEG request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP MJPEG request returned %s", resp.Status)
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("invalid HTTP MJPEG content type: %w", err)
	}
	var boundary string
	var body io.Reader = resp.Body
	switch {
	case strings.EqualFold(mediaType, "multipart/x-mixed-replace"):
		boundary = params["boundary"]
		if boundary == "" {
			return fmt.Errorf("HTTP MJPEG response has no multipart boundary")
		}
	case strings.EqualFold(mediaType, "application/octet-stream"):
		// FFmpeg's HTTP mpjpeg muxer defaults to application/octet-stream.
		boundary, body, err = sniffMJPEGBoundary(resp.Body)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("HTTP MJPEG response has unsupported content type %q", mediaType)
	}

	reader := multipart.NewReader(body, boundary)
	c.setStatus(true, "")
	c.logger.Info("HTTP MJPEG stream connected")
	for {
		part, err := reader.NextPart()
		if err != nil {
			return fmt.Errorf("HTTP MJPEG stream ended: %w", err)
		}
		contentType := part.Header.Get("Content-Type")
		if contentType != "" {
			partType, _, parseErr := mime.ParseMediaType(contentType)
			if parseErr != nil || !strings.EqualFold(partType, "image/jpeg") {
				part.Close()
				continue
			}
		}
		frame, readErr := io.ReadAll(io.LimitReader(part, maxMJPEGFrameBytes+1))
		part.Close()
		if readErr != nil {
			return fmt.Errorf("read HTTP MJPEG frame: %w", readErr)
		}
		if len(frame) > maxMJPEGFrameBytes {
			return fmt.Errorf("HTTP MJPEG frame exceeds %d bytes", maxMJPEGFrameBytes)
		}
		if len(frame) < 4 || frame[0] != 0xff || frame[1] != 0xd8 || frame[len(frame)-2] != 0xff || frame[len(frame)-1] != 0xd9 {
			c.logger.Debug("skipping invalid HTTP MJPEG frame")
			continue
		}
		atomic.AddUint64(&c.framesReceived, 1)
		c.setLatestFrame(frame)
	}
}

func sniffMJPEGBoundary(body io.Reader) (string, io.Reader, error) {
	var prefix [256]byte
	for i := range prefix {
		if _, err := io.ReadFull(body, prefix[i:i+1]); err != nil {
			return "", nil, fmt.Errorf("read HTTP MJPEG boundary: %w", err)
		}
		if prefix[i] != '\n' {
			continue
		}
		line := strings.TrimSuffix(strings.TrimSuffix(string(prefix[:i+1]), "\n"), "\r")
		if !strings.HasPrefix(line, "--") || len(line) <= 2 || strings.HasSuffix(line, "--") {
			return "", nil, fmt.Errorf("HTTP MJPEG body does not start with a multipart boundary")
		}
		return line[2:], io.MultiReader(bytes.NewReader(prefix[:i+1]), body), nil
	}
	return "", nil, fmt.Errorf("HTTP MJPEG boundary exceeds %d bytes", len(prefix))
}
