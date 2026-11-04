package unsmodeler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"monstermq.io/edge/internal/genai"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/oahost/simhost"
	"monstermq.io/edge/internal/stores"
)

func settingsB() *Settings {
	s := &Settings{Direction: DirectionMqttToWinccoa, OaPrefix: "UNS_"}
	s.applyDefaults()
	return s
}

func addAlertClasses(t *testing.T, sim *simhost.Host) {
	t.Helper()
	sim.AddType(simhost.Type{Name: "_AlertClass", Elements: map[string]uint32{"": simhost.ElemStruct, "prior": uint32(oahost.KindUint)}})
	for _, c := range []string{"alert", "warning"} {
		if err := sim.CreateDP("System1", c, "_AlertClass"); err != nil {
			t.Fatal(err)
		}
	}
}

const docB = `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},
 "types":[{"name":"UNS_Pump","elements":[
   {"path":"flow","elementType":"float","unit":"m³/h","description":"Outlet flow","format":"%.1f","pvRange":{"min":0,"max":200},
    "alert":{"kind":"analog","limits":[{"when":"above","limit":180,"class":"warning","text":"high"},{"when":"below","limit":5,"class":"alert","text":"dry"}]}},
   {"path":"state.running","elementType":"bool","alert":{"kind":"binary","class":"alert","text":"stopped","alarmValue":false}},
   {"path":"name","elementType":"string"}]}],
 "datapoints":[{"name":"UNS_P1","type":"UNS_Pump","elements":[{"path":"flow","description":"Pump 1 outlet flow"}],
   "bindings":[{"element":"flow","topic":"plant/p1/data","jsonPath":"$.flow"},{"element":"state.running","topic":"plant/p1/state"},{"element":"name","topic":"plant/p1/name"}]}]}`

func TestValidateBPasses(t *testing.T) {
	oa, sim := newSimOA(t)
	addAlertClasses(t, sim)
	res, err := Validate(context.Background(), []byte(docB), ValidationEnv{Settings: settingsB(), Roots: TopicRoots{Native: "winccoa"}, OA: oa})
	if err != nil {
		t.Fatal(err)
	}
	if HasErrors(res.Issues) {
		t.Fatalf("issues %v", res.Issues)
	}
}

// Failing cases of the Direction B checks with their severity (AC-08).
func TestValidateBChecks(t *testing.T) {
	oa, sim := newSimOA(t)
	addAlertClasses(t, sim)
	if err := sim.CreateDP("System1", "UNS_Old", "Scalar"); err != nil {
		t.Fatal(err)
	}
	type c struct {
		name, types, dps, sev, path, text string
		sample                            map[string]string
	}
	pump := `{"name":"UNS_Pump","elements":[{"path":"flow","elementType":"float"},{"path":"on","elementType":"bool"}]}`
	dp := func(bindings string) string {
		return `{"name":"UNS_P1","type":"UNS_Pump","bindings":[` + bindings + `]}`
	}
	cases := []c{
		{"duplicate type", pump + "," + pump, dp(`{"element":"flow","topic":"plant/a"}`), SeverityError, "/types/1/name", "used twice", nil},
		{"type not listed", pump, `{"name":"UNS_P1","type":"UNS_Other","bindings":[{"element":"flow","topic":"plant/a"}]}`, SeverityError, "/datapoints/0/type", "not listed", nil},
		{"missing element", pump, dp(`{"element":"nope","topic":"plant/a"}`), SeverityError, "/datapoints/0/bindings/0/element", "does not exist", nil},
		{"root of new type", pump, dp(`{"element":"","topic":"plant/a"}`), SeverityError, "/datapoints/0/bindings/0/element", "does not exist", nil},
		{"two bindings", pump, dp(`{"element":"flow","topic":"plant/a"},{"element":"flow","topic":"plant/b"}`), SeverityError, "/datapoints/0/bindings/1/element", "two bindings", nil},
		{"leaf and parent", `{"name":"UNS_Pump","elements":[{"path":"a","elementType":"float"},{"path":"a.b","elementType":"float"}]}`, dp(`{"element":"a","topic":"plant/a"}`), SeverityError, "/types/0/elements", "parent", nil},
		{"prefix", `{"name":"Pump","elements":[{"path":"flow","elementType":"float"}]}`, `{"name":"P1","type":"Pump","bindings":[{"element":"flow","topic":"plant/a"}]}`, SeverityError, "/types/0/name", "prefix", nil},
		{"range order", `{"name":"UNS_Pump","elements":[{"path":"flow","elementType":"float","pvRange":{"min":5,"max":1}}]}`, dp(`{"element":"flow","topic":"plant/a"}`), SeverityError, "/types/0/elements/0/pvRange", "above max", nil},
		{"limits order", `{"name":"UNS_Pump","elements":[{"path":"flow","elementType":"float","alert":{"kind":"analog","limits":[{"when":"below","limit":50,"class":"alert"},{"when":"above","limit":40,"class":"alert"}]}}]}`, dp(`{"element":"flow","topic":"plant/a"}`), SeverityError, "/types/0/elements/0/alert/limits", "good range", nil},
		{"unknown class", `{"name":"UNS_Pump","elements":[{"path":"on","elementType":"bool","alert":{"kind":"binary","class":"nosuch"}}]}`, dp(`{"element":"on","topic":"plant/a"}`), SeverityError, "/types/0/elements/0/alert/class", "does not exist", nil},
		{"dp meta fits", pump, `{"name":"UNS_P1","type":"UNS_Pump","elements":[{"path":"on","unit":"m"}],"bindings":[{"element":"on","topic":"plant/a"}]}`, SeverityError, "/datapoints/0/elements/0/unit", "not allowed", nil},
		{"metadata named element", `{"name":"UNS_Pump","elements":[{"path":"unit","elementType":"string"}]}`, dp(`{"element":"unit","topic":"plant/a"}`), SeverityWarning, "/types/0/elements/0/path", "named like metadata", nil},
		{"outside filters", pump, dp(`{"element":"flow","topic":"other/a"}`), SeverityWarning, "/datapoints/0/bindings/0/topic", "outside the analysed", nil},
		{"native topic", pump, dp(`{"element":"flow","topic":"winccoa/tags/x"}`), SeverityError, "/datapoints/0/bindings/0/topic", "native", nil},
		{"existing dp other type", pump, `{"name":"UNS_Old","type":"UNS_Pump","bindings":[{"element":"flow","topic":"plant/a"}]}`, SeverityError, "/datapoints/0/name", "exists with type Scalar", nil},
		{"new type name exists", `{"name":"PumpType","elements":[{"path":"flow","elementType":"float"}]}`, `{"name":"UNS_X","type":"PumpType","bindings":[{"element":"flow","topic":"plant/a"}]}`, SeverityError, "/types/0/name", "different structure", nil},
		{"reuse missing element", `{"name":"PumpType","reuse":true,"elements":[{"path":"nope","elementType":"float"}]}`, `{"name":"UNS_X","type":"PumpType","bindings":[{"element":"flow","topic":"plant/a"}]}`, SeverityError, "/types/0/elements/0/path", "does not exist", nil},
		{"reuse wrong type", `{"name":"PumpType","reuse":true,"elements":[{"path":"flow","elementType":"int"}]}`, `{"name":"UNS_X","type":"PumpType","bindings":[{"element":"flow","topic":"plant/a"}]}`, SeverityError, "/types/0/elements/0/elementType", "is float", nil},
		{"reuse unwritable element", `{"name":"PumpType","reuse":true}`, `{"name":"UNS_X","type":"PumpType","bindings":[{"element":"label","topic":"plant/a"}]}`, SeverityError, "/datapoints/0/bindings/0/element", "cannot be written", nil},
		{"sample type mismatch", pump, dp(`{"element":"on","topic":"plant/a"}`), SeverityError, "/datapoints/0/bindings/0/element", "does not fit", map[string]string{"plant/a": `"text"`}},
		{"sample jsonPath", pump, dp(`{"element":"flow","topic":"plant/a","jsonPath":"$.x"}`), SeverityWarning, "/datapoints/0/bindings/0/jsonPath", "does not resolve", map[string]string{"plant/a": `{"y":1}`}},
		{"sample timePath", pump, dp(`{"element":"flow","topic":"plant/a","jsonPath":"$.v","timePath":"$.ts","timeFormat":"epochMs"}`), SeverityWarning, "/datapoints/0/bindings/0/timePath", "does not resolve", map[string]string{"plant/a": `{"v":1}`}},
		{"sample time unreadable", pump, dp(`{"element":"flow","topic":"plant/a","jsonPath":"$.v","timePath":"$.ts","timeFormat":"iso8601"}`), SeverityError, "/datapoints/0/bindings/0/timeFormat", "cannot be read as iso8601", map[string]string{"plant/a": `{"v":1,"ts":1791964196247}`}},
		{"sample time seconds as ms", pump, dp(`{"element":"flow","topic":"plant/a","jsonPath":"$.v","timePath":"$.ts","timeFormat":"epochMs"}`), SeverityWarning, "/datapoints/0/bindings/0/timeFormat", "before 2000", map[string]string{"plant/a": `{"v":1,"ts":1791964196}`}},
		{"time format without path", pump, dp(`{"element":"flow","topic":"plant/a","timeFormat":"epochMs"}`), SeverityError, "/datapoints/0/bindings/0", "timePath", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},"types":[` + tc.types + `],"datapoints":[` + tc.dps + `]}`
			env := ValidationEnv{Settings: settingsB(), Roots: TopicRoots{Native: "winccoa"}, OA: oa}
			if tc.sample != nil {
				env.Sample = func(topic string) ([]byte, bool) {
					v, ok := tc.sample[topic]
					return []byte(v), ok
				}
			}
			res, err := Validate(context.Background(), []byte(raw), env)
			if err != nil {
				t.Fatal(err)
			}
			if !hasIssue(res.Issues, tc.sev, tc.path, tc.text) {
				t.Fatalf("want %s at %s containing %q, got %v", tc.sev, tc.path, tc.text, res.Issues)
			}
		})
	}
	// A reused type allows bindings to unlisted elements of the real type.
	raw := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},"types":[{"name":"PumpType","reuse":true,"elements":[{"path":"flow","elementType":"float"}]}],
		"datapoints":[{"name":"UNS_X","type":"PumpType","bindings":[{"element":"pressure","topic":"plant/a"}]}]}`
	res, _ := Validate(context.Background(), []byte(raw), ValidationEnv{Settings: settingsB(), Roots: TopicRoots{Native: "winccoa"}, OA: oa})
	if HasErrors(res.Issues) {
		t.Fatalf("reused unlisted element: %v", res.Issues)
	}
}

func TestPrepareBStamping(t *testing.T) {
	m, devs, docs, _ := newTestManager(t, AlwaysActive{})
	_ = devs.Save(context.Background(), stores.DeviceConfig{Name: "b", Type: DeviceType, Enabled: true, Config: settingsB().Encode()})
	draftRaw := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["old/#"]},
		"types":[{"name":"UNS_Keep","excluded":true,"elements":[{"path":"v","elementType":"float"}]},{"name":"UNS_T","elements":[{"path":"v","elementType":"float"}]}],
		"datapoints":[{"name":"UNS_Old","type":"UNS_T","bindings":[{"element":"v","topic":"old/x"}]}]}`
	_ = docs.Save(context.Background(), "b", SlotDraft, []byte(draftRaw))
	draft, _ := ParseDocument([]byte(draftRaw))
	s := settingsB()
	in := &TopicInput{Mode: "live", Filters: []string{"plant/#"}}
	// The LLM writes excluded:false, drops the excluded type and keeps the
	// out-of-filter datapoint unchanged.
	answer := "```json\n" + `{"$schema":"x","schemaVersion":1,"direction":"mqtt_to_winccoa","types":[{"name":"UNS_T","excluded":false,"elements":[{"path":"v","elementType":"float"}]}],
		"datapoints":[{"name":"UNS_Old","type":"UNS_T","bindings":[{"element":"v","topic":"old/x"}]},{"name":"UNS_New","type":"UNS_T","bindings":[{"element":"v","topic":"plant/y"}]}]}` + "\n```"
	p := m.prepareB(context.Background(), "b", s, answer, draft, in, AiRun{Provider: "p", Model: "m"})
	if HasErrors(p.Issues) {
		t.Fatalf("issues %v\n%s", p.Issues, p.Doc)
	}
	doc, _ := ParseDocument(p.Doc)
	if doc.Source == nil || strings.Join(doc.Source.TopicFilters, ",") != "old/#,plant/#" || doc.AiRun == nil {
		t.Fatalf("stamps %s", p.Doc)
	}
	if len(doc.Types) != 2 || doc.Types[1].Name != "UNS_Keep" || !doc.Types[1].Excluded || doc.Types[0].Excluded {
		t.Fatalf("excluded %s", p.Doc)
	}
	// Changing the out-of-filter datapoint or binding outside the filters
	// is an AI run context error.
	bad := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","types":[{"name":"UNS_T","elements":[{"path":"v","elementType":"float"}]}],
		"datapoints":[{"name":"UNS_New","type":"UNS_T","bindings":[{"element":"v","topic":"elsewhere/y"}]}]}`
	p = m.prepareB(context.Background(), "b", s, bad, draft, in, AiRun{Provider: "p", Model: "m"})
	if !hasIssue(p.Issues, SeverityError, "/datapoints/0/bindings/0/topic", "outside the run's topic filters") || !hasIssue(p.Issues, SeverityError, "/datapoints", "cannot be removed") {
		t.Fatalf("context %v", p.Issues)
	}
	// A document without source passes (PromptSchema view).
	p = m.prepareB(context.Background(), "b", s, `{"schemaVersion":1,"direction":"mqtt_to_winccoa","types":[],"datapoints":[]}`, nil, in, AiRun{Provider: "p", Model: "m"})
	if HasErrors(p.Issues) {
		t.Fatal(p.Issues)
	}
}

type fakeSub struct {
	mu  sync.Mutex
	fns map[string]func(string, []byte, time.Time)
}

func (f *fakeSub) Subscribe(filters []string, fn func(string, []byte, time.Time)) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fns == nil {
		f.fns = map[string]func(string, []byte, time.Time){}
	}
	for _, t := range filters {
		f.fns[t] = fn
	}
	return func() {
		f.mu.Lock()
		f.fns = nil
		f.mu.Unlock()
	}, nil
}

func (f *fakeSub) publish(topic, payload string) {
	f.mu.Lock()
	fn := f.fns[topic]
	f.mu.Unlock()
	if fn != nil {
		fn(topic, []byte(payload), time.Now())
	}
}

func TestCreateInWinCCOaAndFlow(t *testing.T) {
	oa, sim := newSimOA(t)
	addAlertClasses(t, sim)
	devs := &memDevices{m: map[string]stores.DeviceConfig{}}
	m := NewManager(devs, oa, newCapture().pub, AlwaysActive{}, TopicRoots{Native: "winccoa"}, "n1", slog.New(slog.DiscardHandler))
	docs := &memDocs{m: m}
	sub := &fakeSub{}
	m.SetSubscriber(sub)
	t.Cleanup(m.Stop)
	ctx := context.Background()
	_ = devs.Save(ctx, stores.DeviceConfig{Name: "b", Type: DeviceType, Enabled: true, NodeID: "n1", Config: settingsB().Encode()})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = docs.Save(ctx, "b", SlotDraft, []byte(docB))
	res, issues, err := m.CreateInWinCCOa(ctx, "b")
	if err != nil || HasErrors(issues) {
		t.Fatalf("create %v %v", err, issues)
	}
	if res.Failed != 0 || len(res.Objects) != 2 || res.Objects[0].Status != "created" || res.Objects[1].Status != "created" {
		t.Fatalf("result %+v", res)
	}
	// Type layout, metadata (datapoint override wins), range and alerts.
	if _, err := sim.Get("System1:UNS_P1.state.running:_online.._value"); err != nil {
		t.Fatalf("nested element: %v", err)
	}
	unit, format, desc, _ := sim.Meta("System1:UNS_P1.flow")
	if unit != "m³/h" || format != "%.1f" || desc != "Pump 1 outlet flow" {
		t.Fatalf("meta %q %q %q", unit, format, desc)
	}
	get := func(a string) oahost.Value {
		t.Helper()
		v, err := sim.Get("System1:UNS_P1." + a)
		if err != nil {
			t.Fatal(a, err)
		}
		return v
	}
	if get("flow:_pv_range.._type").Int != 7 || get("flow:_pv_range.._max").Float != 200 {
		t.Fatal("pv range")
	}
	// Ranges: <5 alert (dry), [5,180] good, >180 warning (high).
	if get("flow:_alert_hdl.._type").Int != 13 || get("flow:_alert_hdl.1._u_limit").Float != 5 || get("flow:_alert_hdl.1._u_incl").Bool ||
		get("flow:_alert_hdl.1._class").Str != "System1:alert." || get("flow:_alert_hdl.3._class").Str != "System1:warning." ||
		get("flow:_alert_hdl.2._u_incl").Bool != true || get("flow:_alert_hdl.3._text").Str != "high" {
		t.Fatal("analog alert")
	}
	if get("state.running:_alert_hdl.._type").Int != 12 || get("state.running:_alert_hdl.._ok_range").Bool != true || get("state.running:_alert_hdl.._text0").Str != "stopped" {
		t.Fatal("binary alert")
	}
	// A second create skips the existing datapoint and the type.
	res, _, _ = m.CreateInWinCCOa(ctx, "b")
	if res.Objects[0].Status != "skipped" || res.Objects[1].Status != "skipped" {
		t.Fatalf("second create %+v", res.Objects)
	}
	// Flow: jsonPath float, text payload, binary blob-free bool; conversion
	// errors are counted and do not stop the flow.
	sub.publish("plant/p1/data", `{"flow": 42.5}`)
	sub.publish("plant/p1/name", `ON`)
	sub.publish("plant/p1/state", `"yes"`)
	sub.publish("plant/p1/state", `true`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, _ := sim.Get("System1:UNS_P1.flow:_online.._value")
		n, _ := sim.Get("System1:UNS_P1.name:_online.._value")
		r, _ := sim.Get("System1:UNS_P1.state.running:_online.._value")
		if f.Float == 42.5 && n.Str == "ON" && r.Bool {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flow values %+v %+v %+v", f, n, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := m.Status("b"); st.Errors < 1 || st.Writes < 3 {
		t.Fatalf("counters %+v", st)
	}
	// The live monitor shows the last written value and its time.
	var flow *TopicStatus
	for _, ts := range m.LiveTopics("b") {
		if ts.DPE == "System1:UNS_P1.flow" {
			flow = &ts
		}
	}
	if flow == nil || !flow.HasValue || flow.Value != 42.5 || flow.LastPublish.IsZero() || flow.Published != 1 {
		t.Fatalf("live topic %+v", flow)
	}
}

func TestConvertPayload(t *testing.T) {
	cases := []struct {
		payload, path string
		kind          oahost.Kind
		want          string
		err           bool
	}{
		{`{"a":{"b":[1,2.5]}}`, `$.a.b[1]`, oahost.KindFloat, "2.5", false},
		{`{"motor current":3}`, `$['motor current']`, oahost.KindInt, "3", false},
		{`running`, ``, oahost.KindString, `"running"`, false},
		{`"quoted"`, ``, oahost.KindString, `"quoted"`, false},
		{`12.5`, ``, oahost.KindFloat, "12.5", false},
		{`{"value": 1}`, ``, oahost.KindFloat, "", true},
		{`true`, ``, oahost.KindInt, "", true},
		{`{"a":1}`, `$.b`, oahost.KindInt, "", true},
	}
	for _, c := range cases {
		v, err := convertPayload([]byte(c.payload), c.path, c.kind)
		if (err != nil) != c.err {
			t.Fatalf("%+v: err %v", c, err)
		}
		if !c.err {
			got, _ := json.Marshal(v.PlainJSON())
			if string(got) != c.want {
				t.Fatalf("%+v: got %s", c, got)
			}
		}
	}
	v, err := convertPayload([]byte{0, 1, 2}, "", oahost.KindBytes)
	if err != nil || len(v.Bytes) != 3 {
		t.Fatalf("blob %v %v", v, err)
	}
	if fmt.Sprint(Plan(mustParse(t, docB))) != "{1 1 6 0}" {
		t.Fatalf("plan %+v", Plan(mustParse(t, docB)))
	}
}

func mustParse(t *testing.T, s string) *Document {
	t.Helper()
	d, err := ParseDocument([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// AC-29: a binding to a long element of a reused type is an error; AC-31: a
// skipped datapoint with a driver address on a bound element is a warning.
func TestValidateBLongAndAddress(t *testing.T) {
	oa, sim := newSimOA(t)
	sim.AddType(simhost.Type{Name: "LongType", Elements: map[string]uint32{"": simhost.ElemStruct, "big": uint32(oahost.KindInt), "v": uint32(oahost.KindFloat)}, OaTypes: map[string]uint32{"big": 54}})
	if err := sim.CreateDP("System1", "UNS_Drv", "LongType"); err != nil {
		t.Fatal(err)
	}
	if err := sim.SetAttr("System1:UNS_Drv.v:_address.._type", oahost.Value{Kind: oahost.KindInt, Int: 16}); err != nil {
		t.Fatal(err)
	}
	raw := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},"types":[{"name":"LongType","reuse":true}],
		"datapoints":[{"name":"UNS_X","type":"LongType","bindings":[{"element":"big","topic":"plant/a"}]},{"name":"UNS_Drv","type":"LongType","bindings":[{"element":"v","topic":"plant/b"}]}]}`
	res, err := Validate(context.Background(), []byte(raw), ValidationEnv{Settings: settingsB(), Roots: TopicRoots{Native: "winccoa"}, OA: oa})
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(res.Issues, SeverityError, "/datapoints/0/bindings/0/element", "long, which cannot be written") {
		t.Fatalf("long: %v", res.Issues)
	}
	if !hasIssue(res.Issues, SeverityWarning, "/datapoints/1/name", "driver address") {
		t.Fatalf("address: %v", res.Issues)
	}
}

// AC-29: approval mode auto creates an error-free AI proposal without the
// summary; a proposal with errors is never created.
func TestAiRunAutoCreateB(t *testing.T) {
	oa, sim := newSimOA(t)
	addAlertClasses(t, sim)
	devs := &memDevices{m: map[string]stores.DeviceConfig{}}
	m := NewManager(devs, oa, newCapture().pub, AlwaysActive{}, TopicRoots{Native: "winccoa"}, "n1", slog.New(slog.DiscardHandler))
	m.SetSubscriber(&fakeSub{})
	t.Cleanup(m.Stop)
	s := settingsB()
	s.ApprovalMode = ApprovalAuto
	ctx := context.Background()
	_ = devs.Save(ctx, stores.DeviceConfig{Name: "b", Type: DeviceType, Enabled: true, NodeID: "n1", Config: s.Encode()})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	answer := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","types":[{"name":"UNS_Auto","elements":[{"path":"v","elementType":"float"}]}],
		"datapoints":[{"name":"UNS_A1","type":"UNS_Auto","bindings":[{"element":"v","topic":"plant/x"}]}]}`
	f := &fakeAI{t: t, script: []scriptStep{{tool: "validate", args: map[string]any{"proposalJson": answer}}, {final: answer}}}
	m.SetProviders(func(context.Context) (genai.Provider, error) { return f, nil })
	in := &TopicInput{Mode: "live", Filters: []string{"plant/#"}, Topics: map[string]string{"plant/x": "1.5"}}
	if _, err := m.StartAiRun(ctx, "b", AiRunRequest{TopicInput: in}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var st *AiRunStatus
	for {
		st = m.AiRun("b")
		if st != nil && st.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Status != "succeeded" || !st.Applied {
		t.Fatalf("run %+v", st)
	}
	if v, err := sim.Get("System1:UNS_A1.v:_online.._value"); err != nil || v.Kind != oahost.KindFloat {
		t.Fatalf("not created: %v %v", v, err)
	}
}

// A binding with timePath writes with the payload's source time
// (dpSetTimed); without timePath, or when the time cannot be read, the
// receive time is used and the value is still written.
func TestFlowSourceTime(t *testing.T) {
	oa, sim := newSimOA(t)
	addAlertClasses(t, sim)
	devs := &memDevices{m: map[string]stores.DeviceConfig{}}
	m := NewManager(devs, oa, newCapture().pub, AlwaysActive{}, TopicRoots{Native: "winccoa"}, "n1", slog.New(slog.DiscardHandler))
	docs := &memDocs{m: m}
	sub := &fakeSub{}
	m.SetSubscriber(sub)
	t.Cleanup(m.Stop)
	ctx := context.Background()
	_ = devs.Save(ctx, stores.DeviceConfig{Name: "b", Type: DeviceType, Enabled: true, NodeID: "n1", Config: settingsB().Encode()})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(docB, `"jsonPath":"$.flow"}`, `"jsonPath":"$.flow","timePath":"$.ts","timeFormat":"epochMs"}`, 1)
	_ = docs.Save(ctx, "b", SlotDraft, []byte(doc))
	if _, issues, err := m.CreateInWinCCOa(ctx, "b"); err != nil || HasErrors(issues) {
		t.Fatalf("create %v %v", err, issues)
	}
	stime := func(el string) time.Time {
		t.Helper()
		v, err := sim.Get("System1:UNS_P1." + el + ":_online.._stime")
		if err != nil {
			t.Fatal(err)
		}
		return v.Time
	}
	wait := func(el string, cond func(oahost.Value) bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if v, _ := sim.Get("System1:UNS_P1." + el + ":_online.._value"); cond(v) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not written", el)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	src := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	before := time.Now()
	sub.publish("plant/p1/data", fmt.Sprintf(`{"flow": 42.5, "ts": %d}`, src.UnixMilli()))
	sub.publish("plant/p1/name", `ON`)
	wait("flow", func(v oahost.Value) bool { return v.Float == 42.5 })
	wait("name", func(v oahost.Value) bool { return v.Str == "ON" })
	if got := stime("flow"); !got.Equal(src) {
		t.Fatalf("flow source time %v, want %v", got, src)
	}
	if got := stime("name"); got.Before(before) {
		t.Fatalf("name source time %v is not the receive time", got)
	}
	var flow *TopicStatus
	for _, ts := range m.LiveTopics("b") {
		if ts.DPE == "System1:UNS_P1.flow" {
			flow = &ts
		}
	}
	if flow == nil || !flow.SourceTime.Equal(src) {
		t.Fatalf("live source time %+v", flow)
	}

	// Unreadable time: written with the receive time, reported, no error count.
	errs := m.Status("b").Errors
	before = time.Now()
	sub.publish("plant/p1/data", `{"flow": 7.5, "ts": "soon"}`)
	wait("flow", func(v oahost.Value) bool { return v.Float == 7.5 })
	if got := stime("flow"); got.Before(before) {
		t.Fatalf("fallback source time %v", got)
	}
	if st := m.Status("b"); st.Errors != errs {
		t.Fatalf("errors %d -> %d", errs, st.Errors)
	}
	for _, ts := range m.LiveTopics("b") {
		if ts.DPE == "System1:UNS_P1.flow" && (!strings.Contains(ts.LastError, "receive time used") || !ts.SourceTime.IsZero()) {
			t.Fatalf("fallback status %+v", ts)
		}
	}
}

func TestDirectionBArchiving(t *testing.T) {
	// 1. Settings validation
	sBad := settingsB()
	sBad.OaArchiveEnabled = true
	sBad.OaArchiveGroup = ""
	issues := ValidateSettings(sBad, TopicRoots{})
	if !hasIssue(issues, SeverityError, "/oaArchiveGroup", "required when WinCC OA archiving is enabled") {
		t.Fatalf("expected required archive group issue, got %v", issues)
	}

	sInvalid := settingsB()
	sInvalid.OaArchiveEnabled = true
	sInvalid.OaArchiveGroup = "BAD NAME*"
	issues = ValidateSettings(sInvalid, TopicRoots{})
	if !hasIssue(issues, SeverityError, "/oaArchiveGroup", "must be a valid WinCC OA archive group name") {
		t.Fatalf("expected invalid archive group issue, got %v", issues)
	}

	// 2. WinCC OA setup & validation
	oa, sim := newSimOA(t)
	sim.AddType(simhost.Type{Name: "_NGA_Group", Elements: map[string]uint32{
		"":            simhost.ElemStruct,
		"isAlert":     uint32(oahost.KindBool),
		"displayName": uint32(oahost.KindString),
	}})
	if err := sim.CreateDP("System1", "_NGA_G_EVENT", "_NGA_Group"); err != nil {
		t.Fatal(err)
	}
	_ = sim.Set("System1:_NGA_G_EVENT.isAlert", oahost.Value{Kind: oahost.KindBool, Bool: false})
	_ = sim.Set("System1:_NGA_G_EVENT.displayName", oahost.Value{Kind: oahost.KindString, Str: "Event Archive"})
	if err := sim.CreateDP("System1", "_NGA_G_FAST", "_NGA_Group"); err != nil {
		t.Fatal(err)
	}
	_ = sim.Set("System1:_NGA_G_FAST.isAlert", oahost.Value{Kind: oahost.KindBool, Bool: false})
	_ = sim.Set("System1:_NGA_G_FAST.displayName", oahost.Value{Kind: oahost.KindString, Str: "Fast Archive"})

	s := settingsB()
	s.OaArchiveEnabled = true
	s.OaArchiveGroup = "EVENT"

	// Document with non-existent archive group
	docNonExistent := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},
		"types":[{"name":"UNS_T1","elements":[{"path":"v","elementType":"float","archive":{"enabled":true,"group":"NOSUCH"}}]}],"datapoints":[]}`
	res, err := Validate(context.Background(), []byte(docNonExistent), ValidationEnv{Settings: s, Roots: TopicRoots{}, OA: oa})
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(res.Issues, SeverityError, "/types/0/elements/0/archive/group", "does not exist in WinCC OA") {
		t.Fatalf("expected non-existent archive group error, got %v", res.Issues)
	}

	// Disabled device archiving has no default group; explicit element settings still need one.
	sNoDefault := settingsB()
	sNoDefault.OaArchiveEnabled = false
	sNoDefault.OaArchiveGroup = ""
	docNoGroup := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},
		"types":[{"name":"UNS_T1","elements":[{"path":"v","elementType":"float","archive":{"enabled":true}}]}],"datapoints":[]}`
	res, err = Validate(context.Background(), []byte(docNoGroup), ValidationEnv{Settings: sNoDefault, Roots: TopicRoots{}, OA: oa})
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(res.Issues, SeverityError, "/types/0/elements/0/archive", "archive.group is required when archiving is enabled") {
		t.Fatalf("expected missing archive group error, got %v", res.Issues)
	}
	sDisabledWithGroup := settingsB()
	sDisabledWithGroup.OaArchiveGroup = "EVENT"
	explicit, _ := ParseDocument([]byte(`{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},
		"types":[{"name":"UNS_T1","elements":[{"path":"a","elementType":"float","archive":{"enabled":true,"group":"FAST"}},{"path":"b","elementType":"float"}]}],
		"datapoints":[{"name":"UNS_D1","type":"UNS_T1","bindings":[{"element":"a","topic":"plant/a"},{"element":"b","topic":"plant/b"}]}]}`))
	if p := Plan(explicit, sDisabledWithGroup); p.ConfigWrites != 1 {
		t.Fatalf("disabled default: want only the explicit archive write, got %+v", p)
	}

	// 3. CreateInWinCCOa with device default + overrides
	devs := &memDevices{m: map[string]stores.DeviceConfig{}}
	m := NewManager(devs, oa, newCapture().pub, AlwaysActive{}, TopicRoots{Native: "winccoa"}, "n1", slog.New(slog.DiscardHandler))
	docs := &memDocs{m: m}
	sub := &fakeSub{}
	m.SetSubscriber(sub)
	t.Cleanup(m.Stop)
	ctx := context.Background()

	docArchiveB := `{"schemaVersion":1,"direction":"mqtt_to_winccoa","source":{"mode":"live","topicFilters":["plant/#"]},
	 "types":[{"name":"UNS_Motor","elements":[
	   {"path":"speed","elementType":"float"},
	   {"path":"temp","elementType":"float","archive":{"enabled":true,"group":"FAST"}},
	   {"path":"status","elementType":"string","archive":{"enabled":false}},
	   {"path":"label","elementType":"string"}]}],
	 "datapoints":[{"name":"UNS_M1","type":"UNS_Motor",
	   "elements":[{"path":"speed","archive":{"enabled":false}}],
	   "bindings":[
	     {"element":"speed","topic":"plant/m1/speed"},
	     {"element":"temp","topic":"plant/m1/temp"},
	     {"element":"status","topic":"plant/m1/status"},
	     {"element":"label","topic":"plant/m1/label"}
	   ]}]}`

	parsedDoc, err := ParseDocument([]byte(docArchiveB))
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan(parsedDoc, s)
	// temp (FAST) = 1, label (EVENT inherited) = 1; speed (opted out at dp level) = 0, status (opted out at type level) = 0.
	if plan.ConfigWrites != 2 {
		t.Fatalf("expected 2 config writes in plan, got %+v", plan)
	}

	_ = devs.Save(ctx, stores.DeviceConfig{Name: "dev-archive", Type: DeviceType, Enabled: true, NodeID: "n1", Config: s.Encode()})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = docs.Save(ctx, "dev-archive", SlotDraft, []byte(docArchiveB))

	createRes, issues, err := m.CreateInWinCCOa(ctx, "dev-archive")
	if err != nil {
		t.Fatal(err)
	}
	if HasErrors(issues) {
		t.Fatalf("create issues: %v", issues)
	}
	if createRes.ConfigWrites != 2 {
		t.Fatalf("expected 2 config writes, got %d (objects: %+v)", createRes.ConfigWrites, createRes.Objects)
	}

	// 4. Verify WinCC OA config attributes written to simhost
	getAttr := func(addr string) oahost.Value {
		v, err := sim.Get(addr)
		if err != nil {
			t.Fatalf("get %s: %v", addr, err)
		}
		return v
	}

	// speed: opted out -> no _archive config
	if v := getAttr("System1:UNS_M1.speed:_archive.._type"); v.Kind != oahost.KindNull {
		t.Fatalf("speed should not be archived, got %+v", v)
	}
	// status: opted out -> no _archive config
	if v := getAttr("System1:UNS_M1.status:_archive.._type"); v.Kind != oahost.KindNull {
		t.Fatalf("status should not be archived, got %+v", v)
	}
	// temp: archived with FAST
	if v := getAttr("System1:UNS_M1.temp:_archive.._type"); v.Int != 45 {
		t.Fatalf("temp _archive.._type = %+v, want 45", v)
	}
	if v := getAttr("System1:UNS_M1.temp:_archive.1._type"); v.Int != 15 {
		t.Fatalf("temp _archive.1._type = %+v, want 15", v)
	}
	if v := getAttr("System1:UNS_M1.temp:_archive.1._proc"); v.Int != 15 {
		t.Fatalf("temp _archive.1._proc = %+v, want 15", v)
	}
	if v := getAttr("System1:UNS_M1.temp:_archive.1._class"); v.Str != "System1:_NGA_G_FAST." {
		t.Fatalf("temp _archive.1._class = %+v, want System1:_NGA_G_FAST.", v)
	}
	if v := getAttr("System1:UNS_M1.temp:_archive.._archive"); !v.Bool {
		t.Fatalf("temp _archive.._archive = %+v, want true", v)
	}

	// label: text element archived with EVENT (device default)
	if v := getAttr("System1:UNS_M1.label:_archive.._type"); v.Int != 45 {
		t.Fatalf("label _archive.._type = %+v, want 45", v)
	}
	if v := getAttr("System1:UNS_M1.label:_archive.1._class"); v.Str != "System1:_NGA_G_EVENT." {
		t.Fatalf("label _archive.1._class = %+v, want System1:_NGA_G_EVENT.", v)
	}
	if v := getAttr("System1:UNS_M1.label:_archive.._archive"); !v.Bool {
		t.Fatalf("label _archive.._archive = %+v, want true", v)
	}
}
