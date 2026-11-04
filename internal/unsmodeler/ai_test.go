package unsmodeler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/genai"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
)

// scriptStep is one move of the fake model: a tool call or a final answer.
type scriptStep struct {
	tool  string
	args  any
	final string
	check func(t *testing.T, result string)
}

type fakeAI struct {
	t       *testing.T
	script  []scriptStep
	tools   []string
	results []string
}

func (f *fakeAI) ProviderName() string { return "fake" }
func (f *fakeAI) ModelName() string    { return "fake-1" }
func (f *fakeAI) IsReady() bool        { return true }
func (f *fakeAI) Close() error         { return nil }
func (f *fakeAI) Generate(context.Context, genai.Request) (*genai.Response, error) {
	return nil, errors.New("not used")
}

func (f *fakeAI) GenerateWithTools(ctx context.Context, req genai.ToolRequest, tools []genai.Tool) (*genai.ToolResponse, error) {
	byName := map[string]genai.Tool{}
	for _, t := range tools {
		f.tools = append(f.tools, t.Name)
		byName[t.Name] = t
	}
	res := &genai.ToolResponse{}
	for _, s := range f.script {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if req.MaxSteps > 0 && res.ToolCalls >= req.MaxSteps {
			return res, genai.ErrMaxSteps
		}
		res.ToolCalls++
		if s.tool != "" {
			args, _ := json.Marshal(s.args)
			req.OnEvent(genai.ToolEvent{Kind: "tool_call", Name: s.tool, Args: string(args)})
			out, err := byName[s.tool].Handler(ctx, args)
			if err != nil {
				out = "error: " + err.Error()
			}
			req.OnEvent(genai.ToolEvent{Kind: "tool_result", Name: s.tool, Result: out})
			f.results = append(f.results, out)
			if s.check != nil {
				s.check(f.t, out)
			}
			continue
		}
		feedback, done := req.Final(s.final)
		f.results = append(f.results, feedback)
		if s.check != nil {
			s.check(f.t, feedback)
		}
		if done {
			res.Text = s.final
			return res, nil
		}
	}
	return res, errors.New("script ended without an accepted answer")
}

func aiSetup(t *testing.T, gate RoleGate, mode string, script []scriptStep) (*Manager, *fakeAI, *memDocs) {
	t.Helper()
	m, devs, docs, _ := newTestManager(t, gate)
	s := settingsA("u")
	s.ApprovalMode = mode
	_ = devs.Save(context.Background(), stores.DeviceConfig{Name: "d", Type: DeviceType, Enabled: true, NodeID: "n1", Config: s.Encode()})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &fakeAI{t: t, script: script}
	m.SetProviders(func(context.Context) (genai.Provider, error) { return f, nil })
	return m, f, docs
}

func waitRun(t *testing.T, m *Manager) *AiRunStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := m.AiRun("d")
		if st != nil && st.Status != "running" {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var selA = Selection{Items: []SelectionItem{
	{Kind: "type", Type: "PumpType", Pattern: "Hall2_L2_P*"},
	{Kind: "datapoint", Type: "PumpType", Datapoint: "Hall2_P101"},
}}

const goodProposal = `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"id":"invented","name":"hall2","level":"area","children":[
	{"name":"p101","binding":{"kind":"datapoint","type":"PumpType","datapoint":"Hall2_P101","elements":[{"path":"flow"}]}},
	{"name":"line2","binding":{"kind":"pattern","type":"PumpType","pattern":"Hall2_L2_P*","elements":[{"path":"flow"}]}}]}]}`

func TestAiRunTools(t *testing.T) {
	notContains := func(s string) func(*testing.T, string) {
		return func(t *testing.T, r string) {
			if strings.Contains(r, s) {
				t.Errorf("result contains %q: %s", s, r)
			}
		}
	}
	contains := func(s string) func(*testing.T, string) {
		return func(t *testing.T, r string) {
			if !strings.Contains(r, s) {
				t.Errorf("result lacks %q: %s", s, r)
			}
		}
	}
	m, f, docs := aiSetup(t, AlwaysActive{}, ApprovalReview, []scriptStep{
		{tool: "listTypes", args: map[string]any{"pattern": "*"}, check: func(t *testing.T, r string) {
			if r != `["PumpType"]` {
				t.Errorf("listTypes %s", r)
			}
		}},
		{tool: "getTypeStructure", args: map[string]any{"type": "Scalar"}, check: contains("outside the selection")},
		{tool: "getTypeStructure", args: map[string]any{"type": "PumpType"}, check: contains("motor.speed")},
		{tool: "listDatapoints", args: map[string]any{"pattern": "*"}, check: func(t *testing.T, r string) {
			contains("Hall2_L2_P1")(t, r)
			contains("Hall2_P101")(t, r)
			notContains("Hall2_P102")(t, r)
		}},
		{tool: "getElementInfo", args: map[string]any{"dpes": "Hall2_P101.flow\nHall2_P102.flow"}, check: func(t *testing.T, r string) {
			var out []map[string]any
			_ = json.Unmarshal([]byte(r), &out)
			if len(out) != 2 || out[0]["elementType"] != "float" || out[1]["error"] != "outside the selection" {
				t.Errorf("elementInfo %s", r)
			}
		}},
		// A binding outside the selection is an AI run context error.
		{tool: "validate", args: map[string]any{"proposalJson": `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"name":"x","binding":{"kind":"element","type":"PumpType","datapoint":"Hall2_P102","element":"flow"}}]}`},
			check: contains("outside the selection")},
		{tool: "validate", args: map[string]any{"proposalJson": "```json\n" + goodProposal + "\n```"}, check: contains("no issues")},
		// The same document as final answer with an extra error is rejected.
		{final: `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"name":"x","binding":{"kind":"element","type":"PumpType","datapoint":"Hall2_P102","element":"flow"}}]}`, check: contains("outside the selection")},
		{final: "VALIDATED"},
	})
	_, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Prompt: "model hall 2", Selection: selA})
	if err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, m)
	if got.Status != "succeeded" || got.Applied {
		t.Fatalf("run %+v", got)
	}
	want := []string{"listTypes", "getTypeStructure", "listDatapoints", "getElementInfo", "listArchiveGroups", "validate"}
	if fmt.Sprint(f.tools) != fmt.Sprint(want) {
		t.Fatalf("tools %v", f.tools)
	}
	raw, _ := docs.Get(context.Background(), "d", SlotDraft)
	doc, err := ParseDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.AiRun == nil || doc.AiRun.Provider != "fake" || doc.AiRun.Model != "fake-1" || doc.AiRun.FinishedAt == "" {
		t.Fatalf("aiRun %+v", doc.AiRun)
	}
	ids := map[string]bool{}
	doc.Walk(func(_ string, _ []*Node, n *Node) {
		if n.ID == "" || n.ID == "invented" || ids[n.ID] || !n.AiGenerated {
			t.Errorf("node %+v", n)
		}
		ids[n.ID] = true
	})
	if len(got.Events) == 0 {
		t.Fatal("no run events")
	}
}

func TestAiRunNoWritesOnHost(t *testing.T) {
	oa, sim := newSimOA(t)
	m := NewManager(&memDevices{m: map[string]stores.DeviceConfig{}}, oa, newCapture().pub, AlwaysActive{}, TopicRoots{Native: "winccoa"}, "n1", slog.New(slog.DiscardHandler))
	before := map[uint32]uint64{}
	write := []uint32{oahost.OpDpSet, oahost.OpDpCreate, oahost.OpDpDelete, oahost.OpSetMeta, oahost.OpTypeCheck, oahost.OpDpConnect, oahost.OpQueryConnect}
	for _, op := range write {
		before[op] = sim.Calls(op)
	}
	for _, restrict := range []bool{true, false} {
		sc := newScope(selA, "System1")
		sc.sel.AllowBeyond = !restrict
		for _, tl := range m.toolsA(sc) {
			args, _ := json.Marshal(map[string]any{"pattern": "*", "type": "PumpType", "dpes": "Hall2_P101.flow\nTag1.", "limit": 5})
			_, _ = tl.Handler(context.Background(), args)
		}
	}
	for _, op := range write {
		if sim.Calls(op) != before[op] {
			t.Fatalf("tool reached op %d", op)
		}
	}
}

func TestAiRunStandbyAndLimits(t *testing.T) {
	g := &SwitchGate{}
	m, _, docs := aiSetup(t, g, ApprovalReview, nil)
	g.Set(false)
	if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); !errors.Is(err, ErrStandby) {
		t.Fatalf("standby: %v", err)
	}
	g.Set(true)
	if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{}); err == nil {
		t.Fatal("run without selection accepted")
	}
	big := []byte(`{"x":"` + strings.Repeat("a", MaxAIDocumentBytes) + `"}`)
	_ = docs.Save(context.Background(), "d", SlotDraft, big)
	if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); err == nil || !strings.Contains(err.Error(), "limited") {
		t.Fatalf("oversized draft: %v", err)
	}
}

func TestAiRunMaxSteps(t *testing.T) {
	var script []scriptStep
	for i := 0; i < 40; i++ {
		script = append(script, scriptStep{tool: "listTypes", args: map[string]any{}})
	}
	m, _, docs := aiSetup(t, AlwaysActive{}, ApprovalReview, script)
	if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); err != nil {
		t.Fatal(err)
	}
	st := waitRun(t, m)
	if st.Status != "failed" || !strings.Contains(st.Error, "maximum") {
		t.Fatalf("run %+v", st)
	}
	if d, _ := docs.Get(context.Background(), "d", SlotDraft); d != nil {
		t.Fatal("draft stored by a failed run")
	}
}

func TestAiRunValidationRetryLimit(t *testing.T) {
	for _, mode := range []string{"tool", "arguments", "final", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			var script []scriptStep
			for i := 0; i < 10; i++ {
				step := scriptStep{tool: "validate", args: map[string]any{"proposalJson": `{"schemaVersion":`}}
				switch mode {
				case "arguments":
					step.args = []string{"not an arguments object"}
				case "final":
					step = scriptStep{final: "not JSON"}
				case "mixed":
					if i%2 == 1 {
						step = scriptStep{final: "VALIDATED"}
					}
				}
				script = append(script, step)
			}
			m, f, docs := aiSetup(t, AlwaysActive{}, ApprovalAuto, script)
			if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); err != nil {
				t.Fatal(err)
			}
			st := waitRun(t, m)
			if st.Status != "failed" || st.Error != errValidationRetries.Error() || len(f.results) != 4 {
				t.Fatalf("run %+v, %d attempts", st, len(f.results))
			}
			for _, slot := range []string{SlotDraft, SlotApplied} {
				if raw, _ := docs.Get(context.Background(), "d", slot); raw != nil {
					t.Fatalf("failed run stored %s", slot)
				}
			}
		})
	}
}

func TestAiRunCancellation(t *testing.T) {
	for _, action := range []string{"cancel", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			m, f, docs := aiSetup(t, AlwaysActive{}, ApprovalAuto, []scriptStep{{final: goodProposal}})
			started := make(chan struct{})
			m.SetProviders(func(ctx context.Context) (genai.Provider, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			if _, err := m.CancelAiRun("d"); err == nil {
				t.Fatal("cancellation without an active run succeeded")
			}
			if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("provider did not start")
			}
			if action == "shutdown" {
				m.Stop()
			} else if _, err := m.CancelAiRun("d"); err != nil {
				t.Fatal(err)
			}
			st := waitRun(t, m)
			if st.Status != "cancelled" || st.Applied || st.Finished.IsZero() {
				t.Fatalf("cancelled run %+v", st)
			}
			if raw, _ := docs.Get(context.Background(), "d", SlotDraft); raw != nil {
				t.Fatal("cancelled run stored a draft")
			}
			if _, err := m.CancelAiRun("d"); err == nil {
				t.Fatal("completed run accepted cancellation")
			}
			if action == "cancel" {
				m.SetProviders(func(context.Context) (genai.Provider, error) { return f, nil })
				if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); err != nil {
					t.Fatalf("cannot restart after cancellation: %v", err)
				}
				if st := waitRun(t, m); st.Status != "succeeded" {
					t.Fatalf("restarted run %+v", st)
				}
			}
		})
	}
}

func TestAiRunAutoApply(t *testing.T) {
	m, _, docs := aiSetup(t, AlwaysActive{}, ApprovalAuto, []scriptStep{
		{tool: "validate", args: map[string]any{"proposalJson": goodProposal}},
		{final: goodProposal},
	})
	if _, err := m.StartAiRun(context.Background(), "d", AiRunRequest{Selection: selA}); err != nil {
		t.Fatal(err)
	}
	st := waitRun(t, m)
	if st.Status != "succeeded" || !st.Applied {
		t.Fatalf("run %+v", st)
	}
	applied, _ := docs.Get(context.Background(), "d", SlotApplied)
	doc, err := ParseDocument(applied)
	if err != nil || len(doc.Tree) != 1 {
		t.Fatalf("applied %s %v", applied, err)
	}
	doc.Walk(func(_ string, _ []*Node, n *Node) {
		if n.ID == "" {
			t.Error("node without id applied")
		}
	})
	if s := m.Status("d"); s.Topics < 3 {
		t.Fatalf("status %+v", s)
	}
}

// A re-run keeps ids of the draft and only stamps changed nodes.
func TestAiRunMergeKeepsIDs(t *testing.T) {
	m, _, docs := aiSetup(t, AlwaysActive{}, ApprovalReview, nil)
	_, _, _ = m.SaveDraft(context.Background(), "d", []byte(`{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[
		{"id":"keep","name":"hall2","children":[{"id":"other","name":"tag","binding":{"kind":"element","type":"Scalar","datapoint":"Tag1","element":""}}]}]}`))
	raw, _ := docs.Get(context.Background(), "d", SlotDraft)
	draft, _ := ParseDocument(raw)
	sc := newScope(selA, "System1")
	s, _ := ParseSettings(settingsA("u").Encode())
	// Removing the node bound outside the selection is rejected.
	p := m.prepareA(context.Background(), "d", s, `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"id":"keep","name":"hall2"}]}`, draft, sc, AiRun{Provider: "p", Model: "m"})
	if !hasIssue(p.Issues, SeverityError, "/tree", "cannot be removed") {
		t.Fatalf("removal: %v", p.Issues)
	}
	p = m.prepareA(context.Background(), "d", s, `{"schemaVersion":1,"direction":"winccoa_to_uns","tree":[{"id":"keep","name":"hall2","children":[
		{"id":"other","name":"tag","binding":{"kind":"element","type":"Scalar","datapoint":"Tag1","element":""}},
		{"name":"p101","binding":{"kind":"datapoint","type":"PumpType","datapoint":"Hall2_P101"}}]}]}`, draft, sc, AiRun{Provider: "p", Model: "m"})
	if HasErrors(p.Issues) {
		t.Fatal(p.Issues)
	}
	doc, _ := ParseDocument(p.Doc)
	if doc.Tree[0].ID != "keep" || doc.Tree[0].AiGenerated || doc.Tree[0].Children[0].AiGenerated || !doc.Tree[0].Children[1].AiGenerated {
		t.Fatalf("stamping %s", p.Doc)
	}
}
