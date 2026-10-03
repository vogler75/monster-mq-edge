package integration

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/oahost/simhost"
)

func statusOf(t *testing.T, c *rawClient, topic string) map[string]any {
	t.Helper()
	pk, ok := c.NextOn(topic, 3*time.Second)
	if !ok {
		t.Fatalf("no status on %s", topic)
	}
	var st map[string]any
	if err := json.Unmarshal(pk.Payload, &st); err != nil {
		t.Fatalf("status %q: %v", pk.Payload, err)
	}
	return st
}

// The broker status reports the WinCC OA redundancy role of the host the
// broker runs on and follows a switchover.
func TestNativeRedundancyRole(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	sim.Redu = oahost.SysInfo{Redundant: true, Replica: 2, Hosts: []string{"debian1", "debian2"}, LocalHost: "Debian2.plant.local"}
	sim.AddType(simhost.Type{Name: "_ReduManager", Elements: map[string]uint32{"": simhost.ElemStruct, "Status": simhost.ElemStruct, "Status.Active": uint32(oahost.KindBool)}})
	for _, dp := range []string{"_ReduManager", "_ReduManager_2"} {
		if err := sim.CreateDP("System1", dp, "_ReduManager"); err != nil {
			t.Fatal(err)
		}
	}
	_ = sim.Set("System1:_ReduManager.Status.Active", oahost.Value{Kind: oahost.KindBool, Bool: true})
	env := startNative(t, 27195, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "redu", Version: 5, Clean: true})
	defer c.Close()
	const topic = "winccoa/systems/System1"
	c.Subscribe(sub(topic, 1))
	st := statusOf(t, c, topic)
	if st["redundant"] != true || st["host"] != float64(2) || st["hostName"] != "debian2" || st["role"] != "PASSIVE" || st["activeHost"] != float64(1) {
		t.Fatalf("passive status %v", st)
	}

	// Switchover: host 2 becomes active.
	_ = sim.Set("System1:_ReduManager.Status.Active", oahost.Value{Kind: oahost.KindBool, Bool: false})
	_ = sim.Set("System1:_ReduManager_2.Status.Active", oahost.Value{Kind: oahost.KindBool, Bool: true})
	deadline := time.Now().Add(3 * time.Second)
	for {
		st = statusOf(t, c, topic)
		if st["role"] == "ACTIVE" && st["activeHost"] == float64(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("switchover not reflected: %v", st)
		}
	}
	// A repeated value (second event manager connection) publishes nothing.
	c.Drain(200 * time.Millisecond)
	_ = sim.Set("System1:_ReduManager_2.Status.Active", oahost.Value{Kind: oahost.KindBool, Bool: true})
	if pk, ok := c.NextOn(topic, 400*time.Millisecond); ok {
		t.Fatalf("unchanged state republished: %s", pk.Payload)
	}
}

func TestNativeStandaloneRole(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27196, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "standalone", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(sub("winccoa", 1))
	st := statusOf(t, c, "winccoa")
	if st["redundant"] != false || st["role"] != "STANDALONE" {
		t.Fatalf("standalone status %v", st)
	}
	if _, ok := st["host"]; ok {
		t.Fatalf("host reported without redundancy: %v", st)
	}
}
