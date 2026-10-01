package oahost_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/oahost/simhost"
)

func newLoggedAPI(t *testing.T, level slog.Level) (oahost.API, *bytes.Buffer) {
	t.Helper()
	sim := simhost.New("System1", 64)
	client := oahost.NewClient(sim, oahost.Limits{MaxPending: 16, EventQueue: 64, DefaultTimeout: 2 * time.Second})
	sim.Attach(client)
	t.Cleanup(func() { client.Close(); sim.Close() })
	sim.AddType(simhost.Type{Name: "Scalar", Elements: map[string]uint32{"": uint32(oahost.KindFloat)}})
	for i := 0; i < 25; i++ {
		if err := sim.CreateDP("System1", fmt.Sprintf("Tag%d", i), "Scalar"); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	client.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	return oahost.API{C: client}, &buf
}

func TestCallsLoggedAtDebug(t *testing.T) {
	api, buf := newLoggedAPI(t, slog.LevelDebug)
	ctx := context.Background()
	noop := func(oahost.Message) {}

	query := "SELECT '_online.._value', '_online.._stime' FROM 'Tag*.'"
	ref, err := api.QueryConnect(ctx, query, true, noop, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.QueryDisconnect(ctx, ref); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 25)
	values := make([]oahost.Value, 25)
	for i := range names {
		names[i] = fmt.Sprintf("System1:Tag%d.:_original.._value", i)
		values[i] = oahost.Value{Kind: oahost.KindFloat, Float: float64(i)}
	}
	if err := api.DpSet(ctx, names, values, 0); err != nil {
		t.Fatal(err)
	}
	ref, err = api.DpConnect(ctx, names[:2], oahost.FlagAnswer, noop, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = api.DpDisconnect(ctx, ref)
	if _, err := api.Resolve(ctx, "Missing."); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	for _, want := range []string{
		`msg="oa dpQueryConnectSingle"`,
		`query="SELECT '_online.._value', '_online.._stime' FROM 'Tag*.'"`,
		`msg="oa dpQueryDisconnect"`,
		`msg="oa dpSetWait" count=25`,
		`(+5 more)`,
		`msg="oa dpConnect"`,
		`msg="oa dpDisconnect"`,
		`msg="oa resolve" name=Missing. exists=false`,
		`duration=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
}

func TestCallsNotLoggedAboveDebug(t *testing.T) {
	api, buf := newLoggedAPI(t, slog.LevelInfo)
	if _, err := api.Resolve(context.Background(), "Tag1."); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output at INFO: %s", buf.String())
	}
}
