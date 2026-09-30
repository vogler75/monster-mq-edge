package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/mqtt/packets"
)

// Live acceptance tests against a running WCCOAmmq manager and its
// WinCC OA project. They are skipped unless MMQ_LIVE_PORT is set:
//
//	MMQ_LIVE_PORT=1883 MMQ_LIVE_GQL=4000 MMQ_LIVE_PROJECT=Test321 MMQ_LIVE_NODE=<NodeId> \
//	  go test ./test/integration -run TestLive -v -count=1
//
// Prerequisites: winccoa/scripts/mmqLiveFixture.ctl ran in the project,
// `woa` is on PATH (LD_LIBRARY_PATH includes the WinCC OA bin directory).

type liveEnv struct {
	port    int
	gql     int
	project string
	node    string
	tag     string // "(<manager num>)" to select the statistics line
}

func live(t *testing.T) liveEnv {
	p := os.Getenv("MMQ_LIVE_PORT")
	if p == "" {
		t.Skip("MMQ_LIVE_PORT not set")
	}
	port, _ := strconv.Atoi(p)
	gql, _ := strconv.Atoi(os.Getenv("MMQ_LIVE_GQL"))
	return liveEnv{port: port, gql: gql, project: os.Getenv("MMQ_LIVE_PROJECT"), node: os.Getenv("MMQ_LIVE_NODE")}
}

func (e liveEnv) woa(t *testing.T, args ...string) string {
	t.Helper()
	// woa occasionally hangs on startup; retry with a short timeout.
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err = exec.CommandContext(ctx, "woa", append([]string{"-p", e.project}, args...)...).CombinedOutput()
		cancel()
		if err == nil {
			return string(out)
		}
	}
	t.Fatalf("woa %v: %v\n%s", args, err, out)
	return ""
}

// set changes a value in WinCC OA through a separate client's confirmed
// write command; the value then comes back as a real Event-manager hotlink.
// (`woa set` returns before its dpSet is delivered in this project.)
func (e liveEnv) set(t *testing.T, dpe, value, typ string) {
	t.Helper()
	dp, el, _ := strings.Cut(dpe, ".")
	topic := "winccoa/systems/System1/tags/" + dp
	if el != "" {
		topic += "/" + strings.ReplaceAll(el, ".", "/")
	}
	if typ == "text" {
		b, _ := json.Marshal(value)
		value = string(b)
	}
	c := e.client(t, "live-setter-"+strconv.FormatInt(time.Now().UnixNano(), 36), 5)
	defer c.Close()
	c.Subscribe(sub("live/setres", 1))
	if code := c.Publish(rawPub{Topic: topic + "/set", Payload: []byte(value), QoS: 1, ResponseTopic: "live/setres"}); code != 0 {
		t.Fatalf("set %s: PUBACK 0x%02x", dpe, code)
	}
	if pk, ok := c.NextOn("live/setres", 5*time.Second); !ok || !strings.Contains(string(pk.Payload), "confirmed") {
		t.Fatalf("set %s not confirmed: %s", dpe, pk.Payload)
	}
}

// get returns the text after "= " of `woa get`.
func (e liveEnv) get(t *testing.T, dpe string) string {
	t.Helper()
	out := e.woa(t, "get", dpe)
	i := strings.Index(out, "= ")
	if i < 0 {
		t.Fatalf("woa get %s: %s", dpe, out)
	}
	v := out[i+2:]
	if j := strings.Index(v, "  @"); j >= 0 {
		v = v[:j]
	}
	return strings.TrimSpace(v)
}

func (e liveEnv) client(t *testing.T, id string, version byte) *rawClient {
	c, _ := dialRaw(t, e.port, rawConnect{ClientID: id, Version: version, Clean: true})
	return c
}

func TestLiveStatus(t *testing.T) {
	e := live(t)
	c := e.client(t, "live-status", 5)
	defer c.Close()
	c.Subscribe(sub("winccoa/systems/System1", 1))
	pk, ok := c.NextOn("winccoa/systems/System1", 3*time.Second)
	if !ok {
		t.Fatal("no status")
	}
	var st map[string]any
	_ = json.Unmarshal(pk.Payload, &st)
	t.Logf("status %s", pk.Payload)
	if st["ready"] != true || st["oa"] != "connected" || st["system"] == "" {
		t.Fatalf("status %s", pk.Payload)
	}
}

// AC-20 with real WinCC OA resolution.
func TestLiveSubackMatrix(t *testing.T) {
	e := live(t)
	filters := []packets.Subscription{
		sub("winccoa/systems/System1/tags/MMQLive1/speed", 1),
		sub("winccoa/systems/System1/tags/MMQLiveNope/speed", 1),
		sub("winccoa/systems/System1/tags/_Users", 1),
		sub("winccoa/systems/NoSuchSystem/tags/X/y", 1),
		sub("winccoa/systems/System1/tags/MMQLive1/+", 1),
		sub("$share/g/winccoa/systems/System1/tags/MMQLive1/speed", 1),
		sub("winccoa/systems/System1", 1),
		sub("winccoa/systems/System1/cns/View/node", 1),
		sub("winccoa/systems/System1/tags/MMQLive1", 1),
		sub("winccoa/systems/System1/types/Wrong/MMQLive1/speed", 1),
		sub("winccoa/systems/System1/types/MMQLiveTest/MMQLive1/nested/a", 1),
		sub("winccoa/systems/System1/tags/MMQLiveScalar", 0),
		sub("winccoa/systems/System1/tags/MMQLive1/speed/_online.._stime", 0),
	}
	want := []byte{0x01, 0x8F, 0x87, 0x83, 0x01, 0x9E, 0x01, 0x83, 0x8F, 0x8F, 0x01, 0x00, 0x00}
	for _, v := range []byte{5, 4} {
		c := e.client(t, fmt.Sprintf("live-suback-%d", v), v)
		got := c.Subscribe(filters...)
		w := append([]byte(nil), want...)
		if v == 4 {
			for i := range w {
				if w[i] > 2 {
					w[i] = 0x80
				}
			}
		}
		if !bytes.Equal(got, w) {
			t.Errorf("v%d SUBACK\n got % x\nwant % x", v, got, w)
		}
		c.Close()
	}
}

// AC-10/AC-22: initial value without a change, live values, aliases.
func TestLiveValues(t *testing.T) {
	e := live(t)
	e.set(t, "MMQLive1.speed", "12.5", "float")
	c := e.client(t, "live-values", 5)
	defer c.Close()
	tags, types := "winccoa/systems/System1/tags/MMQLive1/speed", "winccoa/systems/System1/types/MMQLiveTest/MMQLive1/speed"
	c.Subscribe(sub(tags, 1), sub(types, 1), sub("winccoa/systems/System1/tags/MMQLiveScalar", 1))
	init := c.NextOnAll(5*time.Second, tags, types, "winccoa/systems/System1/tags/MMQLiveScalar")
	if len(init) != 3 {
		t.Fatalf("initial values %v", init)
	}
	if payloadValue(t, init[tags]) != 12.5 {
		t.Fatalf("initial %s", init[tags].Payload)
	}
	start := time.Now()
	e.set(t, "MMQLive1.speed", "99.25", "float")
	live := c.NextOnAll(5*time.Second, tags, types)
	if len(live) != 2 || payloadValue(t, live[tags]) != 99.25 {
		t.Fatalf("live values %v", live)
	}
	t.Logf("woa set -> MQTT: %s", time.Since(start))
}

// AC-24/AC-26: typed writes applied in WinCC OA, rejections leave it unchanged.
func TestLiveWrites(t *testing.T) {
	e := live(t)
	c := e.client(t, "live-writes", 5)
	defer c.Close()
	c.Subscribe(sub("live/res", 1))
	// Command ids are deduplicated per client for 10 minutes.
	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	base := "winccoa/systems/System1/tags/MMQLive2/"
	cases := []struct {
		elem, payload string
		code          byte
		check         string
	}{
		{"speed", `{"value":1500.5,"id":"` + runID + `"}`, 0, "1500.5"},
		{"running", `true`, 0, "True"},
		{"name", `"über"`, 0, "über"},
		{"count", `-7`, 0, "-7"},
		{"unsigned", `4000000000`, 0, "4000000000"},
		{"nested/a", `2.5`, 0, "2.5"},
		{"count", `1.5`, 0x99, ""},
		{"running", `"yes"`, 0x99, ""},
		{"nope", `1`, 0x90, ""},
		{"nested", `1`, 0x90, ""},
	}
	for _, tc := range cases {
		code := c.Publish(rawPub{Topic: base + tc.elem + "/set", Payload: []byte(tc.payload), QoS: 1})
		if code != tc.code {
			t.Errorf("%s %s: PUBACK 0x%02x want 0x%02x", tc.elem, tc.payload, code, tc.code)
			continue
		}
		if tc.check == "" {
			continue
		}
		time.Sleep(300 * time.Millisecond)
		got := e.get(t, "MMQLive2."+strings.ReplaceAll(tc.elem, "/", "."))
		if !strings.EqualFold(got, tc.check) && got != tc.check+".0" {
			t.Errorf("%s: OA value %q want %q", tc.elem, got, tc.check)
		}
	}
	code := c.Publish(rawPub{Topic: base + "speed/set", Payload: []byte(`{"value":3,"id":"r` + runID + `"}`), QoS: 1, ResponseTopic: "live/res"})
	if code != 0 {
		t.Fatalf("PUBACK 0x%02x", code)
	}
	pk, ok := c.NextOn("live/res", 5*time.Second)
	if !ok || !strings.Contains(string(pk.Payload), `"confirmed"`) {
		t.Fatalf("result %s", pk.Payload)
	}
	if code := c.Publish(rawPub{Topic: base + "speed/set", Payload: []byte(`1`), QoS: 1, Retain: true}); code != 0x83 {
		t.Errorf("retained command: 0x%02x", code)
	}
}

func (e liveEnv) graphql(t *testing.T, query string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query})
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/graphql", e.gql), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if errs, ok := out["errors"]; ok {
		t.Fatalf("graphql errors: %v", errs)
	}
	return out
}

// lastStats returns the fields of the manager's latest statistics line
// (statsSeconds must be small; MMQ_LIVE_LOG is PVSS_II.log of the project).
func (e liveEnv) lastStats(t *testing.T, after time.Time) map[string]int {
	t.Helper()
	path := os.Getenv("MMQ_LIVE_LOG")
	if path == "" {
		t.Skip("MMQ_LIVE_LOG not set")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		lines := strings.Split(string(data), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			l := lines[i]
			j := strings.Index(l, "stats connects=")
			if j < 0 || !strings.Contains(l, "WCCOAmmq") || (e.tag != "" && !strings.Contains(l, e.tag)) {
				continue
			}
			ts, err := time.ParseInLocation("2006.01.02 15:04:05.000", strings.TrimSpace(strings.Split(strings.SplitN(l, "),", 2)[1], ",")[0]), time.Local)
			if err != nil || ts.Before(after) {
				break
			}
			out := map[string]int{}
			for _, f := range strings.Fields(l[j+len("stats "):]) {
				k, v, _ := strings.Cut(f, "=")
				n, _ := strconv.Atoi(v)
				out[k] = n
			}
			return out
		}
		time.Sleep(time.Second)
	}
	t.Fatal("no fresh statistics line")
	return nil
}

func (e liveEnv) ctrl(t *testing.T, script string) {
	t.Helper()
	out, err := exec.Command("/opt/WinCC_OA/3.21/bin/WCCOActrl", "-proj", e.project, "-num", "78", script).CombinedOutput()
	if err != nil {
		t.Fatalf("WCCOActrl %s: %v %s", script, err, out)
	}
}

// AC-06/AC-09/AC-21: shared registrations, exact release, no growth, all
// OA calls on the manager thread.
func TestLiveNoGrowth(t *testing.T) {
	e := live(t)
	// Registrations restored from other persisted sessions are the baseline.
	time.Sleep(6 * time.Second)
	base := e.lastStats(t, time.Now().Add(-5*time.Second))
	t.Logf("baseline: %v", base)
	for i := 0; i < 100; i++ {
		a := e.client(t, fmt.Sprintf("grow-a-%d", i%3), 5)
		b := e.client(t, fmt.Sprintf("grow-b-%d", i%3), 4)
		a.Subscribe(sub("winccoa/systems/System1/tags/MMQLive1/speed", 1), sub("winccoa/systems/System1/types/MMQLiveTest/MMQLive1/count", 0))
		b.Subscribe(sub("winccoa/systems/System1/tags/MMQLive1/speed", 0), sub("winccoa/systems/System1/tags/MMQLive1/speed/_online.._value", 0))
		if i == 50 {
			time.Sleep(6 * time.Second)
			st := e.lastStats(t, time.Now().Add(-4*time.Second))
			t.Logf("while subscribed: %v", st)
			if d := st["connects"] - base["connects"]; d < 1 || d > 2 {
				t.Errorf("expected shared registrations for 2 elements, got %v", st)
			}
		}
		a.Unsubscribe("winccoa/systems/System1/tags/MMQLive1/speed")
		a.Close()
		b.Close()
	}
	time.Sleep(7 * time.Second)
	st := e.lastStats(t, time.Now().Add(-5*time.Second))
	t.Logf("after cycles: %v", st)
	if st["connects"] != base["connects"] || st["queries"] != base["queries"] || st["liveCallbacks"] > base["liveCallbacks"] || st["offThreadCalls"] != 0 {
		t.Fatalf("registrations or callbacks left: %v", st)
	}
}

// AC-23: a deleted datapoint is invalidated and never replayed; recreation
// restores the interest.
func TestLiveDeleteRecreate(t *testing.T) {
	e := live(t)
	topic := "winccoa/systems/System1/tags/MMQLive2/speed"
	c := e.client(t, "live-del", 5)
	defer c.Close()
	c.Subscribe(sub(topic, 1))
	if _, ok := c.NextOn(topic, 5*time.Second); !ok {
		t.Fatal("no initial value")
	}
	e.ctrl(t, "mmqLiveDelete.ctl")
	time.Sleep(2 * time.Second)
	d := e.client(t, "live-del2", 5)
	defer d.Close()
	if got := d.Subscribe(sub(topic, 1)); got[0] != 0x8F {
		t.Errorf("subscribe to a deleted DP: 0x%02x", got[0])
	}
	if code := d.Publish(rawPub{Topic: topic + "/set", Payload: []byte(`1`), QoS: 1}); code != 0x90 {
		t.Errorf("write to a deleted DP: 0x%02x", code)
	}
	e.ctrl(t, "mmqLiveFixture.ctl")
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if pk, ok := c.NextOn(topic, 2*time.Second); ok {
			t.Logf("after recreation: %s", pk.Payload)
			return
		}
	}
	t.Fatal("interest not restored after recreation")
}

// AC-14/AC-17: device configuration and sessions live in MMQConfigs /
// MMQSessions datapoints and survive a manager restart, including a
// queued native change for an offline persistent session.
// MMQ_LIVE_RESTART is a command that restarts the manager.
func TestLiveStoresRestart(t *testing.T) {
	e := live(t)
	restart := os.Getenv("MMQ_LIVE_RESTART")
	if restart == "" {
		t.Skip("MMQ_LIVE_RESTART not set")
	}
	e.graphql(t, `mutation { winCCOaDevice { delete(name: "keep") } }`)
	e.graphql(t, fmt.Sprintf(`mutation { winCCOaDevice { create(input: {name: "keep", namespace: "keep", nodeId: %q, config: {addresses: [{query: "SELECT '_online.._value' FROM 'MMQLive1.speed'", topic: "k", answer: true}]}}) { success errors } } }`, e.node))
	defer e.graphql(t, `mutation { winCCOaDevice { delete(name: "keep") } }`)

	topic := "winccoa/systems/System1/tags/MMQLive1/count"
	c, _ := dialRaw(t, e.port, rawConnect{ClientID: "keep-sess", Version: 5, Clean: true, SessionExpiry: 3600})
	c.Subscribe(sub(topic, 1), sub("keep/plain", 1))
	c.NextOn(topic, 5*time.Second)
	c.Close()
	time.Sleep(500 * time.Millisecond)
	e.set(t, "MMQLive1.count", "4242", "int")
	time.Sleep(time.Second)

	for _, p := range []string{"MMQConfigs_*", "MMQSessions_*"} {
		out := e.woa(t, "names", p)
		t.Logf("%s: %s", p, strings.TrimSpace(out))
		if strings.HasPrefix(strings.TrimSpace(out), "0 datapoint") {
			t.Fatalf("no %s datapoints", p)
		}
	}
	out, err := exec.Command("bash", "-c", restart).CombinedOutput()
	if err != nil {
		t.Fatalf("restart: %v %s", err, out)
	}
	t.Logf("restart: %s", strings.TrimSpace(string(out)))

	res := e.graphql(t, `{ winCCOaClients(name: "keep") { name } }`)
	if b, _ := json.Marshal(res); !strings.Contains(string(b), `"keep"`) {
		t.Fatalf("device lost across restart: %s", b)
	}
	r, ack := dialRaw(t, e.port, rawConnect{ClientID: "keep-sess", Version: 5, Clean: false, SessionExpiry: 3600})
	defer r.Close()
	if !ack.SessionPresent {
		t.Fatal("session not present after restart")
	}
	pk, ok := r.NextOn(topic, 5*time.Second)
	if !ok || payloadValue(t, pk) != float64(4242) {
		t.Fatalf("queued native change after restart: %v %s", ok, pk.Payload)
	}
	pub := e.client(t, "keep-pub", 5)
	defer pub.Close()
	pub.Publish(rawPub{Topic: "keep/plain", Payload: []byte("x"), QoS: 1})
	if _, ok := r.NextOn("keep/plain", 3*time.Second); !ok {
		t.Fatal("restored subscription inactive")
	}
	// Clean up the persistent session.
	r2, _ := dialRaw(t, e.port, rawConnect{ClientID: "keep-sess", Version: 5, Clean: true})
	r2.Close()
}

// AC-18: export the MMQConfigs/MMQSessions datapoints, delete them, import
// the export and verify device configuration, session and subscriptions.
// MMQ_LIVE_STOP / MMQ_LIVE_START control the manager; MMQ_LIVE_ASCII is the
// ASCII manager of the project (WCCOAasciiSQLite for SQLite projects).
func TestLiveBackupRestore(t *testing.T) {
	e := live(t)
	stop, start, ascii := os.Getenv("MMQ_LIVE_STOP"), os.Getenv("MMQ_LIVE_START"), os.Getenv("MMQ_LIVE_ASCII")
	if stop == "" || start == "" || ascii == "" {
		t.Skip("MMQ_LIVE_STOP, MMQ_LIVE_START and MMQ_LIVE_ASCII needed")
	}
	sh := func(cmd string) string {
		out, err := exec.Command("bash", "-c", cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		return string(out)
	}
	e.graphql(t, `mutation { winCCOaDevice { delete(name: "bk2") } }`)
	e.graphql(t, fmt.Sprintf(`mutation { winCCOaDevice { create(input: {name: "bk2", namespace: "bk2", nodeId: %q, config: {messageFormat: JSON_MS, addresses: [{query: "SELECT '_online.._value' FROM 'MMQLive1.count'", topic: "b", answer: true, retained: true}]}}) { success } } }`, e.node))
	topic := "winccoa/systems/System1/types/MMQLiveTest/MMQLive1/speed"
	c, _ := dialRaw(t, e.port, rawConnect{ClientID: "bk-sess", Version: 5, Clean: true, SessionExpiry: 7200})
	c.Subscribe(packets.Subscription{Filter: topic, Qos: 1, RetainHandling: 1}, sub("bk/plain", 0))
	c.Close()
	time.Sleep(time.Second)

	dump := t.TempDir() + "/mmq-backup.dpl"
	sh(fmt.Sprintf("cd /tmp && %s -proj %s -num 79 -filter DO -filterDpType MMQConfigs -filterDpType MMQSessions -out %s", ascii, e.project, dump))
	sh(stop)
	e.ctrl(t, "mmqDeleteStores.ctl")
	if out := e.woa(t, "names", "MMQConfigs_*"); !strings.HasPrefix(strings.TrimSpace(out), "0 datapoint") {
		t.Fatalf("stores not deleted: %s", out)
	}
	sh(fmt.Sprintf("cd /tmp && %s -proj %s -num 79 -yes -in %s", ascii, e.project, dump))
	t.Logf("restored: %s / %s", strings.TrimSpace(e.woa(t, "names", "MMQConfigs_*")), strings.TrimSpace(e.woa(t, "names", "MMQSessions_*")))
	sh(start)

	res := e.graphql(t, `{ winCCOaClients(name: "bk2") { name namespace config { messageFormat addresses { query topic answer retained } } } }`)
	b, _ := json.Marshal(res)
	t.Logf("device after restore: %s", b)
	for _, want := range []string{`"JSON_MS"`, `"retained":true`, `MMQLive1.count`, `"namespace":"bk2"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("restored device lacks %s: %s", want, b)
		}
	}
	r, ack := dialRaw(t, e.port, rawConnect{ClientID: "bk-sess", Version: 5, Clean: false, SessionExpiry: 7200})
	if !ack.SessionPresent {
		t.Fatal("restored session not present")
	}
	e.set(t, "MMQLive1.speed", "31.5", "float")
	if pk, ok := r.NextOn(topic, 5*time.Second); !ok || payloadValue(t, pk) != 31.5 {
		t.Fatalf("restored native subscription inactive: %s", pk.Payload)
	}
	r.Close()
	r2, _ := dialRaw(t, e.port, rawConnect{ClientID: "bk-sess", Version: 5, Clean: true})
	r2.Close()
	e.graphql(t, `mutation { winCCOaDevice { delete(name: "bk2") } }`)
	e.graphql(t, `mutation { winCCOaDevice { delete(name: "bk1") } }`)
}

// AC-07: a burst beyond the host queue capacity is rejected with bounded
// memory; every accepted command reports an outcome; the manager recovers.
// MMQ_LIVE_OVL_PORT is a second manager with queueCapacity = 16 and
// MMQ_LIVE_OVL_NUM its manager number.
func TestLiveOverload(t *testing.T) {
	e := live(t)
	p, _ := strconv.Atoi(os.Getenv("MMQ_LIVE_OVL_PORT"))
	if p == 0 {
		t.Skip("MMQ_LIVE_OVL_PORT not set")
	}
	e.port = p
	e.tag = "(" + os.Getenv("MMQ_LIVE_OVL_NUM") + ")"
	c := e.client(t, "ovl", 5)
	defer c.Close()
	c.Subscribe(sub("ovl/res", 1))
	const n = 1000
	for i := 0; i < n; i++ {
		c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/MMQLive2/count/set", Payload: []byte(fmt.Sprintf(`{"value":%d,"id":"o%d-%d","replyTo":"ovl/res"}`, i, time.Now().UnixNano(), i)), QoS: 0})
	}
	status := map[string]int{}
	deadline := time.Now().Add(30 * time.Second)
	got := 0
	for got < n && time.Now().Before(deadline) {
		pk, ok := c.NextOn("ovl/res", time.Until(deadline))
		if !ok {
			break
		}
		var r map[string]any
		_ = json.Unmarshal(pk.Payload, &r)
		status[fmt.Sprint(r["status"])]++
		got++
	}
	t.Logf("results %d/%d: %v", got, n, status)
	if got != n {
		t.Fatalf("missing results: %d of %d", got, n)
	}
	time.Sleep(6 * time.Second)
	st := e.lastStats(t, time.Now().Add(-5*time.Second))
	t.Logf("stats %v", st)
	if st["queueHighWater"] > 16 || st["offThreadCalls"] != 0 {
		t.Fatalf("unbounded or off-thread: %v", st)
	}
	if st["overloads"] == 0 && status["failed"] == 0 {
		t.Log("burst did not reach the queue limit")
	}
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/MMQLive2/count/set", Payload: []byte(`77`), QoS: 1}); code != 0 {
		t.Fatalf("after burst: PUBACK 0x%02x", code)
	}
	time.Sleep(500 * time.Millisecond)
	if v := e.get(t, "MMQLive2.count"); v != "77" {
		t.Fatalf("value after recovery %q", v)
	}
}

// Native wildcard filters on real WinCC OA queries.
func TestLiveWildcards(t *testing.T) {
	e := live(t)
	c := e.client(t, "live-wild", 5)
	defer c.Close()
	c.Subscribe(sub("winccoa/systems/System1/tags/MMQLive1/#", 1))
	got := topicList(collectTopics(c, 2*time.Second))
	t.Logf("tags/MMQLive1/#: %v", got)
	for _, want := range []string{"speed", "running", "name", "count", "unsigned", "ts", "nested/a"} {
		found := false
		for _, g := range got {
			if g == "winccoa/systems/System1/tags/MMQLive1/"+want {
				found = true
			}
		}
		if !found {
			t.Errorf("tags/MMQLive1/# lacks %s", want)
		}
	}
	d := e.client(t, "live-wild-d", 5)
	defer d.Close()
	d.Subscribe(sub("winccoa/systems/System1/types/MMQLiveTest/+/nested/#", 1), sub("winccoa/systems/System1/tags/+/speed", 1))
	got = topicList(collectTopics(d, 2*time.Second))
	t.Logf("types/+/nested/# and +/speed: %v", got)
	if strings.Join(got, ",") != "winccoa/systems/System1/tags/MMQLive1/speed,winccoa/systems/System1/tags/MMQLive2/speed,winccoa/systems/System1/types/MMQLiveTest/MMQLive1/nested/a,winccoa/systems/System1/types/MMQLiveTest/MMQLive2/nested/a" {
		t.Errorf("unexpected topics %v", got)
	}

	start := time.Now()
	e.set(t, "MMQLive1.speed", "44.5", "float")
	for name, cl := range map[string]*rawClient{"tags/MMQLive1/#": c, "tags/+/speed": d} {
		msgs := collectTopics(cl, time.Second)["winccoa/systems/System1/tags/MMQLive1/speed"]
		if len(msgs) != 1 || !strings.Contains(msgs[0], "44.5") {
			t.Errorf("%s: %v", name, msgs)
		}
	}
	t.Logf("change -> wildcard subscribers: %s", time.Since(start))

	big := e.client(t, "live-wild-big", 5)
	defer big.Close()
	t0 := time.Now()
	big.Subscribe(sub("winccoa/systems/System1/types/MMQLoad/#", 0))
	n := len(collectTopics(big, 5*time.Second))
	t.Logf("types/MMQLoad/#: %d elements (subscribe to last value %s)", n, time.Since(t0))
	if n != 5000 {
		t.Errorf("types/MMQLoad/# delivered %d of 5000", n)
	}

	root := e.client(t, "live-wild-root", 5)
	defer root.Close()
	root.Subscribe(sub("winccoa/systems/System1/tags/#", 0))
	all := collectTopics(root, 6*time.Second)
	t.Logf("tags/#: %d elements", len(all))
	for tp := range all {
		if strings.Contains(tp, "/tags/_") || strings.Contains(tp, "MMQConfigs_") || strings.Contains(tp, "MMQSessions_") {
			t.Fatalf("protected datapoint in tags/#: %s", tp)
		}
	}
	if _, ok := all["winccoa/systems/System1/tags/MMQLiveScalar"]; !ok {
		t.Error("scalar root missing from tags/#")
	}

	// A datapoint created after the subscription.
	e.ctrl(t, "mmqLiveDelete.ctl")
	time.Sleep(time.Second)
	e.ctrl(t, "mmqLiveFixture.ctl")
	time.Sleep(time.Second)
	d.Drain(300 * time.Millisecond)
	e.set(t, "MMQLive2.speed", "5.5", "float")
	msgs := collectTopics(d, 2*time.Second)["winccoa/systems/System1/tags/MMQLive2/speed"]
	t.Logf("recreated DP through tags/+/speed: %v", msgs)
}
