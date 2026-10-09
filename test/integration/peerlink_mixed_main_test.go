package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink"
)

// Interest routing between an edge node and a main (Kotlin) broker, scenario 17 of
// plan-peerlink-interest-routing 15.2. MONSTERMQ_MAIN_BROKER points at main's broker directory
// with target/classes and target/dependencies built (mvn package). Ports 27540-27549.

type mainNode struct {
	t        *testing.T
	mqttPort int
	peerPort int
	cmd      *exec.Cmd
	log      *bytes.Buffer
}

func startMain(t *testing.T, mqttPort, peerPort, edgePeerPort int, peerInterest string) *mainNode {
	t.Helper()
	dir := os.Getenv("MONSTERMQ_MAIN_BROKER")
	if dir == "" {
		t.Skip("MONSTERMQ_MAIN_BROKER not set")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not found")
	}
	tmp := t.TempDir()
	peer := ""
	if peerInterest != "" {
		peer = fmt.Sprintf("\n      Interest: %q", peerInterest)
	}
	cfg := fmt.Sprintf(`NodeId: mixmain
TCP: %d
WS: 0
DefaultStoreType: SQLITE
SessionStoreType: MEMORY
QueueStoreType: MEMORY
SQLite:
  Path: %q
GraphQL:
  Enabled: false
PeerLink:
  Enabled: true
  AllowUnauthenticatedPeers: true
  KeepAliveSeconds: 2
  Listener:
    Address: 127.0.0.1
    Port: %d
    AllowedNetworks: ["127.0.0.0/8"]
    AllowPlaintext: true
  Fetch:
    MaxWaitMs: 200
    ReconnectMaxMs: 300
  Receive:
    Archive: false
  Interest:
    Enabled: true
    FlushMs: 5
  Peers:
    - NodeId: mixedge
      Address: "127.0.0.1:%d"%s
`, mqttPort, tmp, peerPort, edgePeerPort, peer)
	path := filepath.Join(tmp, "main.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &mainNode{t: t, mqttPort: mqttPort, peerPort: peerPort, log: &bytes.Buffer{}}
	m.cmd = exec.Command("java", "-classpath", "target/classes:target/dependencies/*", "at.rocworks.MonsterKt", "-config", path)
	m.cmd.Dir = dir
	m.cmd.Stdout = m.log
	m.cmd.Stderr = m.log
	if err := m.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = m.cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = m.cmd.Process.Kill()
		<-exited
		if t.Failed() {
			t.Logf("main log:\n%s", m.log.String())
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for _, port := range []int{mqttPort, peerPort} {
		for {
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				break
			}
			select {
			case <-exited:
				t.Fatalf("main exited before port %d was listening", port)
			case <-time.After(100 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatalf("main port %d not listening", port)
			}
		}
	}
	return m
}

// mainStatus is the subset of main's status read here.
type mainStatus struct {
	Consumers []peerlink.ConsumerStatus `json:"consumers"`
	Sources   []struct {
		NodeID   string `json:"nodeId"`
		State    string `json:"state"`
		Injected uint64 `json:"injected"`
	} `json:"sources"`
	Interest *peerlink.InterestCounts `json:"interest"`
}

func (m *mainNode) status() mainStatus {
	m.t.Helper()
	code, body := plHTTP(m.t, m.peerPort, "GET", "/peerlink/v1/status")
	if code != http.StatusOK {
		m.t.Fatalf("main status: HTTP %d %s", code, body)
	}
	var st mainStatus
	if err := json.Unmarshal(body, &st); err != nil {
		m.t.Fatalf("main status json: %v", err)
	}
	return st
}

func (m *mainNode) consumerInterest(peer string) peerlink.InterestStatus {
	for _, c := range m.status().Consumers {
		if c.NodeID == peer && c.Interest != nil {
			return *c.Interest
		}
	}
	return peerlink.InterestStatus{}
}

func (m *mainNode) counts() peerlink.InterestCounts {
	if in := m.status().Interest; in != nil {
		return *in
	}
	return peerlink.InterestCounts{}
}

func (m *mainNode) injected(peer string) uint64 {
	for _, s := range m.status().Sources {
		if s.NodeID == peer {
			return s.Injected
		}
	}
	return 0
}

// TestPeerLinkInterestMainPair runs edge and main as a bidirectional pair, first with interest
// agreed in both directions, then with main's per-peer override OFF so both sides serve all.
func TestPeerLinkInterestMainPair(t *testing.T) {
	for i, mode := range []string{"", "OFF"} {
		name := "interest"
		if mode != "" {
			name = "main-off"
		}
		t.Run(name, func(t *testing.T) {
			base := 27540 + i*4
			edgeMQTT, edgePeer, mainMQTT, mainPeer := base, base+1, base+2, base+3
			m := startMain(t, mainMQTT, mainPeer, edgePeer, mode)
			e := startInterestPL(t, "mixedge", edgeMQTT, edgePeer, []config.PeerConfig{plPeer("mixmain", mainPeer)},
				[]string{"m2e/a/#"}, plInterest)

			sub, _ := dialRaw(t, mainMQTT, rawConnect{ClientID: "mix-sub", Clean: true})
			defer sub.Close()
			sub.Subscribe(packets.Subscription{Filter: "e2m/a/#", Qos: 1})
			pub, _ := dialRaw(t, mainMQTT, rawConnect{ClientID: "mix-pub", Clean: true})
			defer pub.Close()

			e.waitStreaming("mixmain")
			plEventually(t, 30*time.Second, "main streaming from edge", func() bool {
				for _, s := range m.status().Sources {
					if s.NodeID == "mixedge" && s.State == "STREAMING" {
						return true
					}
				}
				return false
			})
			want := "FILTERED"
			if mode == "OFF" {
				want = "ALL"
			}
			plEventually(t, 10*time.Second, "edge serves main "+want, func() bool {
				in := e.peerInterest("mixmain")
				return in.Mode == want && (want == "ALL" || in.Filters >= 1)
			})
			plEventually(t, 10*time.Second, "main serves edge "+want, func() bool {
				in := m.consumerInterest("mixedge")
				return in.Mode == want && (want == "ALL" || in.Filters >= 1)
			})
			t.Logf("edge holds %d filters for main, main holds %d for edge",
				e.peerInterest("mixmain").Filters, m.consumerInterest("mixedge").Filters)

			// edge -> main
			eBefore, mInj := e.interestCounts(), m.injected("mixedge")
			e.publish("e2m/a/1", "1", 1, false)
			e.publish("e2m/b/1", "2", 1, false)
			if _, ok := sub.NextOn("e2m/a/1", 10*time.Second); !ok {
				t.Fatal("main subscriber did not get e2m/a/1")
			}
			wantInj := uint64(1)
			if mode == "OFF" {
				wantInj = 2
			}
			plEventually(t, 5*time.Second, "main injected from edge", func() bool {
				return m.injected("mixedge")-mInj >= wantInj
			})
			time.Sleep(300 * time.Millisecond)
			if got := m.injected("mixedge") - mInj; got != wantInj {
				t.Fatalf("main injected %d records from edge, want %d", got, wantInj)
			}
			eAfter := e.interestCounts()
			if mode == "" && eAfter.InterestSkipped-eBefore.InterestSkipped < 1 {
				t.Fatalf("edge did not skip e2m/b/1: %+v", eAfter)
			}

			// main -> edge
			mBefore, eInj := m.counts(), e.source("mixmain").Injected
			pub.Publish(rawPub{Topic: "m2e/a/1", Payload: []byte("1"), QoS: 1})
			pub.Publish(rawPub{Topic: "m2e/b/1", Payload: []byte("2"), QoS: 1})
			plWaitCount(t, e, "m2e/a/", 1, 10*time.Second, 300*time.Millisecond)
			plEventually(t, 5*time.Second, "edge injected from main", func() bool {
				return e.source("mixmain").Injected-eInj >= wantInj
			})
			time.Sleep(300 * time.Millisecond)
			if got := e.source("mixmain").Injected - eInj; got != wantInj {
				t.Fatalf("edge injected %d records from main, want %d", got, wantInj)
			}
			if mAfter := m.counts(); mode == "" && mAfter.InterestSkipped-mBefore.InterestSkipped < 1 {
				t.Fatalf("main did not skip m2e/b/1: %+v", mAfter)
			}
		})
	}
}
