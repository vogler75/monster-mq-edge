package integration

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/pion/rtp"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/pkg/h264"
)

type h264RTSPSource struct {
	stream  *gortsplib.ServerStream
	closed  chan struct{}
	drop    chan struct{}
	packets chan []*rtp.Packet
}

func (s *h264RTSPSource) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}
func (s *h264RTSPSource) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}
func (s *h264RTSPSource) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}
func (s *h264RTSPSource) OnSessionClose(*gortsplib.ServerHandlerOnSessionCloseCtx) {
	select {
	case s.closed <- struct{}{}:
	default:
	}
}

func TestRTSPH264CameraPublishesSnapshots(t *testing.T) {
	for index, tc := range []struct {
		name, transport string
		fixture         string
		width, height   int
		inband          bool
	}{
		{"TCP SDP parameters", "TCP", "motion-baseline", 96, 64, false},
		{"TCP in-band STAP parameters", "TCP", "motion-baseline", 96, 64, true},
		{"UDP fragmented stream", "UDP", "motion-baseline", 96, 64, false},
		{"TCP CABAC Main", "TCP", "motion-cabac", 128, 96, false},
		{"TCP CABAC High", "TCP", "motion-high-cabac", 128, 96, false},
		{"TCP B-pyramid High", "TCP", "motion-b-pyramid", 128, 96, false},
		{"UDP B frames", "UDP", "motion-b-cavlc", 128, 96, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, source := startH264RTSPSource(t, tc.fixture, tc.inband, tc.transport == "UDP")
			mqttPort, gqlPort := 23211+index, 28211+index
			srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) { c.Features.RtspCamera = true })
			defer srv.Close()
			client := mqtt.NewClient(mqttOpts(mqttPort, fmt.Sprintf("rtsp-h264-%d", index)))
			if tok := client.Connect(); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
				t.Fatalf("connect: %v", tok.Error())
			}
			defer client.Disconnect(100)
			pictures := make(chan []byte, 32)
			streamErrors := make(chan string, 16)
			if tok := client.Subscribe("camera/h264/#", 0, func(_ mqtt.Client, m mqtt.Message) {
				if m.Topic() == "camera/h264/status" {
					var status struct {
						LastError string `json:"lastError"`
					}
					if json.Unmarshal(m.Payload(), &status) == nil && status.LastError != "" {
						select {
						case streamErrors <- status.LastError:
						default:
						}
					}
					return
				}
				if !strings.HasPrefix(m.Topic(), "camera/h264/capture/frames/") || strings.HasSuffix(m.Topic(), "/meta") {
					return
				}
				select {
				case pictures <- append([]byte(nil), m.Payload()...):
				default:
				}
			}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
				t.Fatalf("subscribe: %v", tok.Error())
			}
			result := gqlQuery(t, gqlURL, `mutation Create($input: RtspCameraInput!) {rtspCamera {create(input:$input) {success errors}}}`, map[string]any{"input": map[string]any{
				"name": "h264_cam", "nodeId": fmt.Sprintf("g-%d", gqlPort), "enabled": true, "config": map[string]any{
					"url": url, "transport": tc.transport, "topicPrefix": "camera/h264", "mode": "CONTINUOUS", "intervalMs": 50, "slots": 2,
				},
			}})
			created := result["rtspCamera"].(map[string]any)["create"].(map[string]any)
			if created["success"] != true {
				t.Fatalf("create: %v", created)
			}
			unique := map[string]bool{}
			timer := time.NewTimer(6 * time.Second)
			defer timer.Stop()
			for len(unique) < 3 {
				select {
				case data := <-pictures:
					img, err := jpeg.Decode(bytes.NewReader(data))
					if err != nil {
						t.Fatalf("invalid JPEG: %v", err)
					}
					if img.Bounds().Dx() != tc.width || img.Bounds().Dy() != tc.height {
						t.Fatalf("unexpected picture size: %v", img.Bounds())
					}
					unique[string(data)] = true
				case <-timer.C:
					t.Fatalf("only %d distinct snapshots received", len(unique))
				}
			}
			if index == 0 {
				for len(streamErrors) > 0 {
					<-streamErrors
				}
				source.drop <- struct{}{}
				select {
				case msg := <-streamErrors:
					if !strings.Contains(msg, "lost") {
						t.Fatalf("unexpected loss status: %s", msg)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("packet loss was not reported over MQTT")
				}
				draining := true
				for draining {
					select {
					case <-pictures:
					default:
						draining = false
					}
				}
				select {
				case data := <-pictures:
					if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("camera did not recover at the next IDR")
				}
			}
			// A real session close proves the streaming worker releases the RTSP client.
			srv.Close()
			select {
			case <-source.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("RTSP camera session survived broker shutdown")
			}
		})
	}
}

func startH264RTSPSource(t *testing.T, fixture string, inband, udp bool) (string, *h264RTSPSource) {
	t.Helper()
	return startH264RTSPSourceWithPlayback(t, fixture, inband, udp, true)
}

func startH264RTSPSourceWithPlayback(t *testing.T, fixture string, inband, udp, automatic bool) (string, *h264RTSPSource) {
	t.Helper()
	raw, err := os.ReadFile("testdata/h264/" + fixture + ".264")
	if err != nil {
		t.Fatal(err)
	}
	nals, err := h264.SplitAnnexB(raw)
	if err != nil {
		t.Fatal(err)
	}
	var sps, pps []byte
	var units [][][]byte
	var unit [][]byte
	for _, nal := range nals {
		typ := nal[0] & 31
		if typ == 7 {
			sps = nal
		}
		if typ == 8 {
			pps = nal
		}
		if typ == 9 && len(unit) > 0 {
			units = append(units, unit)
			unit = nil
		}
		if typ == 7 || typ == 8 {
			continue
		}
		unit = append(unit, nal)
	}
	if len(unit) > 0 {
		units = append(units, unit)
	}
	f := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	if !inband {
		f.SPS, f.PPS = sps, pps
	}
	media := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{f}}
	source := &h264RTSPSource{closed: make(chan struct{}, 4), drop: make(chan struct{}, 1), packets: make(chan []*rtp.Packet)}
	var listener net.Listener
	server := &gortsplib.Server{RTSPAddress: "127.0.0.1:0", Handler: source, Listen: func(network, address string) (net.Listener, error) {
		var e error
		listener, e = net.Listen(network, address)
		return listener, e
	}}
	if udp {
		server.UDPRTPAddress = "127.0.0.1:24220"
		server.UDPRTCPAddress = "127.0.0.1:24221"
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	source.stream = &gortsplib.ServerStream{Server: server, Desc: &description.Session{Medias: []*description.Media{media}}}
	if err := source.stream.Initialize(); err != nil {
		server.Close()
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		var ticks <-chan time.Time
		if automatic {
			ticks = ticker.C
		}
		seq := uint16(65530)
		ts := uint32(0xffffff00)
		index := 0
		for {
			select {
			case <-stop:
				return
			case packets := <-source.packets:
				for _, pkt := range packets {
					if err := source.stream.WritePacketRTP(media, pkt); err != nil {
						return
					}
				}
				continue
			case <-ticks:
			}
			au := units[index]
			if inband {
				var stap []byte
				stap = append(stap, 0x78)
				for _, p := range [][]byte{sps, pps} {
					stap = binary.BigEndian.AppendUint16(stap, uint16(len(p)))
					stap = append(stap, p...)
				}
				au = append([][]byte{stap}, au...)
			}
			drop := false
			select {
			case <-source.drop:
				drop = true
			default:
			}
			packets := packetizeH264(au, &seq, ts)
			for i, p := range packets {
				if drop && i == len(packets)-2 {
					continue
				}
				if err := source.stream.WritePacketRTP(media, p); err != nil {
					return
				}
			}
			ts += 3600
			index = (index + 1) % len(units)
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait(); source.stream.Close(); server.Close() })
	return "rtsp://" + listener.Addr().String() + "/camera", source
}

func TestRTSPH264TriggeredSnapshots(t *testing.T) {
	url, _ := startH264RTSPSource(t, "motion-b-pyramid", false, false)
	const mqttPort, gqlPort = 23219, 28219
	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) { c.Features.RtspCamera = true })
	defer srv.Close()
	client := mqtt.NewClient(mqttOpts(mqttPort, "rtsp-h264-triggered"))
	if tok := client.Connect(); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("connect: %v", tok.Error())
	}
	defer client.Disconnect(100)
	pictures := make(chan []byte, 32)
	if tok := client.Subscribe("camera/h264-trigger/capture/frames/+", 0, func(_ mqtt.Client, m mqtt.Message) {
		select {
		case pictures <- append([]byte(nil), m.Payload()...):
		default:
		}
	}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe: %v", tok.Error())
	}
	result := gqlQuery(t, gqlURL, `mutation Create($input: RtspCameraInput!) {rtspCamera {create(input:$input) {success errors}}}`, map[string]any{"input": map[string]any{
		"name": "h264_trigger", "nodeId": fmt.Sprintf("g-%d", gqlPort), "enabled": true, "config": map[string]any{
			"url": url, "topicPrefix": "camera/h264-trigger", "mode": "TRIGGERED", "intervalMs": 50,
		},
	}})
	if created := result["rtspCamera"].(map[string]any)["create"].(map[string]any); created["success"] != true {
		t.Fatalf("create: %v", created)
	}
	metrics := func() map[string]any {
		data := gqlQuery(t, gqlURL, `{rtspCamera(name:"h264_trigger") {metrics {framesReceived snapshotsPublished lastError}}}`, nil)
		all := data["rtspCamera"].(map[string]any)["metrics"].([]any)
		if len(all) != 1 {
			t.Fatalf("expected local camera metrics: %v", all)
		}
		return all[0].(map[string]any)
	}
	waitFrames := func(target float64) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			m := metrics()
			starting := m["framesReceived"].(float64) == 0
			if err, _ := m["lastError"].(string); err != "" && !(starting && err == h264.ErrNeedIDR.Error()) {
				t.Fatalf("stream decode: %s", err)
			}
			if m["framesReceived"].(float64) >= target {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("camera did not decode %.0f frames", target)
	}
	// Reference decoding must keep advancing even without a snapshot consumer.
	waitFrames(15)
	if n := metrics()["snapshotsPublished"].(float64); n != 0 {
		t.Fatalf("triggered camera published %v snapshots without a request", n)
	}
	for i := 0; i < 3; i++ {
		waitFrames(metrics()["framesReceived"].(float64) + 3)
		if i == 1 {
			if tok := client.Publish("camera/h264-trigger/trigger", 0, false, "capture"); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
				t.Fatalf("trigger publish: %v", tok.Error())
			}
		} else {
			result := gqlQuery(t, gqlURL, `mutation {rtspCamera {triggerSnapshot(name:"h264_trigger") {success errors}}}`, nil)
			if trigger := result["rtspCamera"].(map[string]any)["triggerSnapshot"].(map[string]any); trigger["success"] != true {
				t.Fatalf("manual trigger: %v", trigger)
			}
		}
		select {
		case data := <-pictures:
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != 128 || img.Bounds().Dy() != 96 {
				t.Fatalf("unexpected snapshot size: %v", img.Bounds())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("snapshot request produced no JPEG")
		}
	}
	if n := metrics()["snapshotsPublished"].(float64); n != 3 {
		t.Fatalf("expected exactly three requested snapshots, got %v", n)
	}
}

// Independent RFC 6184 packet generation: deliberately small FU-A fragments
// exercise reassembly and sequence/timestamp wraparound over the real network.
func packetizeH264(nals [][]byte, seq *uint16, ts uint32) []*rtp.Packet {
	var packets []*rtp.Packet
	add := func(payload []byte, last bool) {
		packets = append(packets, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: *seq, Timestamp: ts, SSRC: 0x12345678, Marker: last}, Payload: payload})
		*seq++
	}
	for i, nal := range nals {
		if len(nal) <= 200 || nal[0]&31 == 24 {
			add(nal, i == len(nals)-1)
			continue
		}
		for pos := 1; pos < len(nal); {
			end := min(pos+198, len(nal))
			flags := nal[0] & 31
			if pos == 1 {
				flags |= 0x80
			}
			if end == len(nal) {
				flags |= 0x40
			}
			payload := append([]byte{nal[0]&0xe0 | 28, flags}, nal[pos:end]...)
			add(payload, i == len(nals)-1 && end == len(nal))
			pos = end
		}
	}
	return packets
}

func TestH264RTPPacketLossRecovery(t *testing.T) {
	raw, err := os.ReadFile("testdata/h264/intra-baseline.264")
	if err != nil {
		t.Fatal(err)
	}
	nals, err := h264.SplitAnnexB(raw)
	if err != nil {
		t.Fatal(err)
	}
	var dep h264.Depacketizer
	dec := h264.NewDecoder(h264.Config{})
	seq := uint16(65530)
	for iteration := 0; iteration < 3; iteration++ {
		packets := packetizeH264(nals, &seq, uint32(iteration*9000))
		complete, sawLoss := false, false
		for i, p := range packets {
			if iteration == 1 && i == len(packets)-2 {
				continue
			}
			au, err := dep.Push(h264.RTPPacket{SequenceNumber: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker, Payload: p.Payload})
			if err != nil {
				sawLoss = true
				dec.Discontinuity()
				continue
			}
			if len(au) > 0 {
				frames, err := dec.Decode(au)
				if err != nil {
					t.Fatal(err)
				}
				complete = len(frames) == 1
			}
		}
		if iteration == 1 {
			if complete || !sawLoss {
				t.Fatal("damaged RTP picture was not discarded")
			}
		} else if !complete {
			t.Fatalf("IDR %d did not decode", iteration)
		}
	}
}

func TestH264RejectsMalformedInput(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, {0}, {0x80, 0}, {0x67}, {0x67, 0, 0, 3}, {0x65, 0, 0, 0}, {0x65, 0xff, 0xff, 0xff, 0xff, 0xff}} {
		d := h264.NewDecoder(h264.Config{MaxPixels: 65536, MaxAccessUnitBytes: 65536})
		if _, err := d.Decode([][]byte{raw}); err == nil {
			t.Fatalf("accepted malformed NAL %x", raw)
		}
	}
	for _, payload := range [][]byte{{}, {0x78}, {0x78, 0, 20, 1}, {0x78, 0, 1, 24}, {0x7c, 0xc5, 1}, {0x7c, 0x85}, {0x7c, 0x45, 1}, {0xff}} {
		var dep h264.Depacketizer
		if _, err := dep.Push(h264.RTPPacket{Payload: payload, Marker: true}); err == nil {
			t.Fatalf("accepted malformed RTP payload %x", payload)
		}
	}
}

func FuzzH264Decoder(f *testing.F) {
	for _, name := range []string{"intra-baseline", "motion-baseline", "intra-cabac", "motion-cabac", "intra-high-cavlc", "motion-high-cabac", "motion-b-cavlc", "motion-b-pyramid"} {
		raw, err := os.ReadFile("testdata/h264/" + name + ".264")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 65536 {
			t.Skip()
		}
		dec := h264.NewDecoder(h264.Config{MaxPixels: 65536, MaxAccessUnitBytes: 65536})
		if nals, err := h264.SplitAnnexB(raw); err == nil {
			var unit [][]byte
			for _, nal := range nals {
				if nal[0]&31 == 9 && len(unit) > 0 {
					_, _ = dec.Decode(unit)
					unit = nil
				}
				unit = append(unit, nal)
			}
			_, _ = dec.Decode(unit)
			dec.Flush()
		} else {
			_, _ = dec.Decode([][]byte{raw})
		}
		var dep h264.Depacketizer
		for i, p := range strings.Split(string(raw), "\x00\x00\x01") {
			_, _ = dep.Push(h264.RTPPacket{SequenceNumber: uint16(i), Timestamp: uint32(i / 5), Marker: i%5 == 4, Payload: []byte(p)})
		}
	})
}
