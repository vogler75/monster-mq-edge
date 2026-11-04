package resolvers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/unsmodeler"
)

type unsModelerMutationsResolver struct{ *Resolver }
type unsModelerDeviceResolver struct{ *Resolver }

func (r *Resolver) UnsModelerMutations() generated.UnsModelerMutationsResolver {
	return &unsModelerMutationsResolver{r}
}

func (r *Resolver) UnsModelerDevice() generated.UnsModelerDeviceResolver {
	return &unsModelerDeviceResolver{r}
}

func (r *mutationResolver) UnsModeler(ctx context.Context) (*generated.UnsModelerMutations, error) {
	return &generated.UnsModelerMutations{}, nil
}

var errUnsNotEmbedded = errors.New("UNS Modeler devices need the broker embedded in WinCC OA (WCCOAmmq)")

func unsDirection(d string) generated.UnsModelerDirection {
	if d == unsmodeler.DirectionMqttToWinccoa {
		return generated.UnsModelerDirectionMqttToWinccoa
	}
	return generated.UnsModelerDirectionWinccoaToUns
}

func unsDirectionString(d generated.UnsModelerDirection) string {
	if d == generated.UnsModelerDirectionMqttToWinccoa {
		return unsmodeler.DirectionMqttToWinccoa
	}
	return unsmodeler.DirectionWinccoaToUns
}

func unsIssues(in []unsmodeler.Issue) []*generated.UnsModelerIssue {
	out := make([]*generated.UnsModelerIssue, len(in))
	for i, is := range in {
		sev := generated.UnsModelerSeverityError
		switch is.Severity {
		case unsmodeler.SeverityWarning:
			sev = generated.UnsModelerSeverityWarning
		case unsmodeler.SeverityInfo:
			sev = generated.UnsModelerSeverityInfo
		}
		out[i] = &generated.UnsModelerIssue{Severity: sev, Path: is.Path, Message: is.Message}
	}
	return out
}

func (r *Resolver) unsDevice(d stores.DeviceConfig) *generated.UnsModelerDevice {
	s, err := unsmodeler.ParseSettings(d.Config)
	if err != nil {
		s, _ = unsmodeler.ParseSettings("")
	}
	cfg := &generated.UnsModelerConfig{
		Direction:        unsDirection(s.Direction),
		ApprovalMode:     generated.UnsModelerApprovalMode(strings.ToUpper(s.ApprovalMode)),
		SystemPrompt:     optString(s.SystemPrompt),
		AiMaxSteps:       s.AiMaxSteps,
		AiTimeoutSec:     s.AiTimeoutSec,
		AiTokenBudget:    s.AiTokenBudget,
		Redundancy:       generated.UnsModelerRedundancy(s.Redundancy),
		UnsRoot:          optString(s.UnsRoot),
		AllowWrite:       s.AllowWrite,
		OaPrefix:         optString(s.OaPrefix),
		FlowEnabled:      s.Flow(),
		OaArchiveEnabled: s.OaArchiveEnabled,
		OaArchiveGroup:   optString(s.OaArchiveGroup),
	}
	if len(s.TopicDefaults) > 0 {
		cfg.TopicDefaults = optString(string(s.TopicDefaults))
	}
	if len(s.PayloadDefaults) > 0 {
		cfg.PayloadDefaults = optString(string(s.PayloadDefaults))
	}
	return &generated.UnsModelerDevice{
		Name: d.Name, Namespace: d.Namespace, NodeID: d.NodeID, Enabled: d.Enabled,
		CreatedAt: formatTime(d.CreatedAt), UpdatedAt: formatTime(d.UpdatedAt),
		IsOnCurrentNode: d.NodeID == r.NodeID || d.NodeID == "*" || d.NodeID == "local" || d.NodeID == "",
		Config:          cfg,
	}
}

// Query: unsModelers --------------------------------------------------------

func (r *queryResolver) UnsModelers(ctx context.Context, name *string) ([]*generated.UnsModelerDevice, error) {
	out := []*generated.UnsModelerDevice{}
	if r.Storage == nil || r.Storage.DeviceConfig == nil {
		return out, nil
	}
	devs, err := r.Storage.DeviceConfig.GetByType(ctx, unsmodeler.DeviceType)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if name != nil && *name != "" && d.Name != *name {
			continue
		}
		out = append(out, r.unsDevice(d))
	}
	return out, nil
}

func (r *queryResolver) UnsModelerValidate(ctx context.Context, name string, document string) ([]*generated.UnsModelerIssue, error) {
	if r.UnsMgr == nil {
		return nil, errUnsNotEmbedded
	}
	issues, err := r.UnsMgr.ValidateDocument(ctx, name, []byte(document))
	if err != nil {
		return nil, err
	}
	return unsIssues(issues), nil
}

func (r *queryResolver) UnsModelerSchema(ctx context.Context, direction *generated.UnsModelerDirection) (string, error) {
	if direction == nil {
		return string(unsmodeler.MappingSchema()), nil
	}
	b, err := unsmodeler.PromptSchema(unsDirectionString(*direction))
	return string(b), err
}

// Device fields ------------------------------------------------------------

func (r *unsModelerDeviceResolver) Status(ctx context.Context, obj *generated.UnsModelerDevice) (*generated.UnsModelerStatus, error) {
	if r.UnsMgr == nil {
		return &generated.UnsModelerStatus{Active: true}, nil
	}
	st := r.UnsMgr.Status(obj.Name)
	out := &generated.UnsModelerStatus{Running: st.Running, Active: st.Active, Topics: st.Topics, Dpes: st.DPEs, Published: st.Published,
		Writes: st.Writes, Errors: st.Errors, ApplyError: optString(st.ApplyError)}
	if !st.LastApplied.IsZero() {
		out.LastApplied = optString(formatTime(st.LastApplied))
	}
	return out, nil
}

func (r *unsModelerDeviceResolver) Draft(ctx context.Context, obj *generated.UnsModelerDevice) (*string, error) {
	return r.unsDoc(ctx, obj.Name, unsmodeler.SlotDraft)
}

func (r *unsModelerDeviceResolver) Applied(ctx context.Context, obj *generated.UnsModelerDevice) (*string, error) {
	return r.unsDoc(ctx, obj.Name, unsmodeler.SlotApplied)
}

func (r *Resolver) unsDoc(ctx context.Context, name, slot string) (*string, error) {
	if r.Storage == nil || r.Storage.DeviceConfig == nil {
		return nil, nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || d == nil {
		return nil, err
	}
	doc := unsmodeler.ConfigDocument(d.Config, slot)
	if doc == nil {
		return nil, nil
	}
	s := string(doc)
	return &s, nil
}

func (r *unsModelerDeviceResolver) LiveTopics(ctx context.Context, obj *generated.UnsModelerDevice) ([]*generated.UnsModelerTopicStatus, error) {
	out := []*generated.UnsModelerTopicStatus{}
	if r.UnsMgr == nil {
		return out, nil
	}
	for _, t := range r.UnsMgr.LiveTopics(obj.Name) {
		g := &generated.UnsModelerTopicStatus{Topic: t.Topic, Dpe: t.DPE, Published: t.Published, LastError: optString(t.LastError)}
		if t.HasValue {
			b, _ := json.Marshal(t.Value)
			g.Value = optString(string(b))
		}
		if t.Status != nil {
			s := int64(*t.Status)
			g.Status = &s
		}
		if !t.LastPublish.IsZero() {
			g.LastPublish = optString(t.LastPublish.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		if !t.SourceTime.IsZero() {
			g.SourceTime = optString(t.SourceTime.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		if !t.LastErrorAt.IsZero() {
			g.LastErrorAt = optString(t.LastErrorAt.UTC().Format(time.RFC3339))
		}
		out = append(out, g)
	}
	return out, nil
}

// Mutations ----------------------------------------------------------------

// unsSettings builds the device settings from the input; current is the
// stored configuration (nil on create).
func unsSettings(in *generated.UnsModelerConfigInput, current *unsmodeler.Settings) *unsmodeler.Settings {
	s := &unsmodeler.Settings{}
	if current != nil {
		*s = *current
	}
	s.Direction = unsDirectionString(in.Direction)
	if in.ApprovalMode != nil {
		s.ApprovalMode = strings.ToLower(string(*in.ApprovalMode))
	}
	if in.SystemPrompt != nil {
		s.SystemPrompt = *in.SystemPrompt
	}
	if in.AiMaxSteps != nil {
		s.AiMaxSteps = *in.AiMaxSteps
	}
	if in.AiTimeoutSec != nil {
		s.AiTimeoutSec = *in.AiTimeoutSec
	}
	if in.AiTokenBudget != nil {
		s.AiTokenBudget = *in.AiTokenBudget
	}
	if in.Redundancy != nil {
		s.Redundancy = string(*in.Redundancy)
	}
	if in.UnsRoot != nil {
		s.UnsRoot = *in.UnsRoot
	}
	if in.AllowWrite != nil {
		s.AllowWrite = *in.AllowWrite
	}
	if in.TopicDefaults != nil {
		s.TopicDefaults = json.RawMessage(strings.TrimSpace(*in.TopicDefaults))
	}
	if in.PayloadDefaults != nil {
		s.PayloadDefaults = json.RawMessage(strings.TrimSpace(*in.PayloadDefaults))
	}
	if in.OaPrefix != nil {
		s.OaPrefix = *in.OaPrefix
	}
	if in.FlowEnabled != nil {
		v := *in.FlowEnabled
		s.FlowEnabled = &v
	}
	if in.OaArchiveEnabled != nil {
		s.OaArchiveEnabled = *in.OaArchiveEnabled
	}
	if in.OaArchiveGroup != nil {
		s.OaArchiveGroup = *in.OaArchiveGroup
	}
	// Re-parse to apply the defaults.
	out, _ := unsmodeler.ParseSettings(s.Encode())
	return out
}

func (r *Resolver) unsRoots() unsmodeler.TopicRoots {
	if r.UnsMgr != nil {
		return r.UnsMgr.Roots()
	}
	root := "winccoa"
	if r.NativeNames != nil {
		root = r.NativeNames().Root
	}
	return unsmodeler.TopicRoots{Native: root}
}

func (r *Resolver) reloadUns(ctx context.Context) {
	if r.UnsMgr != nil {
		if err := r.UnsMgr.Reload(ctx); err != nil {
			r.Logger.Warn("uns modeler reload failed", "err", err)
		}
	}
}

func (r *unsModelerMutationsResolver) save(ctx context.Context, input generated.UnsModelerDeviceInput, existing *stores.DeviceConfig) (*generated.UnsModelerResult, error) {
	fail := func(errs ...string) *generated.UnsModelerResult {
		return &generated.UnsModelerResult{Success: false, Errors: errs, Issues: []*generated.UnsModelerIssue{}}
	}
	if r.UnsMgr == nil {
		return fail(errUnsNotEmbedded.Error()), nil
	}
	if input.Config == nil {
		return fail("config is required"), nil
	}
	if !providerNameRegex.MatchString(input.Name) {
		return fail("name must contain only letters, digits, '_' and '-'"), nil
	}
	var current *unsmodeler.Settings
	d := stores.DeviceConfig{Name: input.Name, Namespace: "uns-modeler", NodeID: r.NodeID, Type: unsmodeler.DeviceType, Enabled: true}
	if existing != nil {
		cur, err := unsmodeler.ParseSettings(existing.Config)
		if err == nil {
			current = cur
		}
		if current != nil && current.Direction != unsDirectionString(input.Config.Direction) {
			return fail("the direction of a device is fixed at creation and cannot be changed"), nil
		}
		d = *existing
	}
	if input.Namespace != nil && *input.Namespace != "" {
		d.Namespace = *input.Namespace
	}
	if input.NodeID != nil && *input.NodeID != "" {
		d.NodeID = *input.NodeID
	}
	if input.Enabled != nil {
		d.Enabled = *input.Enabled
	}
	s := unsSettings(input.Config, current)
	issues := unsmodeler.ValidateSettings(s, r.unsRoots())
	if unsmodeler.HasErrors(issues) {
		res := fail("invalid device settings")
		res.Issues = unsIssues(issues)
		return res, nil
	}
	d.Config = s.Encode()
	if err := r.UnsMgr.SaveDevice(ctx, d); err != nil {
		return fail(err.Error()), nil
	}
	saved, _ := r.Storage.DeviceConfig.Get(ctx, d.Name)
	if saved == nil {
		saved = &d
	}
	r.reloadUns(ctx)
	return &generated.UnsModelerResult{Success: true, Device: r.unsDevice(*saved), Errors: []string{}, Issues: unsIssues(issues)}, nil
}

func (r *unsModelerMutationsResolver) Create(ctx context.Context, _ *generated.UnsModelerMutations, input generated.UnsModelerDeviceInput) (*generated.UnsModelerResult, error) {
	if existing, _ := r.Storage.DeviceConfig.Get(ctx, input.Name); existing != nil {
		return &generated.UnsModelerResult{Success: false, Errors: []string{fmt.Sprintf("Device with name %q already exists", input.Name)}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	return r.save(ctx, input, nil)
}

func (r *unsModelerMutationsResolver) Update(ctx context.Context, _ *generated.UnsModelerMutations, name string, input generated.UnsModelerDeviceInput) (*generated.UnsModelerResult, error) {
	if name != input.Name {
		return &generated.UnsModelerResult{Success: false, Errors: []string{"name in path must match name in input"}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if existing == nil || existing.Type != unsmodeler.DeviceType {
		return &generated.UnsModelerResult{Success: false, Errors: []string{fmt.Sprintf("UNS Modeler device %q not found", name)}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	return r.save(ctx, input, existing)
}

func (r *unsModelerMutationsResolver) Delete(ctx context.Context, _ *generated.UnsModelerMutations, name string) (bool, error) {
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil {
		return false, err
	}
	if existing == nil || existing.Type != unsmodeler.DeviceType {
		return false, nil
	}
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, err
	}
	if r.UnsMgr != nil {
		err = r.UnsMgr.Delete(ctx, name)
	}
	r.reloadUns(ctx)
	return err == nil, err
}

func (r *unsModelerMutationsResolver) Toggle(ctx context.Context, _ *generated.UnsModelerMutations, name string, enabled bool) (*generated.UnsModelerResult, error) {
	d, err := r.Storage.DeviceConfig.Toggle(ctx, name, enabled)
	if err != nil {
		return &generated.UnsModelerResult{Success: false, Errors: []string{err.Error()}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	if d == nil || d.Type != unsmodeler.DeviceType {
		return &generated.UnsModelerResult{Success: false, Errors: []string{fmt.Sprintf("UNS Modeler device %q not found", name)}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	r.reloadUns(ctx)
	return &generated.UnsModelerResult{Success: true, Device: r.unsDevice(*d), Errors: []string{}, Issues: []*generated.UnsModelerIssue{}}, nil
}

func (r *unsModelerMutationsResolver) SaveDraft(ctx context.Context, _ *generated.UnsModelerMutations, name string, document string) (*generated.UnsModelerMappingResult, error) {
	if r.UnsMgr == nil {
		return &generated.UnsModelerMappingResult{Errors: []string{errUnsNotEmbedded.Error()}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	doc, issues, err := r.UnsMgr.SaveDraft(ctx, name, []byte(document))
	res := &generated.UnsModelerMappingResult{Stored: doc != nil, Errors: []string{}, Issues: unsIssues(issues)}
	if doc != nil {
		res.Document = optString(string(doc))
	}
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	res.Success = true
	return res, nil
}

func (r *unsModelerMutationsResolver) Apply(ctx context.Context, _ *generated.UnsModelerMutations, name string) (*generated.UnsModelerMappingResult, error) {
	if r.UnsMgr == nil {
		return &generated.UnsModelerMappingResult{Errors: []string{errUnsNotEmbedded.Error()}, Issues: []*generated.UnsModelerIssue{}}, nil
	}
	issues, err := r.UnsMgr.Apply(ctx, name)
	res := &generated.UnsModelerMappingResult{Errors: []string{}, Issues: unsIssues(issues)}
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	if unsmodeler.HasErrors(issues) {
		res.Errors = append(res.Errors, "the draft has validation errors and was not applied")
		return res, nil
	}
	res.Success, res.Stored = true, true
	if doc, _ := r.UnsMgr.Applied(ctx, name); doc != nil {
		res.Document = optString(string(doc))
	}
	return res, nil
}

// AI runs -------------------------------------------------------------------

func unsAiRun(st *unsmodeler.AiRunStatus) *generated.UnsModelerAiRun {
	if st == nil {
		return nil
	}
	out := &generated.UnsModelerAiRun{
		ID: st.ID, Status: strings.ToUpper(st.Status), Provider: st.Provider, Model: optString(st.Model),
		StartedAt: formatTime(st.Started), Steps: st.Steps, ToolCalls: st.ToolCalls, InputTokens: st.InputTokens,
		OutputTokens: st.OutputTokens, Error: optString(st.Error), Applied: st.Applied, Issues: unsIssues(st.Issues),
		Events: make([]*generated.UnsModelerAiEvent, len(st.Events)),
	}
	if !st.Finished.IsZero() {
		out.FinishedAt = optString(formatTime(st.Finished))
	}
	for i, e := range st.Events {
		out.Events[i] = &generated.UnsModelerAiEvent{Time: formatTime(e.Time), Kind: e.Kind, Name: optString(e.Name), Detail: optString(e.Detail), Error: optString(e.Error)}
	}
	return out
}

func (r *unsModelerDeviceResolver) AiRun(ctx context.Context, obj *generated.UnsModelerDevice) (*generated.UnsModelerAiRun, error) {
	if r.UnsMgr == nil {
		return nil, nil
	}
	return unsAiRun(r.UnsMgr.AiRun(obj.Name)), nil
}

func (r *queryResolver) UnsModelerDefaultPrompt(ctx context.Context, direction generated.UnsModelerDirection) (string, error) {
	if direction == generated.UnsModelerDirectionMqttToWinccoa {
		return unsmodeler.DefaultSystemPromptB, nil
	}
	return unsmodeler.DefaultSystemPromptA, nil
}

func (r *unsModelerMutationsResolver) StartAiRun(ctx context.Context, _ *generated.UnsModelerMutations, name string, input generated.UnsModelerAiRunInput) (*generated.UnsModelerAiRunResult, error) {
	fail := func(err error) *generated.UnsModelerAiRunResult {
		return &generated.UnsModelerAiRunResult{Success: false, Errors: []string{err.Error()}}
	}
	if r.UnsMgr == nil {
		return fail(errUnsNotEmbedded), nil
	}
	req := unsmodeler.AiRunRequest{Prompt: derefStr(input.Prompt), DocumentName: derefStr(input.DocumentName)}
	if b64 := derefStr(input.DocumentBase64); b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fail(fmt.Errorf("documentBase64: %w", err)), nil
		}
		text, err := unsmodeler.ExtractDocumentText(req.DocumentName, data)
		if err != nil {
			return fail(fmt.Errorf("context document: %w", err)), nil
		}
		req.Document = text
	}
	for _, it := range input.Selection {
		req.Selection.Items = append(req.Selection.Items, unsmodeler.SelectionItem{Kind: it.Kind, System: derefStr(it.System), Type: it.Type,
			Datapoint: derefStr(it.Datapoint), Path: derefStr(it.Path), Pattern: derefStr(it.Pattern)})
	}
	req.Selection.AllowBeyond = input.AllowBeyondSelection != nil && *input.AllowBeyondSelection
	if len(input.TopicFilters) > 0 {
		ti := &unsmodeler.TopicInput{Mode: strings.ToLower(derefStr(input.TopicMode)), ArchiveGroup: derefStr(input.ArchiveGroup), Filters: input.TopicFilters}
		if ti.Mode == "" {
			ti.Mode = "live"
		}
		if input.MaxTopics != nil {
			ti.MaxTopics = *input.MaxTopics
		}
		ti.Topics = unsTopicData(input.TopicData)
		req.TopicInput = ti
	}
	st, err := r.UnsMgr.StartAiRun(ctx, name, req)
	if err != nil {
		return fail(err), nil
	}
	return &generated.UnsModelerAiRunResult{Success: true, Errors: []string{}, Run: unsAiRun(st)}, nil
}

func (r *unsModelerMutationsResolver) CancelAiRun(ctx context.Context, _ *generated.UnsModelerMutations, name string) (*generated.UnsModelerAiRunResult, error) {
	if r.UnsMgr == nil {
		return &generated.UnsModelerAiRunResult{Success: false, Errors: []string{errUnsNotEmbedded.Error()}}, nil
	}
	st, err := r.UnsMgr.CancelAiRun(name)
	if err != nil {
		return &generated.UnsModelerAiRunResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	return &generated.UnsModelerAiRunResult{Success: true, Errors: []string{}, Run: unsAiRun(st)}, nil
}

// Direction B -------------------------------------------------------------

func (r *unsModelerDeviceResolver) CreatePlan(ctx context.Context, obj *generated.UnsModelerDevice) (*generated.UnsModelerCreatePlan, error) {
	if obj.Config == nil || obj.Config.Direction != generated.UnsModelerDirectionMqttToWinccoa || r.Storage.DeviceConfig == nil {
		return nil, nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, obj.Name)
	if err != nil || d == nil {
		return nil, err
	}
	raw := unsmodeler.ConfigDocument(d.Config, unsmodeler.SlotDraft)
	if raw == nil {
		return nil, nil
	}
	if issues, _ := unsmodeler.ValidateSchema(raw); len(issues) > 0 {
		return nil, nil
	}
	doc, err := unsmodeler.ParseDocument(raw)
	if err != nil {
		return nil, nil
	}
	s, _ := unsmodeler.ParseSettings(d.Config)
	p := unsmodeler.Plan(doc, s)
	return &generated.UnsModelerCreatePlan{Types: p.Types, Datapoints: p.Datapoints, ConfigWrites: p.ConfigWrites, Excluded: p.Excluded}, nil
}

func (r *unsModelerMutationsResolver) CreateInWinCCOa(ctx context.Context, _ *generated.UnsModelerMutations, name string) (*generated.UnsModelerCreateResult, error) {
	res := &generated.UnsModelerCreateResult{Errors: []string{}, Issues: []*generated.UnsModelerIssue{}, Objects: []*generated.UnsModelerObjectResult{}}
	if r.UnsMgr == nil {
		res.Errors = append(res.Errors, errUnsNotEmbedded.Error())
		return res, nil
	}
	out, issues, err := r.UnsMgr.CreateInWinCCOa(ctx, name)
	res.Issues = unsIssues(issues)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	if out == nil {
		if err == nil {
			res.Errors = append(res.Errors, "the draft has validation errors; nothing was created")
		}
		return res, nil
	}
	for _, o := range out.Objects {
		res.Objects = append(res.Objects, &generated.UnsModelerObjectResult{Kind: o.Kind, Name: o.Name, Status: o.Status, Message: optString(o.Message), ConfigWrites: o.ConfigWrites})
	}
	res.ConfigWrites = out.ConfigWrites
	res.Success = err == nil && out.Failed == 0
	return res, nil
}

func (r *unsModelerMutationsResolver) ImportProposal(ctx context.Context, _ *generated.UnsModelerMutations, name string, document string, topicMode *string, archiveGroup *string, topicFilters []string, topicData []*generated.TopicDataInput) (*generated.UnsModelerMappingResult, error) {
	res := &generated.UnsModelerMappingResult{Errors: []string{}, Issues: []*generated.UnsModelerIssue{}}
	if r.UnsMgr == nil {
		res.Errors = append(res.Errors, errUnsNotEmbedded.Error())
		return res, nil
	}
	in := unsmodeler.TopicInput{Mode: strings.ToLower(derefStr(topicMode)), ArchiveGroup: derefStr(archiveGroup), Filters: topicFilters, Topics: unsTopicData(topicData)}
	if in.Mode == "" {
		in.Mode = "live"
	}
	doc, issues, err := r.UnsMgr.ImportProposal(ctx, name, []byte(document), in)
	res.Issues = unsIssues(issues)
	if doc != nil {
		res.Stored = true
		res.Document = optString(string(doc))
	}
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	res.Success = true
	return res, nil
}

// unsTopicData converts the topics and values the dashboard collected live.
func unsTopicData(in []*generated.TopicDataInput) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for _, t := range in {
		if t != nil && t.Topic != "" {
			out[t.Topic] = derefStr(t.Value)
		}
	}
	return out
}
