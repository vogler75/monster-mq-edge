package unsmodeler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/genai"
	"monstermq.io/edge/internal/winccoabrowse"
)

// MaxAIDocumentBytes bounds the draft and every document of an AI run
// (§6.5): every copy stays in the model context.
const MaxAIDocumentBytes = 64 << 10

// Tool result limits.
const (
	maxToolResultBytes = 32 << 10
	maxToolNames       = 200
	maxRunEvents       = 300
)

// ProviderLookup creates the GenAI provider of AI runs: the configured
// GenAI assistant provider. A nil provider means GenAI is disabled.
type ProviderLookup func(ctx context.Context) (genai.Provider, error)

// DefaultSystemPromptA is the built-in prompt of Direction A.
const DefaultSystemPromptA = `You are a Unified Namespace (UNS) architect for a WinCC OA SCADA system.
You design a UNS topic tree below the device's UNS root for the WinCC OA datapoints the user selected,
following the ISA-95 equipment hierarchy (enterprise, site, area, line or cell, unit, equipment and control modules).

How to work:
- Explore the selected data with the tools: datapoint types and their structure, datapoints, element metadata
  (unit, description) and current values. Never invent datapoints, elements or types.
- Derive the plant structure from datapoint names, descriptions, the user's prompt and the context document.
- Use short, lowercase, hyphenated node names (e.g. "filling-line-1", "pump-p101"); one node is one topic level.
- Set the ISA-95 level of structural nodes where it is clear.
- Prefer one pattern binding for many uniform datapoints of one type below a common node; use datapoint
  bindings for single equipment and element bindings for single values that belong at a specific place.
- Select only meaningful value elements; leave out internal or diagnostic elements unless asked for.
- Use layout "object" (one JSON topic per datapoint in the shape of its type) when the user asks for it or
  when a datapoint's elements belong together and are used together; otherwise keep one topic per element.
- Keep every existing node of the current draft (with its id) unless the user asks to change it.
Call validate with the complete document, correct every error it reports and call it again until it reports
no errors. Then answer with the final document only.`

// DefaultSystemPromptB is the built-in prompt of Direction B.
const DefaultSystemPromptB = `You are an engineer onboarding MQTT data into WinCC OA.
Analyse the selected MQTT topic tree: group topics into devices or logical units, recognise recurring
structures, and propose WinCC OA datapoint types (DPTs) and datapoints with topic bindings.

How to work:
- Browse and sample the topics with the tools; inspect the payloads to derive element types
  (float for measurements, bool for states, string for text, int/uint only for integral counters or codes).
- Reuse an existing WinCC OA type when its structure fits (check it with the WinCC OA tools).
- Put units, descriptions, formats, value ranges, alarm limits and archive configs into element metadata, never into extra elements.
- For JSON payloads bind single fields with jsonPath; without jsonPath the whole payload is the value.
- Look for timestamps in the payloads (ISO 8601 strings, or numbers of seconds or milliseconds since 1970,
  often named ts, time, timestamp, TimeMS). Use them as the source time of the values (timePath and
  timeFormat), not as datapoint elements.
- Type and datapoint names must start with the device's prefix (oaPrefix).
- Keep the names of the current draft's types and datapoints.
Call validate with the complete document, correct every error it reports and call it again until it reports
no errors. Then answer with the final document only.`

// AiEvent is one entry of a run log.
type AiEvent struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Name   string    `json:"name,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// AiRunStatus is the state of the running or last AI run of a device.
type AiRunStatus struct {
	ID              string    `json:"id"`
	Device          string    `json:"device"`
	Status          string    `json:"status"` // running, succeeded, failed, cancelled
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	Started         time.Time `json:"started"`
	Finished        time.Time `json:"finished"`
	Steps           int       `json:"steps"`
	ToolCalls       int       `json:"toolCalls"`
	InputTokens     int       `json:"inputTokens"`
	OutputTokens    int       `json:"outputTokens"`
	Error           string    `json:"error,omitempty"`
	Applied         bool      `json:"applied"`
	Issues          []Issue   `json:"issues,omitempty"`
	Events          []AiEvent `json:"events"`
	cancel          context.CancelCauseFunc
	cancelRequested bool
}

// AiRunRequest starts an AI run.
type AiRunRequest struct {
	Prompt string
	// Document is the context document text (already extracted).
	Document     string
	DocumentName string
	Selection    Selection
	// Direction B input.
	TopicInput *TopicInput
}

type aiRuns struct {
	mu   sync.Mutex
	last map[string]*AiRunStatus
}

func (r *aiRuns) get(device string) *AiRunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.last[device]; s != nil {
		cp := *s
		cp.Events = slices.Clone(s.Events)
		cp.Issues = slices.Clone(s.Issues)
		return &cp
	}
	return nil
}

func (r *aiRuns) update(st *AiRunStatus, fn func(*AiRunStatus)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(st)
	if len(st.Events) > maxRunEvents {
		st.Events = st.Events[len(st.Events)-maxRunEvents:]
	}
}

// SetProviders sets the provider lookup of AI runs.
func (m *Manager) SetProviders(p ProviderLookup) { m.providers = p }

// AiRun returns the running or last AI run of a device since start.
func (m *Manager) AiRun(device string) *AiRunStatus { return m.runs.get(device) }

// ErrRunActive rejects a second run on the same device.
var ErrRunActive = errors.New("an AI run is already running for this device")

const maxValidationFailures = 4 // Initial attempt plus three corrections.

var errValidationRetries = errors.New("AI validation failed after 4 attempts; select fewer topics or reduce the requested scope and try again")

func (m *Manager) CancelAiRun(name string) (*AiRunStatus, error) {
	m.runs.mu.Lock()
	st := m.runs.last[name]
	if st == nil || st.Status != "running" {
		m.runs.mu.Unlock()
		return nil, errors.New("no AI run is running for this device")
	}
	st.cancel(context.Canceled)
	st.cancelRequested = true
	m.runs.mu.Unlock()
	return m.runs.get(name), nil
}

// StartAiRun starts an AI run in the background and returns its status.
func (m *Manager) StartAiRun(ctx context.Context, name string, req AiRunRequest) (*AiRunStatus, error) {
	_, s, err := m.deviceConfig(ctx, name)
	if err != nil {
		return nil, err
	}
	if !m.deviceGate(s).Active() {
		return nil, ErrStandby
	}
	if m.browse == nil {
		return nil, ErrNotEmbedded
	}
	if m.providers == nil {
		return nil, errors.New("GenAI is not available")
	}
	draftRaw, err := m.getDoc(ctx, name, SlotDraft)
	if err != nil {
		return nil, err
	}
	if len(draftRaw) > MaxAIDocumentBytes {
		return nil, fmt.Errorf("the draft has %d bytes; AI runs are limited to drafts of %d bytes", len(draftRaw), MaxAIDocumentBytes)
	}
	if len(req.Document) > MaxAIDocumentBytes {
		return nil, fmt.Errorf("the context document has %d bytes of text; AI runs are limited to %d bytes", len(req.Document), MaxAIDocumentBytes)
	}
	if s.Direction == DirectionWinccoaToUns && len(req.Selection.Items) == 0 {
		return nil, errors.New("select datapoint types, datapoints or elements in the WinCC OA browser first")
	}
	if s.Direction == DirectionMqttToWinccoa && (req.TopicInput == nil || len(req.TopicInput.Filters) == 0) {
		return nil, errors.New("select the topics to analyse first")
	}
	m.runs.mu.Lock()
	if cur := m.runs.last[name]; cur != nil && cur.Status == "running" {
		m.runs.mu.Unlock()
		return nil, ErrRunActive
	}
	st := &AiRunStatus{ID: NewNodeID(), Device: name, Status: "running", Started: time.Now().UTC(), Events: []AiEvent{}}
	runCtx, cancel := context.WithCancelCause(context.Background())
	st.cancel = cancel
	m.runs.last[name] = st
	m.runs.mu.Unlock()
	go m.runAI(runCtx, name, s, req, st, draftRaw)
	return m.runs.get(name), nil
}

func (m *Manager) runLog(st *AiRunStatus) *slog.Logger {
	return m.log.With("device", st.Device, "aiRun", st.ID)
}

func (m *Manager) event(st *AiRunStatus, e AiEvent) {
	e.Time = time.Now().UTC()
	if len(e.Detail) > 4000 {
		e.Detail = e.Detail[:4000] + "…"
	}
	m.runs.update(st, func(s *AiRunStatus) { s.Events = append(s.Events, e) })
}

func (m *Manager) finishRun(st *AiRunStatus, err error, issues []Issue, applied bool) {
	m.runs.update(st, func(s *AiRunStatus) {
		if s.cancelRequested {
			err = context.Canceled
		}
		s.Finished = time.Now().UTC()
		s.Issues = issues
		s.Applied = applied
		if err != nil {
			s.Status, s.Error = "failed", err.Error()
			if errors.Is(err, context.Canceled) {
				s.Status = "cancelled"
			}
		} else {
			s.Status = "succeeded"
		}
	})
	l := m.runLog(st)
	if errors.Is(err, context.Canceled) {
		l.Info("uns ai run cancelled", "steps", st.Steps, "toolCalls", st.ToolCalls, "tokens", st.InputTokens+st.OutputTokens)
	} else if err != nil {
		l.Warn("uns ai run failed", "err", err, "steps", st.Steps, "toolCalls", st.ToolCalls, "tokens", st.InputTokens+st.OutputTokens)
	} else {
		l.Info("uns ai run succeeded", "steps", st.Steps, "toolCalls", st.ToolCalls, "tokens", st.InputTokens+st.OutputTokens, "applied", applied)
	}
}

func (m *Manager) runAI(parent context.Context, name string, s *Settings, req AiRunRequest, st *AiRunStatus, draftRaw []byte) {
	defer st.cancel(nil)
	ctx, cancel := genai.WithTimeout(parent, s.AiTimeoutSec)
	defer cancel()
	l := m.runLog(st)
	prov, err := m.providers(ctx)
	if err == nil && prov == nil {
		err = errors.New("GenAI is disabled")
	}
	if err != nil {
		m.finishRun(st, fmt.Errorf("AI provider: %w", err), nil, false)
		return
	}
	defer prov.Close()
	tc, ok := prov.(genai.ToolCaller)
	if !ok {
		m.finishRun(st, fmt.Errorf("AI provider %s: %w", prov.ProviderName(), genai.ErrNoToolSupport), nil, false)
		return
	}
	m.runs.update(st, func(x *AiRunStatus) { x.Provider, x.Model = prov.ProviderName(), prov.ModelName() })
	run := AiRun{Provider: prov.ProviderName(), Model: prov.ModelName()}
	if run.Model == "" {
		run.Model = "default"
	}
	var draft *Document
	if draftRaw != nil {
		if issues, _ := ValidateSchema(draftRaw); len(issues) == 0 {
			draft, _ = ParseDocument(draftRaw)
		}
	}
	local, err := m.browse.LocalSystem(ctx)
	if err != nil {
		m.finishRun(st, err, nil, false)
		return
	}
	var tools []genai.Tool
	var check func(text string) PreparedAnswer
	var system, prompt string
	switch s.Direction {
	case DirectionWinccoaToUns:
		sc := newScope(req.Selection, local)
		tools = m.toolsA(sc)
		check = func(text string) PreparedAnswer { return m.prepareA(ctx, name, s, text, draft, sc, run) }
		system, prompt = m.promptA(s, req, draft)
	default:
		sc := newScope(Selection{AllowBeyond: true}, local)
		var err error
		tools, err = m.toolsB(ctx, sc, req.TopicInput)
		if err != nil {
			m.finishRun(st, err, nil, false)
			return
		}
		check = func(text string) PreparedAnswer { return m.prepareB(ctx, name, s, text, draft, req.TopicInput, run) }
		system, prompt = m.promptB(s, req, draft)
	}
	var lastValid PreparedAnswer
	var lastIssues []Issue
	failures := 0
	validationFailed := func(issues []Issue) {
		lastIssues = issues
		failures++
		if failures >= maxValidationFailures {
			st.cancel(errValidationRetries)
		}
	}
	tools = append(tools, genai.Tool{
		Name: "validate",
		Description: "Validates a complete mapping document exactly like the final answer (server preparation, schema and semantic checks). " +
			"Pass the whole document as one JSON string. Returns the issues; an empty list means the document can be the final answer.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"proposalJson": map[string]any{"type": "string", "description": "the complete mapping document as JSON text"},
		}},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				ProposalJSON string `json:"proposalJson"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				validationFailed(nil)
				return "", fmt.Errorf("arguments: %v", err)
			}
			p := check(a.ProposalJSON)
			if p.Doc != nil && !HasErrors(p.Issues) {
				lastValid = p
			} else {
				validationFailed(p.Issues)
			}
			l.Info("uns ai validate", "issues", len(p.Issues), "errors", countSeverity(p.Issues, SeverityError))
			return issuesText(p.Issues), nil
		},
	})
	var final PreparedAnswer
	l.Info("uns ai run started", "provider", run.Provider, "model", run.Model, "direction", s.Direction, "selection", len(req.Selection.Items))
	m.event(st, AiEvent{Kind: "start", Detail: fmt.Sprintf("provider %s, model %s, %d tools", run.Provider, run.Model, len(tools))})
	resp, err := tc.GenerateWithTools(ctx, genai.ToolRequest{
		System: system, Prompt: prompt, MaxSteps: s.AiMaxSteps, TokenBudget: s.AiTokenBudget,
		OnEvent: func(e genai.ToolEvent) {
			switch e.Kind {
			case "usage":
				m.runs.update(st, func(x *AiRunStatus) { x.Steps = e.Step })
				return
			case "tool_call":
				m.runs.update(st, func(x *AiRunStatus) { x.ToolCalls++ })
				l.Info("uns ai tool call", "tool", e.Name, "args", truncate(e.Args, 500))
				m.event(st, AiEvent{Kind: e.Kind, Name: e.Name, Detail: e.Args})
			case "tool_result":
				l.Info("uns ai tool result", "tool", e.Name, "result", truncate(e.Result, 500), "err", e.Error)
				m.event(st, AiEvent{Kind: e.Kind, Name: e.Name, Detail: e.Result, Error: e.Error})
			default:
				l.Info("uns ai "+e.Kind, "text", truncate(e.Result, 500))
				m.event(st, AiEvent{Kind: e.Kind, Detail: e.Result})
			}
		},
		Final: func(text string) (string, bool) {
			if strings.EqualFold(strings.Trim(strings.TrimSpace(text), "`'\"."), "VALIDATED") {
				if lastValid.Doc == nil {
					validationFailed(nil)
					return "No document passed validate without errors yet. Call validate with the complete document first.", false
				}
				final = lastValid
				return "", true
			}
			p := check(text)
			if p.Doc != nil && !HasErrors(p.Issues) {
				final = p
				return "", true
			}
			validationFailed(p.Issues)
			return "The final answer is not an error-free mapping document:\n" + issuesText(p.Issues) +
				"\nCorrect the document, call validate, and answer with the corrected complete document.", false
		},
	}, tools)
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if resp != nil {
		m.runs.update(st, func(x *AiRunStatus) {
			x.InputTokens, x.OutputTokens = resp.InputTokens, resp.OutputTokens
		})
	}
	if err != nil {
		m.finishRun(st, err, lastIssues, false)
		return
	}
	if final.Doc == nil {
		m.finishRun(st, errors.New("the AI run ended without an error-free document"), nil, false)
		return
	}
	saveCtx, cancelSave := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSave()
	if err := saveCtx.Err(); err != nil {
		m.finishRun(st, err, final.Issues, false)
		return
	}
	if err := m.saveDoc(saveCtx, name, SlotDraft, final.Doc); err != nil {
		m.finishRun(st, fmt.Errorf("storing the proposal: %w", err), final.Issues, false)
		return
	}
	applied := false
	if s.ApprovalMode == ApprovalAuto {
		var aerr error
		var issues []Issue
		if s.Direction == DirectionWinccoaToUns {
			issues, aerr = m.Apply(saveCtx, name)
		} else {
			_, issues, aerr = m.CreateInWinCCOa(saveCtx, name)
		}
		if aerr != nil || HasErrors(issues) {
			m.finishRun(st, fmt.Errorf("auto apply failed: %v %s", aerr, issuesText(issues)), final.Issues, false)
			return
		}
		applied = true
	}
	m.finishRun(st, nil, final.Issues, applied)
}

func countSeverity(issues []Issue, sev string) int {
	n := 0
	for _, is := range issues {
		if is.Severity == sev {
			n++
		}
	}
	return n
}

func issuesText(issues []Issue) string {
	if len(issues) == 0 {
		return "[] (no issues: the document is valid)"
	}
	b, _ := marshalNoEscape(issues)
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// promptA assembles system and user prompt of a Direction A run (§6.6).
func (m *Manager) promptA(s *Settings, req AiRunRequest, draft *Document) (string, string) {
	var b strings.Builder
	base := s.SystemPrompt
	if strings.TrimSpace(base) == "" {
		base = DefaultSystemPromptA
	}
	b.WriteString(base)
	schema, _ := PromptSchema(DirectionWinccoaToUns)
	fmt.Fprintf(&b, "\n\n## Mapping document (JSON Schema)\n%s\n", schema)
	b.WriteString("\n## ISA-95 levels (Node.level)\n")
	b.WriteString(strings.Join(isa95Levels(), ", "))
	b.WriteString("\n\n## Payload config attributes (allowlist)\n_online attributes except _value (e.g. _online.._invalid) and _lock attributes are dynamic; " +
		"_pv_range, _alert_hdl (limits, classes, texts), _archive, _default and _smooth attributes are static. _original, _address and _distrib are not allowed.\n")
	fmt.Fprintf(&b, "\n## Limits\nDocument at most %d bytes in AI runs, %d node levels, %d nodes, %d bindings, %d pattern bindings, %d expanded DPEs.\n",
		MaxAIDocumentBytes, MaxTreeDepth, MaxNodes, MaxBindings, MaxPatternBindings, MaxExpandedDPEs)
	b.WriteString("\n## Rules\n" +
		"- Topics are relative to the device's UNS root " + quoteJSON(s.UnsRoot) + "; never repeat the root in node names. Every binding needs at least one node level.\n" +
		"- Bind only data of the user's selection (listed in the user message). Do not change or remove existing nodes bound outside the selection.\n" +
		"- Keep the id of every existing node; omit id for new nodes. Do not write aiGenerated or aiRun.\n" +
		"- System is the remote system name; omit it for the local system.\n" +
		"- layout object applies to datapoint and pattern bindings only; its elements have no topic and each element path appears once.\n" +
		"- The final answer is the complete document that passed validate without errors (only the JSON, no explanation), " +
		"or the single word VALIDATED to use the last document that passed validate without errors.\n")
	var u strings.Builder
	if strings.TrimSpace(req.Prompt) != "" {
		u.WriteString(req.Prompt)
		u.WriteString("\n\n")
	}
	sel, _ := marshalNoEscape(req.Selection.Items)
	fmt.Fprintf(&u, "## Selection (scope of the analysis)\n%s\n", sel)
	if req.Selection.AllowBeyond {
		u.WriteString("Browsing beyond the selection is allowed; bindings must stay inside it.\n")
	}
	if req.Document != "" {
		fmt.Fprintf(&u, "\n## Context document %s\n%s\n", req.DocumentName, req.Document)
	}
	if d := promptDraft(draft); d != nil && len(draft.Tree) > 0 {
		fmt.Fprintf(&u, "\n## Current draft (keep its node ids)\n%s\n", d)
	} else {
		u.WriteString("\n## Current draft\nThere is no draft yet.\n")
	}
	return b.String(), u.String()
}

func isa95Levels() []string {
	return []string{"enterprise", "site", "area", "workCenter", "processCell", "productionUnit", "productionLine", "storageZone",
		"workUnit", "unit", "workCell", "storageUnit", "equipmentModule", "controlModule"}
}

// Tools ---------------------------------------------------------------------

func toolParams(props map[string]string) map[string]any {
	p := map[string]any{}
	for name, spec := range props {
		typ, desc, _ := strings.Cut(spec, "|")
		p[name] = map[string]any{"type": typ, "description": desc}
	}
	return map[string]any{"type": "object", "properties": p}
}

type toolArgs struct {
	System  string `json:"system"`
	Pattern string `json:"pattern"`
	Type    string `json:"type"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	DPEs    string `json:"dpes"`
	Filter  string `json:"filter"`
}

func parseArgs(raw json.RawMessage) (toolArgs, error) {
	var a toolArgs
	if len(raw) == 0 {
		return a, nil
	}
	err := json.Unmarshal(raw, &a)
	return a, err
}

func toolJSON(v any) (string, error) {
	b, err := marshalNoEscape(v)
	if err != nil {
		return "", err
	}
	if len(b) > maxToolResultBytes {
		return string(b[:maxToolResultBytes]) + "\n… result truncated; narrow the request (pattern, limit, offset)", nil
	}
	return string(b), nil
}

// toolsA returns the read-only WinCC OA tools of a Direction A run, limited
// to the selection unless it allows browsing beyond it.
func (m *Manager) toolsA(sc *scope) []genai.Tool {
	return m.oaTools(sc, !sc.sel.AllowBeyond)
}

func (m *Manager) oaTools(sc *scope, restrict bool) []genai.Tool {
	b := m.browse
	return []genai.Tool{
		{
			Name:        "listTypes",
			Description: "Lists WinCC OA datapoint type names matching a dpTypes pattern (* and ? wildcards).",
			Parameters:  toolParams(map[string]string{"system": "string|remote system name; empty for the local system", "pattern": "string|type name pattern, default *"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				types, err := b.Types(ctx, a.System, a.Pattern)
				if err != nil {
					return "", err
				}
				if restrict {
					types = slices.DeleteFunc(types, func(t string) bool { return !sc.inType(a.System, t) })
				}
				if len(types) > maxToolNames {
					types = append(types[:maxToolNames], fmt.Sprintf("… %d more; narrow the pattern", len(types)-maxToolNames))
				}
				return toolJSON(types)
			},
		},
		{
			Name:        "getTypeStructure",
			Description: "Returns the element tree of a datapoint type: path, element type, value element or structure.",
			Parameters:  toolParams(map[string]string{"system": "string|remote system name; empty for the local system", "type": "string|datapoint type name"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				if restrict && !sc.inType(a.System, a.Type) {
					return "", fmt.Errorf("type %s is outside the selection", a.Type)
				}
				ts, err := b.TypeStructure(ctx, a.System, a.Type)
				if err != nil {
					return "", err
				}
				type el struct {
					Path        string `json:"path"`
					ElementType string `json:"elementType"`
					Value       bool   `json:"valueElement"`
				}
				out := make([]el, 0, len(ts.Elements))
				for _, e := range ts.Elements {
					out = append(out, el{e.Path, e.ElementType, e.ValueElement})
				}
				return toolJSON(map[string]any{"type": ts.Type, "system": ts.System, "elements": out})
			},
		},
		{
			Name:        "listDatapoints",
			Description: "Lists datapoints (name and type) matching a dpNames pattern, optionally of one type, sorted and paged.",
			Parameters: toolParams(map[string]string{"system": "string|remote system name; empty for the local system", "pattern": "string|datapoint name pattern, default *",
				"type": "string|datapoint type filter", "limit": "integer|page size, at most 200", "offset": "integer|page start"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				if a.Limit <= 0 || a.Limit > maxToolNames {
					a.Limit = maxToolNames
				}
				if !restrict {
					page, err := b.Datapoints(ctx, a.System, a.Pattern, a.Type, a.Limit, a.Offset)
					if err != nil {
						return "", err
					}
					return toolJSON(page)
				}
				pattern := a.Pattern
				if pattern == "" {
					pattern = "*"
				}
				names, err := b.MatchDatapoints(ctx, a.System, pattern, a.Type)
				if err != nil {
					return "", err
				}
				types, err := b.DatapointTypes(ctx, a.System, names)
				if err != nil {
					return "", err
				}
				var items []winccoabrowse.Datapoint
				for i, n := range names {
					if sc.touchesDP(a.System, n, types[i]) {
						items = append(items, winccoabrowse.Datapoint{Name: n, System: sc.sys(a.System), Type: types[i]})
					}
				}
				total := len(items)
				if a.Offset > total {
					a.Offset = total
				}
				end := min(a.Offset+a.Limit, total)
				return toolJSON(map[string]any{"total": total, "offset": a.Offset, "items": items[a.Offset:end]})
			},
		},
		{
			Name:        "getElementInfo",
			Description: "Returns element type, unit, description, format, current value, source time and status of DPEs. Pass one DPE per line (System:DP.element or DP.element), at most 200.",
			Parameters:  toolParams(map[string]string{"dpes": "string|DPE names, one per line"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				var dpes []string
				for _, l := range strings.Split(a.DPEs, "\n") {
					if l = strings.TrimSpace(l); l != "" {
						dpes = append(dpes, l)
					}
				}
				if len(dpes) > winccoabrowse.MaxElementInfoDPEs {
					return "", fmt.Errorf("at most %d DPEs per call", winccoabrowse.MaxElementInfoDPEs)
				}
				infos, err := b.ElementInfo(ctx, dpes)
				if err != nil {
					return "", err
				}
				type info struct {
					DPE         string `json:"dpe"`
					Error       string `json:"error,omitempty"`
					Type        string `json:"type,omitempty"`
					ElementType string `json:"elementType,omitempty"`
					Unit        string `json:"unit,omitempty"`
					Description string `json:"description,omitempty"`
					Format      string `json:"format,omitempty"`
					Value       any    `json:"value,omitempty"`
					STime       string `json:"stime,omitempty"`
				}
				out := make([]info, len(infos))
				for i, e := range infos {
					it := info{DPE: e.DPE, Error: e.Error}
					sys, rest := splitSys(e.DPE)
					dp, path, _ := strings.Cut(rest, ".")
					if restrict && e.Exists && !sc.inDPE(sys, dp, path, e.Type) {
						it.Error = "outside the selection"
						out[i] = it
						continue
					}
					if e.Exists {
						it.Type, it.ElementType, it.Unit, it.Description, it.Format = e.Type, e.ElementType, e.Unit, e.Description, e.Format
						if e.HasValue {
							it.Value = e.Value
							it.STime = e.STime.UTC().Format(time.RFC3339)
						}
					}
					out[i] = it
				}
				return toolJSON(out)
			},
		},
		{
			Name:        "listArchiveGroups",
			Description: "Lists WinCC OA value archive groups (name, dpName, displayName, kind: NGA/HDB/RDB) available for _archive configuration.",
			Parameters:  toolParams(map[string]string{"system": "string|remote system name; empty for the local system"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				groups, err := b.ArchiveGroups(ctx, a.System)
				if err != nil {
					return "", err
				}
				return toolJSON(groups)
			},
		},
	}
}

func splitSys(name string) (string, string) {
	if i := strings.Index(name, ":"); i >= 0 && !strings.Contains(name[:i], ".") {
		return name[:i], name[i+1:]
	}
	return "", name
}
