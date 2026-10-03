package metrics

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestCollectorBusCounters(t *testing.T) {
	c := New(nil, "n1", 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.IncIn()
	c.IncBusIn()
	c.IncBusIn()
	c.IncBusOut(5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, nil)
	defer c.Stop()

	deadline := time.Now().Add(5 * time.Second)
	var snap BrokerSnapshot
	for time.Now().Before(deadline) {
		if snap = c.Latest(); snap.MessageBusIn > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	perSec := 1 / c.Interval().Seconds()
	if snap.MessageBusIn != 2*perSec || snap.MessageBusOut != 5*perSec || snap.MessagesIn != perSec {
		t.Fatalf("snapshot = %+v, want messageBusIn %v, messageBusOut %v, messagesIn %v", snap, 2*perSec, 5*perSec, perSec)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"messageBusIn":`, `"messageBusOut":`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("snapshot JSON %s lacks %s", raw, key)
		}
	}
}
