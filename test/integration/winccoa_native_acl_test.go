package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

// AC-25: read-only, write-only, restricted and denied users cannot bypass
// their ACLs through aliases, type paths, attributes, shared/wildcard
// forms, broad filters, status topics or native-storage datapoints.
func TestNativeAccessIsolation(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	users := []struct {
		u     stores.User
		rules []stores.AclRule
	}{
		{stores.User{Username: "admin", IsAdmin: true, Enabled: true, CanSubscribe: true, CanPublish: true}, nil},
		{stores.User{Username: "reader", Enabled: true, CanSubscribe: true}, []stores.AclRule{
			{ID: "r1", TopicPattern: "winccoa/systems/System1/tags/Pump101/#", Priority: 100},
			{ID: "r2", TopicPattern: "#", CanSubscribe: true, Priority: 1},
		}},
		{stores.User{Username: "writer", Enabled: true, CanPublish: true}, []stores.AclRule{
			{ID: "w1", TopicPattern: "winccoa/systems/System1/tags/Pump1/speed/set", CanPublish: true, Priority: 100},
			{ID: "w2", TopicPattern: "#", Priority: 1},
		}},
		{stores.User{Username: "denied", Enabled: true}, nil},
	}
	cfgFn := func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
	}
	env := startNative(t, 27150, filepath.Join(t.TempDir(), "n.db"), sim, client, cfgFn, broker.Options{
		ConfigureStorage: func(ctx context.Context, s *stores.Storage) error {
			hash, _ := storesqlite.HashPassword("pw")
			for _, x := range users {
				x.u.PasswordHash = hash
				if err := s.Users.CreateUser(ctx, x.u); err != nil {
					return err
				}
				for _, r := range x.rules {
					r.Username = x.u.Username
					if err := s.Users.CreateAclRule(ctx, r); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
	defer env.srv.Close()
	_ = env.srv.AuthCache().Refresh(context.Background())

	login := func(user string) *rawClient {
		c, _ := dialRaw(t, env.port, rawConnect{ClientID: "acl-" + user, Version: 5, Clean: true, Username: user, Password: "pw"})
		return c
	}
	admin, reader, writer, denied := login("admin"), login("reader"), login("writer"), login("denied")
	defer admin.Close()
	defer reader.Close()
	defer writer.Close()
	defer denied.Close()

	codes := reader.Subscribe(
		sub("winccoa/systems/System1/tags/Pump1/speed", 1),                   // allowed
		sub("winccoa/systems/System1/tags/Pump101/speed", 1),                 // ACL denied
		sub("winccoa/systems/System1/types/AnalogDrive/Pump101/speed", 1),    // alias of a denied element
		sub("winccoa/systems/System1/tags/Pump101/speed/_online.._stime", 1), // attribute of a denied element
		sub("winccoa/systems/System1/tags/Pump101/speed/_online.._value", 1), // explicit default attribute
		sub("$share/g/winccoa/systems/System1/tags/Pump101/speed", 1),        // shared form
		sub("winccoa/systems/System1/tags/+/speed", 1),                       // wildcard form
		sub("winccoa/systems/System1/tags/MMQConfigs_k1/config", 1),          // native storage datapoint
		sub("#", 0), // broad filter
	)
	// The wildcard is accepted; delivery-time ACL checks keep the denied
	// element out of it (checked below).
	want := []byte{0x01, 0x87, 0x87, 0x87, 0x87, 0x9E, 0x01, 0x87, 0x00}
	if string(codes) != string(want) {
		t.Fatalf("reader SUBACK\n got % x\nwant % x", codes, want)
	}
	if code := reader.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/speed/set", Payload: []byte(`1`), QoS: 1}); code != 0x87 {
		t.Errorf("read-only user write: PUBACK 0x%02x", code)
	}

	// Another client subscribes the denied element through an alias: the
	// reader's broad filter must not receive it.
	admin.Subscribe(sub("winccoa/systems/System1/types/AnalogDrive/Pump101/speed", 1), sub("winccoa/systems/System1/tags/Pump1/speed", 1))
	admin.Drain(300 * time.Millisecond)
	reader.Drain(300 * time.Millisecond)
	_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 66})
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 11})
	if _, ok := admin.NextOn("winccoa/systems/System1/types/AnalogDrive/Pump101/speed", 2*time.Second); !ok {
		t.Fatal("admin did not receive the alias publication")
	}
	got := map[string]bool{}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pk, ok := reader.Next(time.Until(deadline))
		if !ok {
			break
		}
		got[pk.TopicName] = true
	}
	if got["winccoa/systems/System1/types/AnalogDrive/Pump101/speed"] {
		t.Error("broad filter leaked a denied element through its type alias")
	}
	if got["winccoa/systems/System1/tags/Pump101/speed"] {
		t.Error("native wildcard leaked a denied element")
	}
	if !got["winccoa/systems/System1/tags/Pump1/speed"] {
		t.Errorf("reader did not receive its allowed element: %v", got)
	}

	// Write-only user: only the exact allowed command, no alias widening.
	if code := writer.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/speed/set", Payload: []byte(`42`), QoS: 1}); code != 0 {
		t.Errorf("allowed write: PUBACK 0x%02x", code)
	}
	for _, topic := range []string{
		"winccoa/systems/System1/types/AnalogDrive/Pump1/speed/set",
		"winccoa/systems/System1/tags/Pump1/count/set",
		"winccoa/systems/System1/tags/Pump101/speed/set",
		"winccoa/systems/System1",
		"winccoa/systems/System1/tags/MMQConfigs_k1/config/set",
	} {
		if code := writer.Publish(rawPub{Topic: topic, Payload: []byte(`1`), QoS: 1}); code != 0x87 {
			t.Errorf("writer %s: PUBACK 0x%02x", topic, code)
		}
	}
	if got := writer.Subscribe(sub("winccoa/systems/System1/tags/Pump1/speed", 1)); got[0] != 0x87 {
		t.Errorf("write-only subscribe: 0x%02x", got[0])
	}
	time.Sleep(200 * time.Millisecond)
	if v, _ := sim.Get("System1:Pump1.speed"); v.Float != 42 {
		t.Errorf("allowed write not applied: %v", v.Float)
	}
	if v, _ := sim.Get("System1:Pump101.speed"); v.Float != 66 {
		t.Errorf("denied write applied: %v", v.Float)
	}

	// Denied user: nothing.
	if got := denied.Subscribe(sub("winccoa/systems/System1/tags/Pump1/speed", 1), sub("winccoa/systems/System1", 1)); string(got) != "\x87\x87" {
		t.Errorf("denied SUBACK % x", got)
	}
	if code := denied.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/speed/set", Payload: []byte(`1`), QoS: 1}); code != 0x87 {
		t.Errorf("denied write: PUBACK 0x%02x", code)
	}
}

// AC-26: QoS 2 commands get the same validation outcome on PUBREC.
func TestNativeCommandQoS2(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27151, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "q2", Version: 5, Clean: true})
	defer c.Close()
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/speed/set", Payload: []byte(`5`), QoS: 2}); code != 0 {
		t.Fatalf("QoS 2 accepted command: PUBREC 0x%02x", code)
	}
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/running/set", Payload: []byte(`5`), QoS: 2}); code != 0x99 {
		t.Fatalf("QoS 2 invalid command: PUBREC 0x%02x", code)
	}
	time.Sleep(200 * time.Millisecond)
	if v, _ := sim.Get("System1:Pump1.speed"); v.Float != 5 {
		t.Fatalf("QoS 2 command not applied: %v", v.Float)
	}
}

var _ = packets.Subscription{}
