package unsmodeler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"monstermq.io/edge/internal/genai"
)

// TopicInput is the topic selection of a Direction B run (§5).
type TopicInput struct {
	Mode         string   `json:"mode"` // live or archive
	ArchiveGroup string   `json:"archiveGroup,omitempty"`
	Filters      []string `json:"topicFilters"`
	MaxTopics    int      `json:"maxTopics,omitempty"`
	// Topics are topics and values the dashboard collected live (optional).
	Topics map[string]string `json:"-"`
}

// TopicSource gathers topics and their last payloads, like analyzeTopics.
type TopicSource interface {
	// Collect returns up to max topics matching the filters: live from
	// retained and current messages, or from an archive group's last values.
	Collect(ctx context.Context, mode, archiveGroup string, filters []string, max int) (map[string][]byte, error)
	// Last returns the last payload of a topic (retained or last value).
	Last(ctx context.Context, topic string) ([]byte, bool)
	// ArchiveGroupExists reports a configured archive group.
	ArchiveGroupExists(name string) bool
}

// Subscriber gives the Direction B runtime an in-process subscription.
type Subscriber interface {
	Subscribe(filters []string, fn func(topic string, payload []byte, at time.Time)) (cancel func(), err error)
}

// SetTopicSource sets the topic source of Direction B.
func (m *Manager) SetTopicSource(src TopicSource) { m.topics = src }

// SetSubscriber sets the in-process subscriber of the Direction B runtime.
func (m *Manager) SetSubscriber(sub Subscriber) { m.subscriber = sub }

const (
	defaultMaxTopics = 500
	maxSampleBytes   = 600
)

func (m *Manager) collect(ctx context.Context, in *TopicInput) (map[string][]byte, error) {
	max := in.MaxTopics
	if max <= 0 {
		max = defaultMaxTopics
	}
	out := map[string][]byte{}
	for t, v := range in.Topics {
		if inFilters(in.Filters, t) && len(out) < max {
			out[t] = []byte(v)
		}
	}
	if m.topics != nil {
		got, err := m.topics.Collect(ctx, in.Mode, in.ArchiveGroup, in.Filters, max)
		if err != nil {
			return nil, err
		}
		for t, v := range got {
			if len(out) >= max {
				break
			}
			if _, ok := out[t]; !ok {
				out[t] = v
			}
		}
	}
	for t := range out {
		if strings.HasPrefix(t, "$") || UnderRoot(t, m.roots.Native) {
			delete(out, t)
		}
	}
	return out, nil
}

func sampleText(p []byte) string {
	s := string(p)
	if !utf8.ValidString(s) {
		return fmt.Sprintf("<%d bytes binary>", len(p))
	}
	if len(s) > maxSampleBytes {
		s = s[:maxSampleBytes] + "…"
	}
	return s
}

// toolsB returns the topic tools and the unrestricted WinCC OA tools of a
// Direction B run.
func (m *Manager) toolsB(ctx context.Context, sc *scope, in *TopicInput) ([]genai.Tool, error) {
	topics, err := m.collect(ctx, in)
	if err != nil {
		return nil, err
	}
	if len(topics) == 0 {
		return nil, fmt.Errorf("no topics found for %s", strings.Join(in.Filters, ", "))
	}
	names := make([]string, 0, len(topics))
	for t := range topics {
		names = append(names, t)
	}
	sort.Strings(names)
	match := func(filter string, limit int) []string {
		if filter == "" {
			filter = "#"
		}
		if limit <= 0 || limit > maxToolNames {
			limit = maxToolNames
		}
		var out []string
		for _, t := range names {
			if TopicMatch(filter, t) {
				out = append(out, t)
				if len(out) >= limit {
					break
				}
			}
		}
		return out
	}
	tools := []genai.Tool{
		{
			Name:        "browseTopics",
			Description: "Lists the analysed MQTT topics matching an MQTT topic filter (+ and # wildcards).",
			Parameters:  toolParams(map[string]string{"filter": "string|MQTT topic filter, default #", "limit": "integer|at most 200"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				list := match(a.Filter, a.Limit)
				total := 0
				for _, t := range names {
					if TopicMatch(defaultFilter(a.Filter), t) {
						total++
					}
				}
				return toolJSON(map[string]any{"total": total, "topics": list})
			},
		},
		{
			Name:        "sampleTopics",
			Description: "Returns the current payloads of the analysed topics matching an MQTT topic filter.",
			Parameters:  toolParams(map[string]string{"filter": "string|MQTT topic filter", "limit": "integer|at most 200"}),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := parseArgs(raw)
				if err != nil {
					return "", err
				}
				out := map[string]string{}
				for _, t := range match(a.Filter, a.Limit) {
					out[t] = sampleText(topics[t])
				}
				return toolJSON(out)
			},
		},
	}
	tools = append(tools, m.oaTools(sc, false)...)
	in.Topics = map[string]string{}
	for t, v := range topics {
		in.Topics[t] = string(v)
	}
	return tools, nil
}

func defaultFilter(f string) string {
	if f == "" {
		return "#"
	}
	return f
}

// promptB assembles system and user prompt of a Direction B run (§6.6).
func (m *Manager) promptB(s *Settings, req AiRunRequest, draft *Document) (string, string) {
	var b strings.Builder
	base := s.SystemPrompt
	if strings.TrimSpace(base) == "" {
		base = DefaultSystemPromptB
	}
	b.WriteString(base)
	schema, _ := PromptSchema(DirectionMqttToWinccoa)
	fmt.Fprintf(&b, "\n\n## Mapping document (JSON Schema)\n%s\n", schema)
	b.WriteString("\n## Metadata (fixed set)\nunit, description, format, pvRange (written to _pv_range), alert (written to _alert_hdl) and archive (written to _archive with a WinCC OA value archive group). " +
		"Type-level metadata is the default of every datapoint; datapoints[].elements[] overrides single items (pvRange, alert and archive as a whole).\n")
	fmt.Fprintf(&b, "\n## Limits\nDocument at most %d bytes, %d types, %d elements per type, %d datapoints, %d topic bindings.\n",
		MaxAIDocumentBytes, MaxTypes, MaxTypeElements, MaxDatapoints, MaxTopicBindingsB)
	b.WriteString("\n## Rules\n" +
		"- New types and all datapoints start with the prefix " + quoteJSON(s.OaPrefix) + ". Reused (existing) types keep their name.\n" +
		"- Bind only topics of the analysed selection. Keep the type and datapoint names of the current draft.\n" +
		"- Do not write source, aiRun or excluded; the server sets them.\n")
	if s.OaArchiveEnabled && s.OaArchiveGroup != "" {
		fmt.Fprintf(&b, "- Device default archiving is enabled with WinCC OA archive group %q. All created datapoint elements will be archived by default; set element archive metadata only when a different group or enabled: false is needed.\n", s.OaArchiveGroup)
	} else {
		b.WriteString("- WinCC OA archiving is disabled for this device. Do not add archive metadata; archive metadata in your answer is removed.\n")
	}
	b.WriteString("- Alert classes are names of _AlertClass datapoints, e.g. alert or warning.\n" +
		"- Source time: when a JSON payload has a field that holds the time of a value, set timePath to that field and timeFormat " +
		"to iso8601 (a string like 2026-10-14T09:12:30.250Z), epochMs (milliseconds since 1970, about 1.7e12 today) or epochS (seconds, about 1.7e9). " +
		"Use the timestamp that belongs to the value: a timestamp in the same object as the value (e.g. $.temp.ts for $.temp.value) wins, " +
		"else a timestamp of the whole payload (e.g. $.TimeMS for $.Value). Without a related timestamp omit timePath; the receive time is used. " +
		"Do not create datapoint elements for these timestamps.\n" +
		"- The final answer is the complete document that passed validate without errors (only the JSON, no explanation), " +
		"or the single word VALIDATED to use the last document that passed validate without errors.\n")
	var u strings.Builder
	if strings.TrimSpace(req.Prompt) != "" {
		u.WriteString(req.Prompt)
		u.WriteString("\n\n")
	}
	in := req.TopicInput
	fmt.Fprintf(&u, "## Selection\nmode %s, topic filters %s\n", in.Mode, strings.Join(in.Filters, ", "))
	values := map[string]string{}
	for t, v := range in.Topics {
		values[t] = sampleText([]byte(v))
	}
	if len(values) > 0 {
		fmt.Fprintf(&u, "\n## Topics (%d, with current payloads)\n%s\n", len(values), genai.FormatTopicsAsHierarchy(capTopics(values, 200)))
		if len(values) > 200 {
			u.WriteString("Only the first 200 topics are listed; use browseTopics and sampleTopics for the others.\n")
		}
	}
	if req.Document != "" {
		fmt.Fprintf(&u, "\n## Context document %s\n%s\n", req.DocumentName, req.Document)
	}
	if d := promptDraftB(draft); d != nil {
		fmt.Fprintf(&u, "\n## Current draft (keep its names)\n%s\n", d)
	} else {
		u.WriteString("\n## Current draft\nThere is no draft yet.\n")
	}
	return b.String(), u.String()
}

func capTopics(in map[string]string, n int) map[string]string {
	if len(in) <= n {
		return in
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]string{}
	for _, k := range keys[:n] {
		out[k] = in[k]
	}
	return out
}

// promptDraftB returns the draft without source, aiRun and excluded.
func promptDraftB(doc *Document) []byte {
	if doc == nil || (len(doc.Types) == 0 && len(doc.Datapoints) == 0) {
		return nil
	}
	cp, _ := ParseDocument(mustEncode(doc))
	cp.AiRun, cp.Source = nil, nil
	for _, t := range cp.Types {
		t.Excluded = false
	}
	for _, d := range cp.Datapoints {
		d.Excluded = false
	}
	b, _ := json.Marshal(struct {
		SchemaVersion int             `json:"schemaVersion"`
		Direction     string          `json:"direction"`
		Types         []*TypeProposal `json:"types"`
		Datapoints    []*Datapoint    `json:"datapoints"`
	}{1, DirectionMqttToWinccoa, cp.Types, cp.Datapoints})
	return b
}

// stampB sets source, aiRun and the excluded flags of the draft on an AI
// (or imported) Direction B document, before validation. Key order and
// number spelling of the document are kept. With stripArchive, element
// archive metadata of the document is dropped: while device archiving is
// disabled only archive settings edited in the draft count.
func stampB(raw []byte, draft *Document, in *TopicInput, run *AiRun, stripArchive bool) ([]byte, error) {
	v, _, ok := decodeDocument(raw)
	if !ok {
		return nil, fmt.Errorf("not valid JSON")
	}
	root, isObj := v.(*jsonObject)
	if !isObj {
		return nil, fmt.Errorf("not a JSON object")
	}
	filters := []string{}
	if draft != nil && draft.Source != nil {
		filters = append(filters, draft.Source.TopicFilters...)
	}
	for _, f := range in.Filters {
		if !slices.Contains(filters, f) {
			filters = append(filters, f)
		}
	}
	src := Source{Mode: in.Mode, TopicFilters: filters}
	if src.Mode == "" {
		src.Mode = "live"
	}
	if src.Mode == "archive" {
		src.ArchiveGroup = in.ArchiveGroup
	}
	setKey(root, "source", toOrdered(src), "direction")
	if run != nil {
		r := *run
		r.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		setKey(root, "aiRun", toOrdered(r), "")
	}
	excludedTypes, excludedDPs := map[string]bool{}, map[string]bool{}
	var keepTypes, keepDPs []any
	if draft != nil {
		for _, t := range draft.Types {
			if t.Excluded {
				excludedTypes[t.Name] = true
				keepTypes = append(keepTypes, toOrdered(t))
			}
		}
		for _, d := range draft.Datapoints {
			if d.Excluded {
				excludedDPs[d.Name] = true
				keepDPs = append(keepDPs, toOrdered(d))
			}
		}
	}
	restamp := func(key string, excluded map[string]bool, keep []any) {
		items, _ := root.vals[key].([]any)
		seen := map[string]bool{}
		for _, it := range items {
			o, ok := it.(*jsonObject)
			if !ok {
				continue
			}
			deleteKey(o, "excluded")
			if stripArchive {
				if elems, ok := o.vals["elements"].([]any); ok {
					kept := elems[:0]
					for _, e := range elems {
						if eo, ok := e.(*jsonObject); ok {
							deleteKey(eo, "archive")
							// A datapoint element with nothing but its path is invalid.
							if len(eo.keys) == 1 && eo.keys[0] == "path" {
								continue
							}
						}
						kept = append(kept, e)
					}
					if len(kept) == 0 {
						deleteKey(o, "elements")
					} else {
						o.vals["elements"] = kept
					}
				}
			}
			name, _ := o.vals["name"].(string)
			seen[name] = true
			if excluded[name] {
				setKey(o, "excluded", true, "name")
			}
		}
		for _, k := range keep {
			if name, _ := k.(*jsonObject).vals["name"].(string); !seen[name] {
				items = append(items, k)
			}
		}
		if _, present := root.vals[key]; present || len(items) > 0 {
			if items == nil {
				items = []any{}
			}
			setKey(root, key, items, "")
		}
	}
	restamp("types", excludedTypes, keepTypes)
	restamp("datapoints", excludedDPs, keepDPs)
	var b bytes.Buffer
	encodeOrdered(&b, root)
	return b.Bytes(), nil
}

// toOrdered converts a value into the ordered document form.
func toOrdered(v any) any {
	b, _ := marshalNoEscape(v)
	out, _, _ := decodeDocument(b)
	return out
}

// setKey sets a key of an ordered object; a new key goes after the key
// after (or to the end).
func setKey(o *jsonObject, key string, v any, after string) {
	if _, ok := o.vals[key]; !ok {
		i := len(o.keys)
		if after != "" {
			if j := slices.Index(o.keys, after); j >= 0 {
				i = j + 1
			}
		}
		o.keys = slices.Insert(o.keys, i, key)
	}
	o.vals[key] = v
}

func deleteKey(o *jsonObject, key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

// prepareB runs the AI answer preparation of a Direction B document (§5):
// strip fences and $schema, stamp source, aiRun and excluded, check the
// run's topic filters against the draft, and validate.
func (m *Manager) prepareB(ctx context.Context, device string, s *Settings, text string, draft *Document, in *TopicInput, run AiRun) PreparedAnswer {
	return m.prepareBDoc(ctx, device, s, []byte(stripAnswer(text)), draft, in, &run)
}

func (m *Manager) prepareBDoc(ctx context.Context, device string, s *Settings, raw []byte, draft *Document, in *TopicInput, run *AiRun) PreparedAnswer {
	raw = stripSchemaKey(raw)
	if len(raw) > MaxAIDocumentBytes && run != nil {
		return PreparedAnswer{Issues: []Issue{{Severity: SeverityError, Message: fmt.Sprintf("document has %d bytes, more than the AI run limit of %d", len(raw), MaxAIDocumentBytes)}}}
	}
	if is := fatalIssue(raw); is != nil {
		return PreparedAnswer{Issues: []Issue{*is}}
	}
	stamped, err := stampB(raw, draft, in, run, !s.OaArchiveEnabled)
	if err != nil {
		return PreparedAnswer{Issues: []Issue{{Severity: SeverityError, Message: "the document must be a JSON object: " + err.Error()}}}
	}
	if issues, _ := ValidateSchema(stamped); len(issues) > 0 {
		out := make([]Issue, len(issues))
		for i, is := range issues {
			out[i] = Issue{Severity: SeverityError, Path: is.Path, Message: is.Message}
		}
		return PreparedAnswer{Doc: stamped, Issues: out}
	}
	doc, err := ParseDocument(stamped)
	if err != nil {
		return PreparedAnswer{Issues: []Issue{{Severity: SeverityError, Message: err.Error()}}}
	}
	var ctxIssues []Issue
	if run != nil {
		before := map[string]*Datapoint{}
		if draft != nil {
			for _, d := range draft.Datapoints {
				before[d.Name] = d
			}
		}
		outside := func(d *Datapoint) bool {
			for _, b := range d.Bindings {
				if !inFilters(in.Filters, b.Topic) {
					return true
				}
			}
			return false
		}
		after := map[string]bool{}
		for i, d := range doc.Datapoints {
			after[d.Name] = true
			old := before[d.Name]
			for j, b := range d.Bindings {
				if inFilters(in.Filters, b.Topic) {
					continue
				}
				changed := old == nil || !slices.ContainsFunc(old.Bindings, func(o *TopicBinding) bool { return *o == *b })
				if changed {
					ctxIssues = append(ctxIssues, Issue{Severity: SeverityError, Path: fmt.Sprintf("/datapoints/%d/bindings/%d/topic", i, j),
						Message: fmt.Sprintf("AI run context: topic %s is outside the run's topic filters", quoteJSON(b.Topic))})
				}
			}
			if old != nil && outside(old) {
				ob, _ := json.Marshal(old)
				nb, _ := json.Marshal(d)
				if !bytes.Equal(ob, nb) {
					ctxIssues = append(ctxIssues, Issue{Severity: SeverityError, Path: fmt.Sprintf("/datapoints/%d", i),
						Message: fmt.Sprintf("AI run context: datapoint %s has bindings outside the run's topic filters and must stay unchanged", quoteJSON(d.Name))})
				}
			}
		}
		for name, old := range before {
			if !after[name] && outside(old) {
				ctxIssues = append(ctxIssues, Issue{Severity: SeverityError, Path: "/datapoints",
					Message: fmt.Sprintf("AI run context: datapoint %s has bindings outside the run's topic filters and cannot be removed", quoteJSON(name))})
			}
		}
	}
	env := m.Env(device, s)
	if in.Topics != nil {
		inner := env.Sample
		env.Sample = func(topic string) ([]byte, bool) {
			if v, ok := in.Topics[topic]; ok {
				return []byte(v), true
			}
			if inner != nil {
				return inner(topic)
			}
			return nil, false
		}
	}
	res, err := Validate(ctx, stamped, env)
	if err != nil {
		return PreparedAnswer{Doc: stamped, Issues: append(ctxIssues, Issue{Severity: SeverityError, Message: err.Error()})}
	}
	return PreparedAnswer{Doc: stamped, Issues: capIssues(append(ctxIssues, res.Issues...))}
}

// ImportProposal stores a hand-written Direction B document as the draft:
// the server stamps source from the topic selection and validates it like
// an AI answer (§7 B1 Import JSON).
func (m *Manager) ImportProposal(ctx context.Context, name string, raw []byte, in TopicInput) ([]byte, []Issue, error) {
	_, s, err := m.deviceConfig(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if s.Direction != DirectionMqttToWinccoa {
		return nil, nil, fmt.Errorf("import is for Direction B devices")
	}
	if len(in.Filters) == 0 {
		return nil, nil, fmt.Errorf("select the analysed topics first")
	}
	draftRaw, _ := m.getDoc(ctx, name, SlotDraft)
	var draft *Document
	if draftRaw != nil {
		if issues, _ := ValidateSchema(draftRaw); len(issues) == 0 {
			draft, _ = ParseDocument(draftRaw)
		}
	}
	p := m.prepareBDoc(ctx, name, s, raw, draft, &in, nil)
	if p.Doc == nil {
		return nil, p.Issues, ErrInvalidDocument
	}
	if err := m.saveDoc(ctx, name, SlotDraft, p.Doc); err != nil {
		return nil, p.Issues, err
	}
	m.log.Info("uns proposal imported", "device", name, "issues", len(p.Issues))
	return p.Doc, p.Issues, nil
}
