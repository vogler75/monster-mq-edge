package integration

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/hmi"
)

func TestHmiSync_FullLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	hmiDir := filepath.Join(tempDir, "hmi_data")
	if err := os.MkdirAll(hmiDir, 0755); err != nil {
		t.Fatal(err)
	}

	mqttPort := 22050
	gqlPort := 24050
	srv, _ := startWithHMI(t, mqttPort, gqlPort, hmiDir)
	defer srv.Close()

	// 1. Connect MQTT client with session UUID
	sessionUUID := "test-session-123"
	opts := mqttOpts(mqttPort, "hmi-sync-client")
	cl := paho.NewClient(opts)
	if token := cl.Connect(); token.Wait() && token.Error() != nil {
		t.Fatalf("mqtt connect failed: %v", token.Error())
	}
	defer cl.Disconnect(250)

	downstreamTopic := fmt.Sprintf("monstermq/hmi/sync/%s/downstream", sessionUUID)
	upstreamTopic := fmt.Sprintf("monstermq/hmi/sync/%s/upstream", sessionUUID)

	respCh := make(chan hmi.SyncResponse, 10)
	subToken := cl.Subscribe(downstreamTopic, 1, func(_ paho.Client, msg paho.Message) {
		var resp hmi.SyncResponse
		if err := json.Unmarshal(msg.Payload(), &resp); err == nil {
			respCh <- resp
		}
	})
	if subToken.Wait() && subToken.Error() != nil {
		t.Fatalf("subscribe failed: %v", subToken.Error())
	}

	sendUpstream := func(req hmi.SyncRequest) hmi.SyncResponse {
		t.Helper()
		payload, _ := json.Marshal(req)
		pubToken := cl.Publish(upstreamTopic, 1, false, payload)
		if pubToken.Wait() && pubToken.Error() != nil {
			t.Fatalf("publish failed: %v", pubToken.Error())
		}
		select {
		case resp := <-respCh:
			return resp
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for downstream response to action %q", req.Action)
			return hmi.SyncResponse{}
		}
	}

	// 2. Test Ping
	pingResp := sendUpstream(hmi.SyncRequest{
		Action: "ping",
		ReqID:  "p1",
	})
	if !pingResp.Success {
		t.Fatalf("ping failed: %s", pingResp.Error)
	}
	if pingResp.BrokerVersion == "" || pingResp.MainDashboard != "main" {
		t.Fatalf("unexpected ping resp: %+v", pingResp)
	}

	// 3. Test List on default dashboard
	listResp := sendUpstream(hmi.SyncRequest{
		Action:    "list",
		ReqID:     "l1",
		Dashboard: "main",
	})
	if !listResp.Success {
		t.Fatalf("list failed: %s", listResp.Error)
	}
	if listResp.FileCount < 1 {
		t.Fatalf("expected at least index.html in main dashboard, got fileCount=%d", listResp.FileCount)
	}

	// 4. Test Export (Initial Pull Zip)
	exportResp := sendUpstream(hmi.SyncRequest{
		Action:    "export",
		ReqID:     "e1",
		Dashboard: "main",
	})
	if !exportResp.Success {
		t.Fatalf("export failed: %s", exportResp.Error)
	}
	zipBytes, err := base64.StdEncoding.DecodeString(exportResp.ZipBase64)
	if err != nil || len(zipBytes) == 0 {
		t.Fatalf("invalid zip bytes returned: %v", err)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}
	foundIndex := false
	for _, f := range zipReader.File {
		if f.Name == "index.html" {
			foundIndex = true
			break
		}
	}
	if !foundIndex {
		t.Fatal("expected index.html inside exported zip")
	}

	// 5. Test Live Write
	newContent := "console.log('sensor widget active');"
	writeResp := sendUpstream(hmi.SyncRequest{
		Action:        "write",
		ReqID:         "w1",
		Dashboard:     "main",
		Path:          "widget.js",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(newContent)),
	})
	if !writeResp.Success {
		t.Fatalf("write failed: %s", writeResp.Error)
	}
	if writeResp.BytesWritten != int64(len(newContent)) {
		t.Fatalf("expected bytesWritten=%d, got %d", len(newContent), writeResp.BytesWritten)
	}

	// Verify file is on disk
	diskFile := filepath.Join(hmiDir, "main", "widget.js")
	diskBytes, err := os.ReadFile(diskFile)
	if err != nil {
		t.Fatalf("expected widget.js on disk at %s, err: %v", diskFile, err)
	}
	if string(diskBytes) != newContent {
		t.Fatalf("content mismatch on disk: got %q, expected %q", string(diskBytes), newContent)
	}

	// Verify HTTP serving of written file
	httpURL := fmt.Sprintf("http://localhost:%d/hmi/main/widget.js", gqlPort)
	httpResp, err := http.Get(httpURL)
	if err != nil {
		t.Fatalf("http GET %s failed: %v", httpURL, err)
	}
	body, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if httpResp.StatusCode != 200 || string(body) != newContent {
		t.Fatalf("expected 200 OK with content, got status %d, body %q", httpResp.StatusCode, string(body))
	}

	// 6. Test Read
	readResp := sendUpstream(hmi.SyncRequest{
		Action:    "read",
		ReqID:     "r1",
		Dashboard: "main",
		Path:      "widget.js",
	})
	if !readResp.Success {
		t.Fatalf("read failed: %s", readResp.Error)
	}
	readBytes, _ := base64.StdEncoding.DecodeString(readResp.ContentBase64)
	if string(readBytes) != newContent {
		t.Fatalf("read content mismatch: got %q, expected %q", string(readBytes), newContent)
	}

	// 7. Test Directory Traversal Defense
	traversalResp := sendUpstream(hmi.SyncRequest{
		Action:        "write",
		ReqID:         "t1",
		Dashboard:     "main",
		Path:          "../../outside.txt",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("malicious")),
	})
	if traversalResp.Success {
		t.Fatal("expected traversal write to fail, but got success")
	}

	// 8. Test Delete
	delResp := sendUpstream(hmi.SyncRequest{
		Action:    "delete",
		ReqID:     "d1",
		Dashboard: "main",
		Path:      "widget.js",
	})
	if !delResp.Success {
		t.Fatalf("delete failed: %s", delResp.Error)
	}

	// Verify file is gone from disk
	if _, err := os.Stat(diskFile); !os.IsNotExist(err) {
		t.Fatal("expected widget.js to be deleted from disk")
	}

	// Verify HTTP returns 404
	httpResp2, err := http.Get(httpURL)
	if err == nil {
		httpResp2.Body.Close()
		if httpResp2.StatusCode != 404 {
			t.Fatalf("expected 404 Not Found after delete, got %d", httpResp2.StatusCode)
		}
	}
}

func TestHmiSync_MultiSessionIsolation(t *testing.T) {
	tempDir := t.TempDir()
	hmiDir := filepath.Join(tempDir, "hmi_data")
	_ = os.MkdirAll(hmiDir, 0755)

	mqttPort := 22051
	gqlPort := 24051
	srv, _ := startWithHMI(t, mqttPort, gqlPort, hmiDir)
	defer srv.Close()

	// Client Alpha
	clAlpha := paho.NewClient(mqttOpts(mqttPort, "client-alpha"))
	if tok := clAlpha.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer clAlpha.Disconnect(250)

	alphaDownstream := "monstermq/hmi/sync/sess-alpha/downstream"
	alphaUpstream := "monstermq/hmi/sync/sess-alpha/upstream"
	alphaCh := make(chan hmi.SyncResponse, 5)
	clAlpha.Subscribe(alphaDownstream, 1, func(_ paho.Client, msg paho.Message) {
		var r hmi.SyncResponse
		_ = json.Unmarshal(msg.Payload(), &r)
		alphaCh <- r
	})

	// Client Beta
	clBeta := paho.NewClient(mqttOpts(mqttPort, "client-beta"))
	if tok := clBeta.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer clBeta.Disconnect(250)

	betaDownstream := "monstermq/hmi/sync/sess-beta/downstream"
	betaUpstream := "monstermq/hmi/sync/sess-beta/upstream"
	betaCh := make(chan hmi.SyncResponse, 5)
	clBeta.Subscribe(betaDownstream, 1, func(_ paho.Client, msg paho.Message) {
		var r hmi.SyncResponse
		_ = json.Unmarshal(msg.Payload(), &r)
		betaCh <- r
	})

	time.Sleep(100 * time.Millisecond)

	// Send from Alpha
	reqAlpha, _ := json.Marshal(hmi.SyncRequest{Action: "ping", ReqID: "req-alpha"})
	clAlpha.Publish(alphaUpstream, 1, false, reqAlpha)

	// Send from Beta
	reqBeta, _ := json.Marshal(hmi.SyncRequest{Action: "ping", ReqID: "req-beta"})
	clBeta.Publish(betaUpstream, 1, false, reqBeta)

	// Verify Alpha receives only Alpha's response
	select {
	case respA := <-alphaCh:
		if respA.ReqID != "req-alpha" {
			t.Fatalf("unexpected reqId for alpha: %s", respA.ReqID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alpha timed out waiting for response")
	}

	// Verify Beta receives only Beta's response
	select {
	case respB := <-betaCh:
		if respB.ReqID != "req-beta" {
			t.Fatalf("unexpected reqId for beta: %s", respB.ReqID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("beta timed out waiting for response")
	}
}
