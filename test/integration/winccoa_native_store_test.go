package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/oahost/simhost"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/stores/oastore"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

func withOAStores(c *config.Config) {
	c.ConfigStoreType = config.StoreWinCCOA
	c.SessionStoreType = config.StoreWinCCOA
}

// AC-14: every OA-backed store operation, and survival across restart.
func TestNativeStoreCoverage(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	db := filepath.Join(t.TempDir(), "n.db")
	env := startNative(t, 27140, db, sim, client, withOAStores, broker.Options{})
	ctx := context.Background()
	st := env.srv.Storage()
	if _, ok := st.DeviceConfig.(*oastore.DeviceConfigStore); !ok {
		t.Fatalf("device store is %T", st.DeviceConfig)
	}

	devs := []stores.DeviceConfig{
		{Name: "a", Namespace: "ns", NodeID: "n-27140", Type: "WinCCOA-Client", Enabled: true, Config: `{"x":1}`},
		{Name: "b", Namespace: "ns", NodeID: "*", Type: "MQTT-Client", Enabled: false, Config: `{}`},
		{Name: "c", Namespace: "ns", NodeID: "other", Type: "MQTT-Client", Enabled: true, Config: `{}`},
	}
	for _, d := range devs {
		if err := st.DeviceConfig.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	check := func(label string, got []stores.DeviceConfig, want ...string) {
		t.Helper()
		var names []string
		for _, d := range got {
			names = append(names, d.Name)
		}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: %v want %v", label, names, want)
		}
	}
	all, _ := st.DeviceConfig.GetAll(ctx)
	check("all", all, "a", "b", "c")
	byType, _ := st.DeviceConfig.GetByType(ctx, "MQTT-Client")
	check("type", byType, "b", "c")
	byNode, _ := st.DeviceConfig.GetByNode(ctx, "n-27140")
	check("node", byNode, "a", "b")
	enabled, _ := st.DeviceConfig.GetEnabledByNode(ctx, "n-27140")
	check("enabled", enabled, "a")
	a1, _ := st.DeviceConfig.Get(ctx, "a")
	if _, err := st.DeviceConfig.Toggle(ctx, "a", false); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.DeviceConfig.Reassign(ctx, "c", "n-27140"); d == nil || d.NodeID != "n-27140" {
		t.Fatalf("reassign %+v", d)
	}
	a2, _ := st.DeviceConfig.Get(ctx, "a")
	if a2.Enabled || !a2.CreatedAt.Equal(a1.CreatedAt) || !a2.UpdatedAt.After(a1.CreatedAt.Add(-time.Millisecond)) {
		t.Fatalf("toggle/created: %+v", a2)
	}
	if d, err := st.DeviceConfig.Toggle(ctx, "missing", true); d != nil || err != nil {
		t.Fatalf("toggle of a missing device: %v %v", d, err)
	}
	if err := st.DeviceConfig.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	grp := stores.ArchiveGroupConfig{Name: "G1", Enabled: true, TopicFilters: []string{"a/#", "b/+"}, LastValType: stores.MessageStoreType("NONE"), ArchiveType: stores.MessageArchiveType("NONE"), DatabaseConnectionName: "pg1"}
	if err := st.ArchiveConfig.Save(ctx, grp); err != nil {
		t.Fatal(err)
	}
	grp.RetainedOnly = true
	if err := st.ArchiveConfig.Update(ctx, grp); err != nil {
		t.Fatal(err)
	}
	conn := stores.DatabaseConnectionConfig{Name: "pg1", Type: stores.DatabaseConnectionType("POSTGRES"), URL: "postgres://x", Username: "u", Password: "p", ReadOnly: true}
	if err := st.ArchiveConfig.SaveDatabaseConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveConfig.SaveDatabaseConnection(ctx, stores.DatabaseConnectionConfig{Name: "tmp", Type: "POSTGRES"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveConfig.DeleteDatabaseConnection(ctx, "tmp"); err != nil {
		t.Fatal(err)
	}
	env.srv.Close()

	// Restart on the same OA datapoints: everything is still there.
	env2 := startNative(t, 27140, db, sim, client, withOAStores, broker.Options{})
	defer env2.srv.Close()
	st2 := env2.srv.Storage()
	all, _ = st2.DeviceConfig.GetAll(ctx)
	check("after restart", all, "a", "c")
	if g, _ := st2.ArchiveConfig.Get(ctx, "G1"); g == nil || !g.RetainedOnly || strings.Join(g.TopicFilters, ",") != "a/#,b/+" {
		t.Fatalf("archive group after restart %+v", g)
	}
	conns, _ := st2.ArchiveConfig.GetAllDatabaseConnections(ctx)
	if len(conns) != 1 || conns[0].Password != "p" || !conns[0].ReadOnly {
		t.Fatalf("db connections after restart %+v", conns)
	}
	if err := st2.ArchiveConfig.Delete(ctx, "G1"); err != nil {
		t.Fatal(err)
	}
	if g, _ := st2.ArchiveConfig.Get(ctx, "G1"); g != nil {
		t.Fatal("archive group not deleted")
	}
}

// AC-15: identity, encoding, size limits and corrupt/unsupported records.
func TestNativeStoreIdentity(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27141, filepath.Join(t.TempDir(), "n.db"), sim, client, withOAStores, broker.Options{})
	ctx := context.Background()
	st := env.srv.Storage()

	long := strings.Repeat("prefix/", 40)
	keys := []string{"a.b:c", "a_b_c", "a/b/c", "Pumpe-Ölstand ✓", long + "1", long + "2", "", " "}
	for i, k := range keys {
		if err := st.DeviceConfig.Save(ctx, stores.DeviceConfig{Name: k, Config: fmt.Sprintf(`{"i":%d}`, i)}); err != nil {
			t.Fatalf("save %q: %v", k, err)
		}
	}
	for i, k := range keys {
		d, err := st.DeviceConfig.Get(ctx, k)
		if err != nil || d == nil || d.Config != fmt.Sprintf(`{"i":%d}`, i) {
			t.Fatalf("key %q round trip: %+v %v", k, d, err)
		}
	}
	seen := map[string]bool{}
	for _, k := range keys {
		n := oastore.DPName(oastore.ConfigType, "device", k)
		if seen[n] || strings.ContainsAny(n, ".: /") {
			t.Fatalf("datapoint name %q for key %q", n, k)
		}
		seen[n] = true
	}

	if err := st.DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "big", Config: strings.Repeat("x", 300<<10)}); !errors.Is(err, oahost.ErrTooLarge) {
		t.Fatalf("oversized record: %v", err)
	}

	// Sessions: binary will payload and metadata round trip exactly.
	will := []byte{0, 1, 2, 0xFF, '"', '\\', 0}
	if err := st.Sessions.SetClient(ctx, stores.SessionInfo{ClientID: "bin", NodeID: "n", CleanSession: false, Connected: true, Information: `{"sessionExpiryInterval":60}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions.SetLastWill(ctx, "bin", "will/topic", will, 1, true); err != nil {
		t.Fatal(err)
	}
	si, _ := st.Sessions.GetSession(ctx, "bin")
	if si == nil || !bytes.Equal(si.LastWillPayload, will) || si.LastWillQoS != 1 || !si.LastWillRetain || si.Information != `{"sessionExpiryInterval":60}` {
		t.Fatalf("session round trip %+v", si)
	}
	env.srv.Close()

	// Corrupt and future-version records are never reset silently.
	corrupt := oastore.DPName(oastore.ConfigType, "device", "a/b/c")
	if err := sim.Set("System1:"+corrupt+".config", oahost.Value{Kind: oahost.KindString, Str: "{not json"}); err != nil {
		t.Fatal(err)
	}
	future := oastore.DPName(oastore.ConfigType, "device", "a_b_c")
	if err := sim.Set("System1:"+future+".config", oahost.Value{Kind: oahost.KindString, Str: `{"v":2,"kind":"device","key":"a_b_c","rev":9,"data":{}}`}); err != nil {
		t.Fatal(err)
	}
	env2 := startNative(t, 27141, filepath.Join(t.TempDir(), "n2.db"), sim, client, withOAStores, broker.Options{})
	defer env2.srv.Close()
	st2 := env2.srv.Storage()
	for _, k := range []string{"a/b/c", "a_b_c"} {
		if d, _ := st2.DeviceConfig.Get(ctx, k); d != nil {
			t.Fatalf("unreadable record %q returned: %+v", k, d)
		}
		if err := st2.DeviceConfig.Save(ctx, stores.DeviceConfig{Name: k}); !errors.Is(err, oastore.ErrCorrupt) {
			t.Fatalf("overwrite of unreadable record %q: %v", k, err)
		}
	}
	if v, _ := sim.Get("System1:" + corrupt + ".config"); v.Str != "{not json" {
		t.Fatalf("corrupt record changed: %q", v.Str)
	}
	if d, _ := st2.DeviceConfig.Get(ctx, "Pumpe-Ölstand ✓"); d == nil {
		t.Fatal("valid record lost next to corrupt ones")
	}
}

// AC-16: failed or unconfirmed writes are reported and never cached as
// committed; the committed revision stays readable.
func TestNativeStoreCommitErrors(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27142, filepath.Join(t.TempDir(), "n.db"), sim, client, withOAStores, broker.Options{})
	defer env.srv.Close()
	ctx := context.Background()
	st := env.srv.Storage()
	if err := st.DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "d", Config: `{"rev":1}`}); err != nil {
		t.Fatal(err)
	}

	sim.Pause()
	err := st.DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "d", Config: `{"rev":2}`})
	sim.Resume()
	if !errors.Is(err, oahost.ErrPersist) {
		t.Fatalf("unconfirmed write reported as %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if d, _ := st.DeviceConfig.Get(ctx, "d"); d == nil || d.Config != `{"rev":1}` {
		t.Fatalf("cache advanced past the committed revision: %+v", d)
	}
	name := oastore.DPName(oastore.ConfigType, "device", "d")
	v, _ := sim.Get("System1:" + name + ".config")
	if !strings.Contains(v.Str, `"rev":1`) || strings.Contains(v.Str, `\"rev\":2`) {
		t.Fatalf("expired write executed late: %s", v.Str)
	}

	// OA refuses the write (DP deleted behind the broker's back).
	sim.DeleteDP("System1", name)
	if err := st.DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "d", Config: `{"rev":3}`}); !errors.Is(err, oahost.ErrPersist) {
		t.Fatalf("denied write reported as %v", err)
	}

	// Concurrent subscription changes on one client are all committed.
	errc := make(chan error, 40)
	for i := 0; i < 40; i++ {
		go func(i int) {
			errc <- st.Sessions.AddSubscriptions(ctx, []stores.MqttSubscription{{ClientID: "cc", TopicFilter: fmt.Sprintf("t/%d", i), QoS: 1}})
		}(i)
	}
	for i := 0; i < 40; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	subs, _ := st.Sessions.GetSubscriptionsForClient(ctx, "cc")
	if len(subs) != 40 {
		t.Fatalf("lost concurrent subscription updates: %d", len(subs))
	}
}

// AC-17: MQTT session behavior with sessions stored in OA datapoints.
func TestNativeStoreSessions(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	db := filepath.Join(t.TempDir(), "n.db")
	cfgFn := func(c *config.Config) {
		withOAStores(c)
		c.QueuedMessagesEnabled = true
		c.QueueStoreType = config.StoreSQLite
	}
	env := startNative(t, 27143, db, sim, client, cfgFn, broker.Options{})

	// MQTT 5 persistent session with a native and an ordinary subscription.
	c, ack := dialRaw(t, env.port, rawConnect{ClientID: "p5", Version: 5, Clean: true, SessionExpiry: 3600})
	if ack.SessionPresent {
		t.Fatal("unexpected session present")
	}
	c.Subscribe(sub("winccoa/systems/System1/tags/Pump1/count", 1), sub("plain/topic", 1))
	c.Close()
	// MQTT 3.1.1 clean session: purged, not restored.
	c3, _ := dialRaw(t, env.port, rawConnect{ClientID: "c3", Version: 4, Clean: true})
	c3.Subscribe(sub("plain/topic", 1))
	c3.Close()
	// MQTT 5 with zero expiry: gone at disconnect.
	c0, _ := dialRaw(t, env.port, rawConnect{ClientID: "p0", Version: 5, Clean: true})
	c0.Subscribe(sub("winccoa/systems/System1/tags/Pump1/speed", 1))
	c0.Close()
	time.Sleep(300 * time.Millisecond)
	ctx := context.Background()
	if si, _ := env.srv.Storage().Sessions.GetSession(ctx, "p5"); si == nil || si.Connected {
		t.Fatalf("p5 session after disconnect %+v", si)
	}
	env.srv.Close()

	env2 := startNative(t, 27143, db, sim, client, cfgFn, broker.Options{})
	defer env2.srv.Close()
	time.Sleep(300 * time.Millisecond)
	// Only the persistent native interest was restored.
	if names := sim.ConnectedNames(); len(names) != 1 || !strings.HasPrefix(names[0], "System1:Pump1.count") {
		t.Fatalf("restored interests %v", names)
	}
	_ = sim.Set("System1:Pump1.count", oahost.Value{Kind: oahost.KindInt, Int: 5})
	time.Sleep(200 * time.Millisecond)

	r, ack := dialRaw(t, env2.port, rawConnect{ClientID: "p5", Version: 5, Clean: false, SessionExpiry: 3600})
	defer r.Close()
	if !ack.SessionPresent {
		t.Fatal("CONNACK session present must reflect the restored OA session")
	}
	if pk, ok := r.NextOn("winccoa/systems/System1/tags/Pump1/count", 3*time.Second); !ok || payloadValue(t, pk) != float64(5) {
		t.Fatal("queued native change not delivered after restart")
	}
	pub, _ := dialRaw(t, env2.port, rawConnect{ClientID: "pub", Version: 5, Clean: true})
	defer pub.Close()
	pub.Publish(rawPub{Topic: "plain/topic", Payload: []byte("x"), QoS: 1})
	if _, ok := r.NextOn("plain/topic", 3*time.Second); !ok {
		t.Fatal("restored ordinary subscription not active")
	}
	if si, _ := env2.srv.Storage().Sessions.GetSession(ctx, "c3"); si != nil {
		t.Fatalf("clean 3.1.1 session restored: %+v", si)
	}

	// Client takeover with clean start drops the old interests.
	r2, _ := dialRaw(t, env2.port, rawConnect{ClientID: "p5", Version: 5, Clean: true})
	defer r2.Close()
	time.Sleep(300 * time.Millisecond)
	if n := sim.Connections(); n != 0 {
		t.Fatalf("interests survived a clean takeover: %d", n)
	}
}

// AC-08: a missing or different store DPT stops startup.
func TestNativeStoreTypes(t *testing.T) {
	sim := simhost.New("System1", 0)
	client := oahost.NewClient(sim, oahost.Limits{DefaultTimeout: time.Second})
	sim.Attach(client)
	defer sim.Close()
	sim.AddType(simhost.Type{Name: "MMQConfigs", Elements: map[string]uint32{"": simhost.ElemStruct, "config": uint32(oahost.KindInt)}})
	cfg := config.Default()
	cfg.NodeID = "mt"
	cfg.TCP.Port = 27144
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "n.db")
	cfg.WinCCOaNative = config.WinCCOaNativeConfig{Enabled: true}
	cfg.ConfigStoreType = config.StoreWinCCOA
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// WINCCOA stores need the WinCC OA manager.
	if _, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, broker.Options{}); err == nil || !strings.Contains(err.Error(), "WinCC OA manager") {
		t.Fatalf("expected an error without OA, got %v", err)
	}
	// An existing type with a different layout is an error and stays as it is.
	_, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, broker.Options{OA: client})
	if err == nil || !strings.Contains(err.Error(), "MMQConfigs") {
		t.Fatalf("expected a layout error, got %v", err)
	}
	// Missing types are created by the manager.
	cfg.ConfigStoreType = config.StoreSQLite
	cfg.SessionStoreType = config.StoreWinCCOA
	cfg.RetainedStoreType = config.StoreWinCCOA
	srv, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, broker.Options{OA: client})
	if err != nil {
		t.Fatalf("missing types not created: %v", err)
	}
	srv.Close()
	if n := sim.TypesCreated(); n != 2 {
		t.Fatalf("created %d types, want MMQSessions and MMQRetained", n)
	}
	// Once they exist, a restart creates nothing.
	srv, err = broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, broker.Options{OA: client})
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if n := sim.TypesCreated(); n != 2 {
		t.Fatalf("types created again: %d", n)
	}
}

// RetainedStoreType WINCCOA: one MMQRetained datapoint per retained topic
// with payload and MQTT user; an empty retained publish deletes it.
func TestNativeStoreRetained(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	withRetained := func(c *config.Config) { c.RetainedStoreType = config.StoreWinCCOA }
	env := startNative(t, 27145, filepath.Join(t.TempDir(), "n.db"), sim, client, withRetained, broker.Options{})
	if _, ok := env.srv.Storage().Retained.(*oastore.RetainedStore); !ok {
		t.Fatalf("retained store is %T", env.srv.Storage().Retained)
	}
	const topic = "plant/line1/temp"
	dp := oastore.DPName(oastore.RetainedType, "retained", topic)
	pub, _ := dialRaw(t, env.port, rawConnect{ClientID: "ret-pub", Version: 5, Clean: true, Username: "alice", Password: "x"})
	if code := pub.Publish(rawPub{Topic: topic, Payload: []byte("21.5"), QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("PUBACK 0x%02x", code)
	}
	pub.Close()
	// The broker's own status topic is retained but gets no datapoint.
	if _, err := sim.Get("System1:" + oastore.DPName(oastore.RetainedType, "retained", "winccoa/systems/System1") + ".value"); err == nil {
		t.Fatal("datapoint created for the native status topic")
	}
	if m, _ := env.srv.Storage().Retained.Get(context.Background(), "winccoa/systems/System1"); m == nil {
		t.Fatal("native status topic not retained in memory")
	}
	check := func(el string, want oahost.Value) {
		t.Helper()
		v, err := sim.Get("System1:" + dp + "." + el)
		if err != nil {
			t.Fatalf("%s.%s: %v", dp, el, err)
		}
		if v.Kind != want.Kind || v.Str != want.Str || v.Uint != want.Uint || !bytes.Equal(v.Bytes, want.Bytes) {
			t.Fatalf("%s.%s = %+v, want %+v", dp, el, v, want)
		}
	}
	check("value", oahost.Value{Kind: oahost.KindBytes, Bytes: []byte("21.5")})
	check("topic", oahost.Value{Kind: oahost.KindString, Str: topic})
	check("user", oahost.Value{Kind: oahost.KindString, Str: "alice"})
	check("qos", oahost.Value{Kind: oahost.KindUint, Uint: 1})
	env.srv.Close()

	// Restart with an empty SQLite file: the retained value comes from OA.
	env = startNative(t, 27145, filepath.Join(t.TempDir(), "n2.db"), sim, client, withRetained, broker.Options{})
	defer env.srv.Close()
	sub, _ := dialRaw(t, env.port, rawConnect{ClientID: "ret-sub", Version: 5, Clean: true})
	sub.Subscribe(sub1("plant/#"))
	pk, ok := sub.NextOn(topic, 2*time.Second)
	if !ok || string(pk.Payload) != "21.5" || !pk.FixedHeader.Retain {
		t.Fatalf("retained message after restart: %v %q", ok, pk.Payload)
	}
	sub.Close()

	// An empty retained publish removes the datapoint.
	pub, _ = dialRaw(t, env.port, rawConnect{ClientID: "ret-pub2", Version: 5, Clean: true, Username: "bob", Password: "x"})
	defer pub.Close()
	if code := pub.Publish(rawPub{Topic: topic, QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("clear PUBACK 0x%02x", code)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := sim.Get("System1:" + dp + ".value"); err == nil {
		t.Fatal("retained datapoint not deleted")
	}
	sub, _ = dialRaw(t, env.port, rawConnect{ClientID: "ret-sub2", Version: 5, Clean: true})
	defer sub.Close()
	sub.Subscribe(sub1("plant/#"))
	if pk, ok := sub.NextOn(topic, 500*time.Millisecond); ok {
		t.Fatalf("cleared retained message delivered: %q", pk.Payload)
	}
}

// UserStoreType WINCCOA: one MMQUsers datapoint per user with its ACL
// rules; the default admin, MQTT login, ACLs and restart all work on it.
func TestNativeStoreUsers(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	withUsers := func(c *config.Config) {
		c.UserStoreType = config.StoreWinCCOA
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
	}
	ctx := context.Background()
	env := startNative(t, 27146, filepath.Join(t.TempDir(), "n.db"), sim, client, withUsers, broker.Options{})
	us, ok := env.srv.Storage().Users.(*oastore.UserStore)
	if !ok {
		t.Fatalf("user store is %T", env.srv.Storage().Users)
	}
	dp := func(user string) string { return "System1:" + oastore.DPName(oastore.UserType, "user", user) }
	if v, err := sim.Get(dp("Admin") + ".isAdmin"); err != nil || !v.Bool {
		t.Fatalf("default admin datapoint: %v %v", v, err)
	}
	hash, _ := storesqlite.HashPassword("pw")
	if err := us.CreateUser(ctx, stores.User{Username: "alice", PasswordHash: hash, Enabled: true, CanSubscribe: true, CanPublish: true}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(ctx, stores.User{Username: "alice"}); err == nil {
		t.Fatal("duplicate user created")
	}
	for _, r := range []stores.AclRule{
		{Username: "alice", TopicPattern: "plant/#", CanSubscribe: true, CanPublish: true, Priority: 1},
		{Username: "alice", TopicPattern: "winccoa/tags/Pump1/#", CanSubscribe: true, Priority: 5},
	} {
		if err := us.CreateAclRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.srv.AuthCache().Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := sim.Get(dp("alice") + ".acl"); !strings.Contains(v.Str, "plant/#") {
		t.Fatalf("acl element %q", v.Str)
	}
	env.srv.Close()

	// Restart with an empty SQLite file: users and rules come from OA.
	env = startNative(t, 27146, filepath.Join(t.TempDir(), "n2.db"), sim, client, withUsers, broker.Options{})
	us = env.srv.Storage().Users.(*oastore.UserStore)
	rules, _ := us.GetUserAclRules(ctx, "alice")
	if len(rules) != 2 || rules[0].TopicPattern != "winccoa/tags/Pump1/#" {
		t.Fatalf("rules after restart (priority order): %+v", rules)
	}
	if u, err := us.ValidateCredentials(ctx, "alice", "wrong"); u != nil || err != nil {
		t.Fatalf("wrong password accepted: %v %v", u, err)
	}
	c, ack := dialRaw(t, env.port, rawConnect{ClientID: "u-ok", Version: 5, Clean: true, Username: "alice", Password: "pw"})
	if ack.ReasonCode != 0 {
		t.Fatalf("login 0x%02x", ack.ReasonCode)
	}
	// Allowed by the ACL; the user datapoints are never reachable as tags.
	got := c.Subscribe(sub("plant/x", 1), sub("other/x", 1), sub("winccoa/tags/"+oastore.DPName(oastore.UserType, "user", "alice")+"/passwordHash", 1))
	if string(got) != "\x01\x87\x87" {
		t.Fatalf("SUBACK % x", got)
	}
	c.Close()
	// Moving a rule to another user, deleting a rule and the user.
	if err := us.CreateUser(ctx, stores.User{Username: "bob", PasswordHash: hash, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	moved := rules[1]
	moved.Username = "bob"
	if err := us.UpdateAclRule(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if a, _ := us.GetUserAclRules(ctx, "alice"); len(a) != 1 {
		t.Fatalf("alice rules after move: %+v", a)
	}
	if b, _ := us.GetUserAclRules(ctx, "bob"); len(b) != 1 || b[0].ID != moved.ID {
		t.Fatalf("bob rules after move: %+v", b)
	}
	if err := us.DeleteAclRule(ctx, moved.ID); err != nil {
		t.Fatal(err)
	}
	if err := us.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Get(dp("alice") + ".user"); err == nil {
		t.Fatal("user datapoint not deleted")
	}
	env.srv.Close()
}

// DefaultStoreType WINCCOA: every persistent store in WinCC OA, queue and
// metrics in memory, no SQLite file.
func TestNativeDefaultStoreWinCCOA(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	dbPath := filepath.Join(t.TempDir(), "never.db")
	env := startNative(t, 27147, dbPath, sim, client, func(c *config.Config) {
		c.DefaultStoreType = config.StoreWinCCOA
		c.UserManagement.Enabled = true
	}, broker.Options{})
	defer env.srv.Close()
	st := env.srv.Storage()
	if _, ok := st.DeviceConfig.(*oastore.DeviceConfigStore); !ok {
		t.Errorf("device configs: %T", st.DeviceConfig)
	}
	if _, ok := st.Sessions.(*oastore.SessionStore); !ok {
		t.Errorf("sessions: %T", st.Sessions)
	}
	if _, ok := st.Retained.(*oastore.RetainedStore); !ok {
		t.Errorf("retained: %T", st.Retained)
	}
	if _, ok := st.Users.(*oastore.UserStore); !ok {
		t.Errorf("users: %T", st.Users)
	}
	if st.Metrics == nil || fmt.Sprintf("%T", st.Metrics) != "*memory.MetricsStore" {
		t.Errorf("metrics: %T", st.Metrics)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("SQLite file created: %v", err)
	}
	// Offline queue in memory works.
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "q", Version: 5, Clean: false, SessionExpiry: 3600, Username: "Admin", Password: "Admin"})
	c.Subscribe(sub("q/#", 1))
	c.Close()
	p, _ := dialRaw(t, env.port, rawConnect{ClientID: "qp", Version: 5, Clean: true, Username: "Admin", Password: "Admin"})
	p.Publish(rawPub{Topic: "q/a", Payload: []byte("x"), QoS: 1})
	p.Close()
	c, ack := dialRaw(t, env.port, rawConnect{ClientID: "q", Version: 5, Clean: false, SessionExpiry: 3600, Username: "Admin", Password: "Admin"})
	defer c.Close()
	if !ack.SessionPresent {
		t.Fatal("session not present")
	}
	if _, ok := c.NextOn("q/a", 2*time.Second); !ok {
		t.Fatal("queued message not delivered")
	}
}

func sub1(filter string) packets.Subscription { return sub(filter, 1) }

var _ = packets.Subscription{}
