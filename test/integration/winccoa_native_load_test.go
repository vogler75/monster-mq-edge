package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/loadgen"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/oahost/simhost"
)

// Preliminary AC-34 numbers with the simulated host on the build machine.
// Set MMQ_LOAD=1 to run; the acceptance run uses cmd/mmqload against the
// real manager and project.
func TestNativeLoadSimulated(t *testing.T) {
	if os.Getenv("MMQ_LOAD") == "" {
		t.Skip("set MMQ_LOAD=1 to run the load measurement")
	}
	sim, client := newSim(0)
	defer sim.Close()
	sim.AddType(simhost.Type{Name: "MMQLoad", Elements: map[string]uint32{"": simhost.ElemStruct, "value": uint32(oahost.KindFloat)}})
	const dpes = 5000
	for i := 0; i < dpes; i++ {
		if err := sim.CreateDP("System1", fmt.Sprintf("MMQLoad%05d", i), "MMQLoad"); err != nil {
			t.Fatal(err)
		}
	}
	env := startNative(t, 27170, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	base := loadgen.Config{
		Broker: "tcp://127.0.0.1:27170", Subscribers: 50, Writers: 4, DPEs: dpes,
		TopicFmt: loadgen.TopicFmtFor("MMQLoad", "value"), Rate: 2000, Duration: 30 * time.Second,
	}
	// Leg 1: OA value change -> MQTT subscriber (budget p99 <= 50 ms).
	leg := base
	leg.Change = func(dpe int, seq int64) error {
		return sim.Set(fmt.Sprintf("System1:MMQLoad%05d.value", dpe), oahost.Value{Kind: oahost.KindFloat, Float: float64(seq)})
	}
	res, err := loadgen.Run(leg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("OA change -> subscriber:\n%s", res)
	if res.Lost > 0 || res.P99 > 50*time.Millisecond {
		t.Errorf("OA -> MQTT budget missed: lost=%d p99=%s", res.Lost, res.P99)
	}
	// Round trip: MQTT write -> OA -> hotlink -> subscriber (budget p99 <=
	// write 100 ms + delivery 50 ms).
	res, err = loadgen.Run(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("write round trip:\n%s\nclient stats %+v", res, client.Stats())
	if res.Lost > 0 || res.P99 > 150*time.Millisecond {
		t.Errorf("round-trip budget missed: lost=%d p99=%s", res.Lost, res.P99)
	}
}
