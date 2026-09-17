// h264capture tests the native decoder against a real RTSP camera. The existing
// RTSP client handles the session; pkg/h264 handles RTP payloads and pixels.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"monstermq.io/edge/pkg/h264"
)

type options struct {
	url, out, transport string
	count               int
	timeout             time.Duration
	record              bool
}
type decoded struct {
	frame   *h264.Frame
	elapsed time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "", "RTSP URL of an H.264 camera")
	flag.StringVar(&o.out, "out", "", "output directory (default: a new temporary directory)")
	flag.StringVar(&o.transport, "transport", "tcp", "RTSP transport: tcp or udp")
	flag.IntVar(&o.count, "frames", 5, "number of consecutive decoded snapshots to save")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Second, "total connection and capture deadline")
	flag.BoolVar(&o.record, "record", false, "also save stream.264 and decoded.yuv for independent pixel comparison")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := capture(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "h264capture:", err)
		os.Exit(1)
	}
}

func capture(parent context.Context, o options) error {
	if o.url == "" || o.count < 1 || o.timeout <= 0 {
		return errors.New("provide -url, a positive -frames count and a positive -timeout")
	}
	proto := gortsplib.ProtocolTCP
	switch strings.ToLower(o.transport) {
	case "tcp":
	case "udp":
		proto = gortsplib.ProtocolUDP
	default:
		return errors.New("-transport must be tcp or udp")
	}
	u, err := base.ParseURL(o.url)
	if err != nil {
		return fmt.Errorf("parse RTSP URL: %w", err)
	}
	if o.out == "" {
		o.out, err = os.MkdirTemp("", "h264-capture-")
		if err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(o.out, 0755); err != nil {
			return err
		}
	}
	var stream, pixels *os.File
	if o.record {
		stream, err = os.OpenFile(filepath.Join(o.out, "stream.264"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer stream.Close()
		pixels, err = os.OpenFile(filepath.Join(o.out, "decoded.yuv"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer pixels.Close()
	}
	ctx, cancel := context.WithTimeout(parent, o.timeout)
	defer cancel()
	client := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &proto, ReadTimeout: min(o.timeout, 10*time.Second), WriteTimeout: min(o.timeout, 10*time.Second)}
	if err := client.Start(); err != nil {
		return err
	}
	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); <-ctx.Done(); client.Close() }()
	defer func() { cancel(); <-watcherDone }()
	desc, _, err := client.Describe(u)
	if err != nil {
		return fmt.Errorf("DESCRIBE: %w", err)
	}
	var track *format.H264
	media := desc.FindFormat(&track)
	if media == nil {
		return errors.New("camera did not advertise an H.264 video track")
	}
	if track.PacketizationMode > 1 {
		return fmt.Errorf("unsupported H.264 packetization mode %d", track.PacketizationMode)
	}
	fmt.Printf("H.264 track found; packetization=%d, transport=%s\n", track.PacketizationMode, strings.ToUpper(o.transport))
	dec := h264.NewDecoder(h264.Config{})
	sps, pps := track.SafeParams()
	var params [][]byte
	for _, p := range [][]byte{sps, pps} {
		if len(p) > 0 {
			params = append(params, p)
		}
	}
	if _, err := dec.Decode(params); err != nil {
		return fmt.Errorf("SDP parameters: %w", err)
	}
	if _, err := client.Setup(desc.BaseURL, media, 0, 0); err != nil {
		return fmt.Errorf("SETUP: %w", err)
	}
	frames := make(chan decoded, 1)
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	dep := &h264.Depacketizer{}
	started, waiting, haveSSRC := false, false, false
	var ssrc uint32
	delivered := 0
	client.OnPacketRTP(media, track, func(pkt *rtp.Packet) {
		if ctx.Err() != nil || delivered >= o.count {
			return
		}
		if haveSSRC && ssrc != pkt.SSRC {
			fail(errors.New("camera changed RTP source during capture"))
			return
		}
		haveSSRC, ssrc = true, pkt.SSRC
		au, err := dep.Push(h264.RTPPacket{SequenceNumber: pkt.SequenceNumber, Timestamp: pkt.Timestamp, Marker: pkt.Marker, Payload: pkt.Payload})
		if err != nil {
			fail(err)
			return
		}
		if len(au) == 0 {
			return
		}
		idr := false
		for _, n := range au {
			if n[0]&31 == 5 {
				idr = true
			}
		}
		if stream != nil && (started || idr) {
			if !started {
				if err := writeNALs(stream, params); err != nil {
					fail(err)
					return
				}
			}
			if err := writeNALs(stream, au); err != nil {
				fail(err)
				return
			}
			started = true
		}
		begin := time.Now()
		pictures, err := dec.Decode(au)
		elapsed := time.Since(begin)
		if errors.Is(err, h264.ErrNeedIDR) && delivered == 0 {
			if !waiting {
				fmt.Fprintln(os.Stderr, "Waiting for the camera's next IDR picture...")
				waiting = true
			}
			return
		}
		if err != nil {
			fail(err)
			return
		}
		for _, f := range pictures {
			if delivered >= o.count {
				break
			}
			select {
			case frames <- decoded{f, elapsed}:
				delivered++
			case <-ctx.Done():
				return
			}
		}
	})
	if _, err := client.Play(nil); err != nil {
		return fmt.Errorf("PLAY: %w", err)
	}
	ended := make(chan error, 1)
	go func() { ended <- client.Wait() }()
	var total time.Duration
	for i := 0; i < o.count; i++ {
		select {
		case result := <-frames:
			f := result.frame
			path := filepath.Join(o.out, fmt.Sprintf("frame-%04d.jpg", i+1))
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			err = f.WriteJPEG(file, 85)
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if pixels != nil {
				if err := writePixels(pixels, f.Pixels); err != nil {
					return err
				}
			}
			total += result.elapsed
			fmt.Printf("%s: %dx%d, IDR=%v, frame_num=%d, POC=%d, access-unit decode=%s\n", path, f.Pixels.Rect.Dx(), f.Pixels.Rect.Dy(), f.KeyFrame, f.FrameNum, f.PictureOrderCount, result.elapsed.Round(time.Microsecond))
		case err := <-failures:
			return fmt.Errorf("decode: %w", err)
		case err := <-ended:
			if err == nil {
				err = io.EOF
			}
			return fmt.Errorf("RTSP ended: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("captured %d/%d pictures: %w", i, o.count, ctx.Err())
		}
	}
	fmt.Printf("Saved %d snapshots. Mean access-unit decode time for returned batches: %s\n", o.count, (total / time.Duration(o.count)).Round(time.Microsecond))
	return nil
}
func writeNALs(w io.Writer, nals [][]byte) error {
	for _, n := range nals {
		if _, err := w.Write([]byte{0, 0, 0, 1}); err != nil {
			return err
		}
		if _, err := w.Write(n); err != nil {
			return err
		}
	}
	return nil
}
func writePixels(w io.Writer, img *image.YCbCr) error {
	for plane, data := range [][]byte{img.Y, img.Cb, img.Cr} {
		width, height, stride := img.Rect.Dx(), img.Rect.Dy(), img.YStride
		if plane != 0 {
			width, height, stride = (width+1)/2, (height+1)/2, img.CStride
		}
		for y := 0; y < height; y++ {
			if _, err := w.Write(data[y*stride : y*stride+width]); err != nil {
				return err
			}
		}
	}
	return nil
}
