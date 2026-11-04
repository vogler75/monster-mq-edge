package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/oahost"
)

const unsDeviceFields = `success errors issues { severity path message } device { name enabled config { direction unsRoot allowWrite topicDefaults } status { running topics dpes published errors } }`

func TestUnsModelerAiRunControl(t *testing.T) {
	for _, action := range []string{"cancel", "retry-limit"} {
		t.Run(action, func(t *testing.T) {
			entered, cancelled, release := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				select {
				case entered <- struct{}{}:
				default:
				}
				if action == "cancel" {
					select {
					case <-r.Context().Done():
						cancelled <- struct{}{}
					case <-release:
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"schemaVersion\":"},"finish_reason":"stop"}]}`)
			}))
			t.Cleanup(model.Close)
			t.Cleanup(func() { close(release) })
			_, url := startNativeGraphQL(t, 27617, 28617, func(c *config.Config) {
				c.GraphQL.Address = "127.0.0.1"
				c.Features.GenAi, c.GenAI.Enabled = true, true
				c.GenAI.Assistant.Provider = "openai"
				c.GenAI.Assistant.Model = "mock"
				c.GenAI.Assistant.DocsPath = t.TempDir()
				c.GenAI.Providers.OpenAI.ApiKey = "test-token"
				c.GenAI.Providers.OpenAI.Endpoint = model.URL
			})
			created := gqlQuery(t, url, `mutation { unsModeler { create(input: { name: "ai", config: { direction: MQTT_TO_WINCCOA, oaPrefix: "UNS_", approvalMode: AUTO } }) { success errors } } }`, nil)["unsModeler"].(map[string]any)["create"].(map[string]any)
			if created["success"] != true {
				t.Fatal(created)
			}
			start := gqlQuery(t, url, `mutation { unsModeler { startAiRun(name: "ai", input: { topicFilters: ["plant/#"], topicData: [{topic: "plant/x", value: "1.5"}] }) { success errors } } }`, nil)["unsModeler"].(map[string]any)["startAiRun"].(map[string]any)
			if start["success"] != true {
				t.Fatal(start)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("AI provider request did not start")
			}
			wantStatus := "FAILED"
			if action == "cancel" {
				wantStatus = "CANCELLED"
				res := gqlQuery(t, url, `mutation { unsModeler { cancelAiRun(name: "ai") { success errors run { status } } } }`, nil)["unsModeler"].(map[string]any)["cancelAiRun"].(map[string]any)
				if res["success"] != true {
					t.Fatal(res)
				}
				select {
				case <-cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("Stop did not cancel the in-flight provider HTTP request")
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				dev := gqlQuery(t, url, `{ unsModelers(name: "ai") { draft applied aiRun { status error applied } } }`, nil)["unsModelers"].([]any)[0].(map[string]any)
				run := dev["aiRun"].(map[string]any)
				if run["status"] != "RUNNING" {
					if run["status"] != wantStatus || run["applied"] != false || dev["draft"] != nil || dev["applied"] != nil {
						t.Fatalf("run outcome: %v", dev)
					}
					if action == "retry-limit" && (calls.Load() != 4 || !strings.Contains(fmt.Sprint(run["error"]), "after 4 attempts")) {
						t.Fatalf("retry limit: %d requests, %v", calls.Load(), run)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("AI run did not finish: %v", run)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func unsCreate(t *testing.T, url, token, name, unsRoot string, extra string) map[string]any {
	t.Helper()
	q := fmt.Sprintf(`mutation { unsModeler { create(input: { name: %q, config: { direction: WINCCOA_TO_UNS, unsRoot: %q %s } }) { %s } } }`, name, unsRoot, extra, unsDeviceFields)
	return gqlQueryAuth(t, url, q, nil, token)["unsModeler"].(map[string]any)["create"].(map[string]any)
}

func unsSaveDraft(t *testing.T, url, token, name, doc string) map[string]any {
	t.Helper()
	return gqlQueryAuth(t, url, `mutation($n: String!, $d: String!) { unsModeler { saveDraft(name: $n, document: $d) { success stored document errors issues { severity path message } } } }`,
		map[string]any{"n": name, "d": doc}, token)["unsModeler"].(map[string]any)["saveDraft"].(map[string]any)
}

func unsApply(t *testing.T, url, token, name string) map[string]any {
	t.Helper()
	return gqlQueryAuth(t, url, `mutation($n: String!) { unsModeler { apply(name: $n) { success stored errors issues { severity path message } } } }`,
		map[string]any{"n": name}, token)["unsModeler"].(map[string]any)["apply"].(map[string]any)
}

type unsSink struct{ ch chan plMsg }

func newUnsSink() *unsSink { return &unsSink{ch: make(chan plMsg, 256)} }

func (s *unsSink) handler(_ paho.Client, m paho.Message) {
	s.ch <- plMsg{m.Topic(), string(m.Payload()), m.Qos(), m.Retained()}
}

func (s *unsSink) waitValue(t *testing.T, topic string, value any) plMsg {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-s.ch:
			if m.topic != topic {
				continue
			}
			var p map[string]any
			if json.Unmarshal([]byte(m.payload), &p) == nil && p["value"] == value {
				return m
			}
		case <-deadline:
			t.Fatalf("no value %v on %s", value, topic)
		}
	}
}

func (s *unsSink) none(t *testing.T, topic string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-s.ch:
			if m.topic == topic {
				t.Fatalf("unexpected message %+v", m)
			}
		case <-deadline:
			return
		}
	}
}

const unsDocA = `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"name":"line1","children":[
	{"name":"speed","binding":{"kind":"element","type":"AnalogDrive","datapoint":"Pump101","element":"speed"}},
	{"name":"pump1","binding":{"kind":"datapoint","type":"AnalogDrive","datapoint":"Pump1","elements":[{"path":"speed"},{"path":"nested.a"}]}},
	{"name":"drives","binding":{"kind":"pattern","type":"AnalogDrive","pattern":"Pump*","elements":[{"path":"count"}]}}]}]}`

// AC-06, AC-07, AC-09, AC-12 (flag), AC-13, AC-14 against the simulated host.
func TestUnsModelerDirectionA(t *testing.T) {
	env, url := startNativeGraphQL(t, 27613, 28613, nil)
	tok := ""
	// Devices: three at the same time, two with overlapping roots.
	for _, d := range []struct{ name, root string }{{"uns1", "acme/hall2"}, {"uns2", "acme"}, {"uns3", "acme/hall2"}} {
		if res := unsCreate(t, url, tok, d.name, d.root, ""); res["success"] != true {
			t.Fatalf("create %s: %v", d.name, res)
		}
	}
	if res := unsCreate(t, url, tok, "bad", "winccoa/x", ""); res["success"] == true || !strings.Contains(fmt.Sprint(res["issues"]), "native") {
		t.Fatalf("native root accepted: %v", res)
	}
	if res := unsCreate(t, url, tok, "bad2", "ok", `, topicDefaults: "{\"qos\":5}"`); res["success"] == true {
		t.Fatalf("bad topicDefaults accepted: %v", res)
	}
	upd := gqlQueryAuth(t, url, `mutation { unsModeler { update(name: "uns1", input: { name: "uns1", config: { direction: MQTT_TO_WINCCOA, oaPrefix: "UNS_" } }) { success errors } } }`, nil, tok)
	if fmt.Sprint(upd) == "" || upd["unsModeler"].(map[string]any)["update"].(map[string]any)["success"] == true {
		t.Fatalf("direction change accepted: %v", upd)
	}

	// Drafts: non-JSON rejected, schema-invalid stored, ids assigned.
	if res := unsSaveDraft(t, url, tok, "uns1", `{"tree":`); res["stored"] == true {
		t.Fatalf("broken JSON stored: %v", res)
	}
	res := unsSaveDraft(t, url, tok, "uns1", `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"name":"a","binding":{"kind":"element"}}]}`)
	if res["stored"] != true || len(res["issues"].([]any)) == 0 {
		t.Fatalf("invalid draft: %v", res)
	}
	if !strings.Contains(res["document"].(string), `"id":"n`) {
		t.Fatalf("no node id: %v", res["document"])
	}
	if a := unsApply(t, url, tok, "uns1"); a["success"] == true {
		t.Fatalf("draft with errors applied: %v", a)
	}

	// Apply a valid mapping and receive values over MQTT.
	c := plPaho(t, env.port, "uns-sub")
	sink := newUnsSink()
	if tk := c.Subscribe("acme/#", 1, sink.handler); !tk.WaitTimeout(3*time.Second) || tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	if res := unsSaveDraft(t, url, tok, "uns1", unsDocA); res["success"] != true {
		t.Fatalf("save: %v", res)
	}
	if a := unsApply(t, url, tok, "uns1"); a["success"] != true {
		t.Fatalf("apply: %v", a)
	}
	set := func(dpe string, v oahost.Value) {
		t.Helper()
		if err := env.sim.Set(dpe+":_original.._value", v); err != nil {
			t.Fatal(err)
		}
	}
	set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 12})
	sink.waitValue(t, "acme/hall2/line1/speed", 12.0)
	set("System1:Pump1.nested.a", oahost.Value{Kind: oahost.KindFloat, Float: 3})
	sink.waitValue(t, "acme/hall2/line1/pump1/nested/a", 3.0)
	set("System1:Pump101.count", oahost.Value{Kind: oahost.KindInt, Int: 5})
	sink.waitValue(t, "acme/hall2/line1/drives/Pump101/count", 5.0)

	// Write-through is off by default: ..set is rejected, nothing written.
	pc := plPaho(t, env.port, "uns-pub")
	plPahoPublish(t, pc, "acme/hall2/line1/speed/..set", 0, false, `99`)
	time.Sleep(200 * time.Millisecond)
	if v, _ := env.sim.Get("System1:Pump101.speed:_online.._value"); v.Float == 99 {
		t.Fatal("..set written without allowWrite")
	}
	upd = gqlQueryAuth(t, url, `mutation { unsModeler { update(name: "uns1", input: { name: "uns1", config: { direction: WINCCOA_TO_UNS, allowWrite: true } }) { success errors } } }`, nil, tok)
	if upd["unsModeler"].(map[string]any)["update"].(map[string]any)["success"] != true {
		t.Fatalf("enable write: %v", upd)
	}
	time.Sleep(300 * time.Millisecond)
	plPahoPublish(t, pc, "acme/hall2/line1/speed/..set", 1, false, `{"value": 77}`)
	sink.waitValue(t, "acme/hall2/line1/speed", 77.0)

	// Lifecycle: disable stops publishing and releases the connections.
	before := env.sim.Connections()
	tg := gqlQueryAuth(t, url, `mutation { unsModeler { toggle(name: "uns1", enabled: false) { success } } }`, nil, tok)
	if tg["unsModeler"].(map[string]any)["toggle"].(map[string]any)["success"] != true {
		t.Fatal(tg)
	}
	time.Sleep(200 * time.Millisecond)
	if after := env.sim.Connections(); after >= before {
		t.Fatalf("connections not released: %d -> %d", before, after)
	}
	set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 13})
	sink.none(t, "acme/hall2/line1/speed", 300*time.Millisecond)

	// Re-enable resumes the applied mapping without apply.
	gqlQueryAuth(t, url, `mutation { unsModeler { toggle(name: "uns1", enabled: true) { success } } }`, nil, tok)
	set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 14})
	sink.waitValue(t, "acme/hall2/line1/speed", 14.0)

	// Status, list and $SYS counters.
	list := gqlQueryAuth(t, url, `{ unsModelers(name: "uns1") { name draft applied status { running topics published } liveTopics { topic value published } } }`, nil, tok)["unsModelers"].([]any)
	dev := list[0].(map[string]any)
	st := dev["status"].(map[string]any)
	if st["running"] != true || st["topics"].(float64) < 4 || dev["applied"] == nil || len(dev["liveTopics"].([]any)) < 4 {
		t.Fatalf("device %v", dev)
	}
	sys := newUnsSink()
	if tk := c.Subscribe("$SYS/unsmodeler/uns1/#", 0, sys.handler); !tk.WaitTimeout(3*time.Second) || tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	select {
	case <-sys.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no $SYS/unsmodeler counters")
	}

	// Delete removes the device record and with it both documents.
	del := gqlQueryAuth(t, url, `mutation { unsModeler { delete(name: "uns1") } }`, nil, tok)
	if del["unsModeler"].(map[string]any)["delete"] != true {
		t.Fatal(del)
	}
	if d, _ := env.srv.Storage().DeviceConfig.Get(t.Context(), "uns1"); d != nil {
		t.Fatal("device record kept after delete")
	}
}

// AC-14: after a broker restart the device resumes publishing, also when a
// bound datapoint was deleted meanwhile.
func TestUnsModelerResume(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	db := filepath.Join(t.TempDir(), "r.db")
	cfgFn := func(c *config.Config) {
		c.GraphQL.Enabled = true
		c.GraphQL.Port = 28614
		c.GraphQL.TLSPort = 29614
	}
	env := startNative(t, 27614, db, sim, client, cfgFn, broker.Options{})
	url := "http://localhost:28614/graphql"
	time.Sleep(200 * time.Millisecond)
	if res := unsCreate(t, url, "", "res", "plant", ""); res["success"] != true {
		t.Fatal(res)
	}
	unsSaveDraft(t, url, "", "res", `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[
		{"name":"a","binding":{"kind":"element","type":"AnalogDrive","datapoint":"Pump101","element":"speed"}},
		{"name":"b","binding":{"kind":"element","type":"AnalogDrive","datapoint":"Pump1","element":"speed"}}]}`)
	if a := unsApply(t, url, "", "res"); a["success"] != true {
		t.Fatal(a)
	}
	env.srv.Close()
	sim.DeleteDP("System1", "Pump1")

	env = startNative(t, 27614, db, sim, client, cfgFn, broker.Options{})
	defer env.srv.Close()
	c := plPaho(t, env.port, "res-sub")
	sink := newUnsSink()
	if tk := c.Subscribe("plant/#", 1, sink.handler); !tk.WaitTimeout(3*time.Second) || tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	if err := sim.Set("System1:Pump101.speed:_original.._value", oahost.Value{Kind: oahost.KindFloat, Float: 21}); err != nil {
		t.Fatal(err)
	}
	sink.waitValue(t, "plant/a", 21.0)
}

// AC-12: a client without publish permission cannot write through ..set.
func TestUnsModelerWriteACL(t *testing.T) {
	env, url := startNativeGraphQL(t, 27615, 28615, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
	})
	admin := loginToken(t, url, "Admin", "Admin")
	if res := unsCreate(t, url, admin, "acl", "plant", ", allowWrite: true"); res["success"] != true {
		t.Fatal(res)
	}
	unsSaveDraft(t, url, admin, "acl", `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"name":"a","binding":{"kind":"element","type":"AnalogDrive","datapoint":"Pump101","element":"speed"}}]}`)
	if a := unsApply(t, url, admin, "acl"); a["success"] != true {
		t.Fatal(a)
	}
	gqlQueryAuth(t, url, `mutation { user { createUser(input: { username: "ro", password: "pw", canSubscribe: true, canPublish: true }) { success } } }`, nil, admin)
	gqlQueryAuth(t, url, `mutation { user { createAclRule(input: { username: "ro", topicPattern: "plant/#", canSubscribe: true, canPublish: false, priority: 1 }) { success } } }`, nil, admin)
	o := mqttOpts(env.port, "ro-client")
	o.SetUsername("ro")
	o.SetPassword("pw")
	c := paho.NewClient(o)
	if tk := c.Connect(); !tk.WaitTimeout(3*time.Second) || tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	defer c.Disconnect(50)
	c.Publish("plant/a/..set", 1, false, `55`).WaitTimeout(time.Second)
	time.Sleep(300 * time.Millisecond)
	if v, _ := env.sim.Get("System1:Pump101.speed:_online.._value"); v.Float == 55 {
		t.Fatal("write through without publish permission")
	}
	o2 := mqttOpts(env.port, "admin-client")
	o2.SetUsername("Admin")
	o2.SetPassword("Admin")
	c2 := paho.NewClient(o2)
	if tk := c2.Connect(); !tk.WaitTimeout(3*time.Second) || tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	defer c2.Disconnect(50)
	c2.Publish("plant/a/..set", 1, false, `56`).WaitTimeout(time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if v, _ := env.sim.Get("System1:Pump101.speed:_online.._value"); v.Float == 56 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admin write not executed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AC-28, AC-29, AC-31, AC-32: Direction B import, create and value flow
// through GraphQL and MQTT on the simulated host.
func TestUnsModelerDirectionB(t *testing.T) {
	env, url := startNativeGraphQL(t, 27616, 28616, nil)
	res := gqlQueryAuth(t, url, `mutation { unsModeler { create(input: { name: "onb", config: { direction: MQTT_TO_WINCCOA, oaPrefix: "UNS_" } }) { success errors issues { message } } } }`, nil, "")
	if res["unsModeler"].(map[string]any)["create"].(map[string]any)["success"] != true {
		t.Fatal(res)
	}
	cancel := gqlQueryAuth(t, url, `mutation { unsModeler { cancelAiRun(name: "onb") { success errors } } }`, nil, "")["unsModeler"].(map[string]any)["cancelAiRun"].(map[string]any)
	if cancel["success"] != false || !strings.Contains(fmt.Sprint(cancel["errors"]), "no AI run") {
		t.Fatalf("cancellation without an active run: %v", cancel)
	}
	pc := plPaho(t, env.port, "onb-pub")
	plPahoPublish(t, pc, "plant/p1/data", 1, true, `{"flow": 1.5}`)
	plPahoPublish(t, pc, "plant/p1/state", 1, true, `ON`)
	proposal := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","types":[{"name":"UNS_Pump","elements":[
		{"path":"flow","elementType":"float","unit":"m3/h","pvRange":{"min":0,"max":100},"archive":{"enabled":true,"group":"EVENT"}},{"path":"motor.state","elementType":"string"}]}],
		"datapoints":[{"name":"UNS_P1","type":"UNS_Pump","elements":[{"path":"motor.state","archive":{"enabled":true,"group":"EVENT"}}],"bindings":[{"element":"flow","topic":"plant/p1/data","jsonPath":"$.flow"},{"element":"motor.state","topic":"plant/p1/state"}]}]}`
	imp := gqlQueryAuth(t, url, `mutation($d: String!) { unsModeler { importProposal(name: "onb", document: $d, topicFilters: ["plant/#"]) { success stored document errors issues { severity message } } } }`,
		map[string]any{"d": proposal}, "")["unsModeler"].(map[string]any)["importProposal"].(map[string]any)
	if imp["success"] != true || !strings.Contains(imp["document"].(string), `"topicFilters":["plant/#"]`) {
		t.Fatalf("import %v", imp)
	}
	plan := gqlQueryAuth(t, url, `{ unsModelers(name: "onb") { createPlan { types datapoints configWrites } } }`, nil, "")["unsModelers"].([]any)[0].(map[string]any)["createPlan"].(map[string]any)
	if plan["types"].(float64) != 1 || plan["datapoints"].(float64) != 1 || plan["configWrites"].(float64) != 2 {
		t.Fatalf("plan %v", plan)
	}
	cr := gqlQueryAuth(t, url, `mutation { unsModeler { createInWinCCOa(name: "onb") { success errors objects { kind name status message } configWrites } } }`, nil, "")["unsModeler"].(map[string]any)["createInWinCCOa"].(map[string]any)
	if cr["success"] != true || cr["configWrites"].(float64) != 2 {
		t.Fatalf("create %v", cr)
	}
	// No driver address config is created.
	if v, _ := env.sim.Get("System1:UNS_P1.flow:_address.._type"); v.Kind != oahost.KindNull {
		t.Fatalf("address config created: %+v", v)
	}
	for _, path := range []string{"flow", "motor.state"} {
		if v, err := env.sim.Get("System1:UNS_P1." + path + ":_archive.._type"); err != nil || v.Kind != oahost.KindNull {
			t.Fatalf("archiving disabled, but %s has archive config: %+v (%v)", path, v, err)
		}
	}
	plPahoPublish(t, pc, "plant/p1/data", 1, false, `{"flow": 42}`)
	plPahoPublish(t, pc, "plant/p1/state", 1, false, `RUNNING`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, _ := env.sim.Get("System1:UNS_P1.flow:_online.._value")
		s, _ := env.sim.Get("System1:UNS_P1.motor.state:_online.._value")
		if f.Float == 42 && s.Str == "RUNNING" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flow: %+v %+v", f, s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A conflicting type blocks the create.
	conflict := strings.Replace(proposal, `{"path":"motor.state","elementType":"string"}`, `{"path":"motor.state","elementType":"bool"}`, 1)
	conflict = strings.Replace(conflict, "UNS_P1", "UNS_P2", 1)
	gqlQueryAuth(t, url, `mutation($d: String!) { unsModeler { saveDraft(name: "onb", document: $d) { stored } } }`, map[string]any{"d": strings.Replace(conflict, `"types"`, `"source":{"mode":"live","topicFilters":["plant/#"]},"types"`, 1)}, "")
	cr = gqlQueryAuth(t, url, `mutation { unsModeler { createInWinCCOa(name: "onb") { success issues { severity message } objects { name } } } }`, nil, "")["unsModeler"].(map[string]any)["createInWinCCOa"].(map[string]any)
	if cr["success"] == true || len(cr["objects"].([]any)) != 0 || !strings.Contains(fmt.Sprint(cr["issues"]), "different structure") {
		t.Fatalf("conflict not blocking: %v", cr)
	}
}
