package integration

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/pkg/h264"
)

func TestRTSPH264KeyframesOnly(t *testing.T) {
	for index, transport := range []string{"TCP", "UDP"} {
		t.Run(transport, func(t *testing.T) {
			url, source := startH264RTSPSourceWithPlayback(t, "motion-b-pyramid", true, transport == "UDP", false)
			mqttPort, gqlPort := 23230+index, 28230+index
			srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
				c.Features.RtspCamera = true
			})
			defer srv.Close()
			client := mqtt.NewClient(mqttOpts(mqttPort, fmt.Sprintf("h264-keyframes-%d", index)))
			if tok := client.Connect(); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
				t.Fatalf("connect: %v", tok.Error())
			}
			defer client.Disconnect(100)
			pictures := make(chan []byte, 8)
			if tok := client.Subscribe("camera/keyframes/capture/frames/+", 0, func(_ mqtt.Client, m mqtt.Message) {
				select {
				case pictures <- append([]byte(nil), m.Payload()...):
				default:
				}
			}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
				t.Fatalf("subscribe: %v", tok.Error())
			}
			created := gqlQuery(t, gqlURL, `mutation Create($input: RtspCameraInput!) {rtspCamera {create(input:$input) {success errors}}}`, map[string]any{"input": map[string]any{
				"name": "keyframes", "nodeId": fmt.Sprintf("g-%d", gqlPort), "enabled": true, "config": map[string]any{
					"url": url, "transport": transport, "topicPrefix": "camera/keyframes", "mode": "TRIGGERED", "intervalMs": 1000, "h264DecodeMode": "KEYFRAMES_ONLY",
				},
			}})
			if result := created["rtspCamera"].(map[string]any)["create"].(map[string]any); result["success"] != true {
				t.Fatalf("create: %v", result)
			}
			metrics := func() map[string]any {
				result := gqlQuery(t, gqlURL, `{rtspCamera(name:"keyframes") {metrics {connected framesReceived lastError}}}`, nil)
				all := result["rtspCamera"].(map[string]any)["metrics"].([]any)
				if len(all) != 1 {
					t.Fatalf("expected local metrics: %v", all)
				}
				return all[0].(map[string]any)
			}
			wait := func(want func(map[string]any) bool) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if want(metrics()) {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatalf("camera did not reach expected state: %v", metrics())
			}
			wait(func(m map[string]any) bool { return m["connected"] == true })
			waitFrames := func(n float64) {
				t.Helper()
				wait(func(m map[string]any) bool { return m["framesReceived"].(float64) == n && m["lastError"] == nil })
			}
			trigger := func() bool {
				result := gqlQuery(t, gqlURL, `mutation {rtspCamera {triggerSnapshot(name:"keyframes") {success errors}}}`, nil)
				return result["rtspCamera"].(map[string]any)["triggerSnapshot"].(map[string]any)["success"] == true
			}
			snapshot := func(want []byte) {
				t.Helper()
				if !trigger() {
					t.Fatal("snapshot failed")
				}
				select {
				case got := <-pictures:
					if !bytes.Equal(got, want) {
						t.Fatal("snapshot differs from independently selected IDR picture")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("snapshot not published")
				}
			}
			seq, timestamp := uint16(100), uint32(0)
			send := func(nals [][]byte, drop bool) {
				t.Helper()
				timestamp += 3600
				packets := packetizeH264(nals, &seq, timestamp)
				if drop {
					packets = append(packets[:len(packets)-2], packets[len(packets)-1])
				}
				select {
				case source.packets <- packets:
				case <-time.After(time.Second):
					t.Fatal("RTSP source stalled")
				}
			}
			paramsA, idrA, jpegA := h264IDRFixture(t, "motion-b-pyramid")
			paramsB, idrB, jpegB := h264IDRFixture(t, "intra-crop")
			first := time.Now()
			send(append(paramsA, idrA...), false)
			waitFrames(1)
			// B-frame reorder depth must not hold the only decoded picture.
			snapshot(jpegA)
			// This deliberately invalid non-IDR slice must not be decoded, but
			// its updated parameter sets are needed by a subsequent IDR.
			send(append(paramsB, []byte{0x41, 0x80}), false)
			send(idrB, false)
			time.Sleep(100 * time.Millisecond)
			if m := metrics(); m["framesReceived"].(float64) != 1 || m["lastError"] != nil {
				t.Fatalf("intervening picture decoded or throttled IDR accepted: %v", m)
			}
			if delay := time.Until(first.Add(1200 * time.Millisecond)); delay > 0 {
				time.Sleep(delay)
			}
			send(idrB, false)
			waitFrames(2)
			snapshot(jpegB)
			// Loss clears the cached picture and permits immediate recovery,
			// even though the previous IDR was decoded less than an interval ago.
			send(idrB, true)
			// Advance the UDP reorder window so the missing fragment is
			// reported even though this source has no automatic playback.
			for i := 0; i < 128; i++ {
				send([][]byte{{0x41, 0x80}}, false)
			}
			wait(func(m map[string]any) bool { e, _ := m["lastError"].(string); return strings.Contains(e, "lost") })
			if trigger() {
				t.Fatal("damaged stream left a publishable cached picture")
			}
			send(idrB, false)
			waitFrames(3)
			snapshot(jpegB)
		})
	}
}

func TestRTSPH264DecodeModePerCamera(t *testing.T) {
	url, _ := startH264RTSPSource(t, "motion-b-pyramid", false, false)
	srv, gqlURL := startWithGraphQL(t, 23232, 28232, func(c *config.Config) { c.Features.RtspCamera = true })
	defer srv.Close()
	inputs := map[string]map[string]any{}
	for _, name := range []string{"full", "keys"} {
		cameraConfig := map[string]any{"url": url, "topicPrefix": "camera/" + name, "mode": "CONTINUOUS", "intervalMs": 50}
		want := "FULL"
		if name == "keys" {
			want = "KEYFRAMES_ONLY"
			cameraConfig["h264DecodeMode"] = want
		}
		input := map[string]any{"name": name, "nodeId": "g-28232", "enabled": true, "config": cameraConfig}
		inputs[name] = input
		result := gqlQuery(t, gqlURL, `mutation Create($input: RtspCameraInput!) {rtspCamera {create(input:$input) {success errors camera {config {h264DecodeMode}}}}}`, map[string]any{"input": input})
		created := result["rtspCamera"].(map[string]any)["create"].(map[string]any)
		if created["success"] != true {
			t.Fatalf("create: %v", created)
		}
		if got := created["camera"].(map[string]any)["config"].(map[string]any)["h264DecodeMode"]; got != want {
			t.Fatalf("camera %s mode=%v, want %s", name, got, want)
		}
	}
	counts := func() map[string]float64 {
		result := gqlQuery(t, gqlURL, `{rtspCameras {name config {h264DecodeMode} metrics {framesReceived}}}`, nil)
		out := map[string]float64{}
		for _, item := range result["rtspCameras"].([]any) {
			camera := item.(map[string]any)
			out[camera["name"].(string)] = camera["metrics"].([]any)[0].(map[string]any)["framesReceived"].(float64)
		}
		return out
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		n := counts()
		if n["full"] >= 20 && n["keys"] >= 1 {
			if n["full"] < n["keys"]*5 {
				t.Fatalf("decode mode did not isolate the two cameras: %v", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cameras did not decode: %v", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	input := inputs["keys"]
	input["config"].(map[string]any)["h264DecodeMode"] = "FULL"
	result := gqlQuery(t, gqlURL, `mutation Update($input: RtspCameraInput!) {rtspCamera {update(name:"keys", input:$input) {success errors camera {config {h264DecodeMode}}}}}`, map[string]any{"input": input})
	updated := result["rtspCamera"].(map[string]any)["update"].(map[string]any)
	if updated["success"] != true || updated["camera"].(map[string]any)["config"].(map[string]any)["h264DecodeMode"] != "FULL" {
		t.Fatalf("update: %v", updated)
	}
	deadline = time.Now().Add(3 * time.Second)
	for counts()["keys"] < 10 {
		if time.Now().After(deadline) {
			t.Fatal("saving FULL did not restart this camera with full decoding")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func h264IDRFixture(t *testing.T, name string) (params, idr [][]byte, jpeg []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/h264/" + name + ".264")
	if err != nil {
		t.Fatal(err)
	}
	nals, err := h264.SplitAnnexB(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, nal := range nals {
		if nal[0]&31 == 9 && len(idr) > 0 {
			break
		}
		switch nal[0] & 31 {
		case 7, 8:
			params = append(params, nal)
		case 5:
			idr = append(idr, nal)
		}
	}
	dec := h264.NewDecoder(h264.Config{})
	frames, err := dec.Decode(append(params, idr...))
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, dec.Flush()...)
	if len(frames) != 1 || !frames[0].KeyFrame {
		t.Fatal("fixture has no single IDR picture")
	}
	var buf bytes.Buffer
	if err := frames[0].WriteJPEG(&buf, 85); err != nil {
		t.Fatal(err)
	}
	return params, idr, buf.Bytes()
}
