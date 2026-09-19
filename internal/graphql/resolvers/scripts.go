package resolvers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/scripting"
	"monstermq.io/edge/internal/stores"
)

// Query: scripts(name, nodeId) -----------------------------------------------

func (r *queryResolver) Scripts(ctx context.Context, name, nodeId *string) ([]*generated.Script, error) {
	if !r.Cfg.Features.PythonScripts {
		return []*generated.Script{}, nil
	}
	devices, err := r.Storage.DeviceConfig.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	out := []*generated.Script{}
	for _, d := range devices {
		if d.Type != scripting.DeviceTypeScript {
			continue
		}
		if name != nil && *name != "" && d.Name != *name {
			continue
		}
		if nodeId != nil && *nodeId != "" && d.NodeID != *nodeId {
			continue
		}
		out = append(out, r.deviceToScript(d))
	}
	return out, nil
}

// Query: script(name) --------------------------------------------------------

func (r *queryResolver) Script(ctx context.Context, name string) (*generated.Script, error) {
	if !r.Cfg.Features.PythonScripts {
		return nil, nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || d == nil || d.Type != scripting.DeviceTypeScript {
		return nil, nil
	}
	return r.deviceToScript(*d), nil
}

// Query: scriptLanguages -----------------------------------------------------

func (r *queryResolver) ScriptLanguages(ctx context.Context) ([]*generated.ScriptLanguage, error) {
	if !r.Cfg.Features.PythonScripts {
		return []*generated.ScriptLanguage{}, nil
	}
	isDefault := true
	desc := "Python-compatible dialect supported across both Edge and Main brokers."
	return []*generated.ScriptLanguage{
		{
			Name:        "starlark",
			DisplayName: "Starlark (Go / Python dialect)",
			Description: &desc,
			IsDefault:   &isDefault,
		},
	}, nil
}

// Mutation: script -----------------------------------------------------------

type scriptMutationsResolver struct{ *Resolver }

func (r *mutationResolver) Script(ctx context.Context) (*generated.ScriptMutations, error) {
	return &generated.ScriptMutations{}, nil
}

func (r *scriptMutationsResolver) Create(ctx context.Context, _ *generated.ScriptMutations, input generated.ScriptInput) (*generated.ScriptResult, error) {
	if !r.Cfg.Features.PythonScripts {
		return &generated.ScriptResult{Success: false, Errors: []string{"PythonScripts feature is disabled"}}, nil
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return &generated.ScriptResult{Success: false, Errors: []string{"name cannot be empty"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err == nil && existing != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{fmt.Sprintf("Script %q already exists", name)}}, nil
	}

	cfg := scriptConfigInputToConfig(input.Config)
	// Validate script compilation
	if _, err := scripting.NewEngine(name, cfg.Script); err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{fmt.Sprintf("Script compilation error: %v", err)}}, nil
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	nodeID := input.NodeID
	if nodeID == "" {
		nodeID = r.NodeID
	}
	namespace := input.Namespace
	if namespace == "" {
		namespace = "script"
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	d := stores.DeviceConfig{
		Name:      name,
		Namespace: namespace,
		NodeID:    nodeID,
		Type:      scripting.DeviceTypeScript,
		Enabled:   enabled,
		Config:    string(cfgJSON),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := r.Storage.DeviceConfig.Save(ctx, d); err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	r.reloadScripts(ctx)
	saved, _ := r.Storage.DeviceConfig.Get(ctx, d.Name)
	if saved == nil {
		saved = &d
	}

	return &generated.ScriptResult{
		Success: true,
		Script:  r.deviceToScript(*saved),
		Errors:  []string{},
	}, nil
}

func (r *scriptMutationsResolver) Update(ctx context.Context, _ *generated.ScriptMutations, name string, input generated.ScriptInput) (*generated.ScriptResult, error) {
	if !r.Cfg.Features.PythonScripts {
		return &generated.ScriptResult{Success: false, Errors: []string{"PythonScripts feature is disabled"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil || existing.Type != scripting.DeviceTypeScript {
		return &generated.ScriptResult{Success: false, Errors: []string{fmt.Sprintf("Script %q not found", name)}}, nil
	}

	cfg := scriptConfigInputToConfig(input.Config)
	if _, err := scripting.NewEngine(name, cfg.Script); err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{fmt.Sprintf("Script compilation error: %v", err)}}, nil
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	nodeID := existing.NodeID
	if input.NodeID != "" {
		nodeID = input.NodeID
	}
	namespace := existing.Namespace
	if input.Namespace != "" {
		namespace = input.Namespace
	}

	enabled := existing.Enabled
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	updated := stores.DeviceConfig{
		Name:      existing.Name,
		Namespace: namespace,
		NodeID:    nodeID,
		Type:      scripting.DeviceTypeScript,
		Enabled:   enabled,
		Config:    string(cfgJSON),
		CreatedAt: existing.CreatedAt,
		UpdatedAt: time.Now().UTC(),
	}

	if err := r.Storage.DeviceConfig.Save(ctx, updated); err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	r.reloadScripts(ctx)
	saved, _ := r.Storage.DeviceConfig.Get(ctx, name)
	if saved == nil {
		saved = &updated
	}

	return &generated.ScriptResult{
		Success: true,
		Script:  r.deviceToScript(*saved),
		Errors:  []string{},
	}, nil
}

func (r *scriptMutationsResolver) Delete(ctx context.Context, _ *generated.ScriptMutations, name string) (bool, error) {
	if !r.Cfg.Features.PythonScripts {
		return false, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil || existing.Type != scripting.DeviceTypeScript {
		return false, nil
	}
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, err
	}
	r.reloadScripts(ctx)
	return true, nil
}

func (r *scriptMutationsResolver) Toggle(ctx context.Context, _ *generated.ScriptMutations, name string, enabled bool) (*generated.ScriptResult, error) {
	if !r.Cfg.Features.PythonScripts {
		return &generated.ScriptResult{Success: false, Errors: []string{"PythonScripts feature is disabled"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil || existing.Type != scripting.DeviceTypeScript {
		return &generated.ScriptResult{Success: false, Errors: []string{fmt.Sprintf("Script %q not found", name)}}, nil
	}

	existing.Enabled = enabled
	existing.UpdatedAt = time.Now().UTC()
	if err := r.Storage.DeviceConfig.Save(ctx, *existing); err != nil {
		return &generated.ScriptResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	r.reloadScripts(ctx)
	saved, _ := r.Storage.DeviceConfig.Get(ctx, name)
	if saved == nil {
		saved = existing
	}

	return &generated.ScriptResult{
		Success: true,
		Script:  r.deviceToScript(*saved),
		Errors:  []string{},
	}, nil
}

func (r *scriptMutationsResolver) Start(ctx context.Context, _ *generated.ScriptMutations, name string) (*generated.ScriptResult, error) {
	return r.Toggle(ctx, nil, name, true)
}

func (r *scriptMutationsResolver) Stop(ctx context.Context, _ *generated.ScriptMutations, name string) (*generated.ScriptResult, error) {
	return r.Toggle(ctx, nil, name, false)
}

func (r *scriptMutationsResolver) Test(ctx context.Context, _ *generated.ScriptMutations, input generated.ScriptInput, testTopic, testPayload, testArgs *string) (*generated.ScriptTestResult, error) {
	if !r.Cfg.Features.PythonScripts || r.Scripts == nil {
		return &generated.ScriptTestResult{
			Success: false,
			Errors:  []string{"PythonScripts feature is disabled or engine not initialized"},
		}, nil
	}

	cfg := scriptConfigInputToConfig(input.Config)
	var topicStr, payloadStr string
	if testTopic != nil {
		topicStr = *testTopic
	}
	if testPayload != nil {
		payloadStr = *testPayload
	}

	var parsedArgs map[string]any
	if testArgs != nil && strings.TrimSpace(*testArgs) != "" {
		if err := json.Unmarshal([]byte(*testArgs), &parsedArgs); err != nil {
			return &generated.ScriptTestResult{
				Success: false,
				Errors:  []string{fmt.Sprintf("testArgs invalid JSON: %v", err)},
			}, nil
		}
	}

	res, err := r.Scripts.TestScript(input.Name, cfg, topicStr, payloadStr, parsedArgs)
	if err != nil {
		return &generated.ScriptTestResult{
			Success: false,
			Errors:  []string{err.Error()},
		}, nil
	}

	var outputMsgs []*generated.ScriptPublishedMessage
	for _, pm := range res.PublishedMessages {
		outputMsgs = append(outputMsgs, &generated.ScriptPublishedMessage{
			Topic:   pm.Topic,
			Payload: pm.Payload,
			Qos:     pm.QoS,
			Retain:  pm.Retain,
		})
	}
	if outputMsgs == nil {
		outputMsgs = []*generated.ScriptPublishedMessage{}
	}

	var errs []string
	if res.Error != nil {
		errs = append(errs, res.Error.Error())
	}
	if errs == nil {
		errs = []string{}
	}

	logs := res.Logs
	if logs == nil {
		logs = []string{}
	}

	var returnValStr *string
	if res.ReturnValue != nil {
		s := fmt.Sprintf("%v", res.ReturnValue)
		returnValStr = &s
	}

	return &generated.ScriptTestResult{
		Success:         res.Success,
		ReturnValue:     returnValStr,
		OutputMessages:  outputMsgs,
		Logs:            logs,
		Errors:          errs,
		ExecutionTimeMs: res.ExecutionTimeMs,
	}, nil
}

// Helpers --------------------------------------------------------------------

func (r *Resolver) deviceToScript(d stores.DeviceConfig) *generated.Script {
	cfg, _ := scripting.ParseConfig(d.Config)
	if cfg == nil {
		cfg = &scripting.ScriptConfig{
			Language:     scripting.DefaultLanguage,
			TriggerType:  scripting.DefaultTriggerType,
			InstanceMode: scripting.DefaultInstanceMode,
		}
	}

	var (
		execCount  int64
		errCount   int64
		lastTime   string
		lastStatus string
		recentLogs = []string{}
	)
	if r.Scripts != nil {
		if conn := r.Scripts.GetConnector(d.Name); conn != nil {
			execCount, errCount, lastTime, lastStatus, recentLogs = conn.Stats()
		}
	}

	var lastTimePtr *string
	if lastTime != "" {
		lastTimePtr = &lastTime
	}
	var lastStatusPtr *string
	if lastStatus != "" {
		lastStatusPtr = &lastStatus
	}

	return &generated.Script{
		Name:                d.Name,
		Namespace:           d.Namespace,
		NodeID:              d.NodeID,
		Enabled:             d.Enabled,
		Config:              scriptConfigToGenerated(*cfg),
		CreatedAt:           d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:           d.UpdatedAt.UTC().Format(time.RFC3339),
		IsOnCurrentNode:     d.NodeID == r.NodeID,
		ExecutionCount:      &execCount,
		ErrorCount:          &errCount,
		LastExecutionTime:   lastTimePtr,
		LastExecutionStatus: lastStatusPtr,
		RecentLogs:          recentLogs,
	}
}

func scriptConfigInputToConfig(input *generated.ScriptConfigInput) scripting.ScriptConfig {
	cfg := scripting.ScriptConfig{
		Language:     scripting.DefaultLanguage,
		TriggerType:  scripting.DefaultTriggerType,
		TopicFilters: []string{},
		InstanceMode: scripting.DefaultInstanceMode,
		TimeoutMs:    scripting.DefaultTimeoutMs,
	}
	if input == nil {
		return cfg
	}
	if input.Language != "" {
		cfg.Language = input.Language
	}
	cfg.Script = input.Script
	if input.TriggerType != "" {
		cfg.TriggerType = scripting.ScriptTriggerType(input.TriggerType)
	}
	if len(input.TopicFilters) > 0 {
		cfg.TopicFilters = input.TopicFilters
	}
	if input.TriggerOnChangeOnly != nil {
		cfg.TriggerOnChangeOnly = *input.TriggerOnChangeOnly
	}
	if input.TimerIntervalMs != nil {
		cfg.TimerIntervalMs = int64(*input.TimerIntervalMs)
	}
	if input.InstanceMode != "" {
		cfg.InstanceMode = scripting.ScriptInstanceMode(input.InstanceMode)
	}
	if input.TimeoutMs != nil && *input.TimeoutMs > 0 {
		cfg.TimeoutMs = int64(*input.TimeoutMs)
	}
	if input.Description != nil {
		cfg.Description = *input.Description
	}
	return cfg
}

func scriptConfigToGenerated(cfg scripting.ScriptConfig) *generated.ScriptConfig {
	desc := cfg.Description
	triggerOnChange := cfg.TriggerOnChangeOnly
	timerMs := int(cfg.TimerIntervalMs)
	timeoutMs := int(cfg.TimeoutMs)

	return &generated.ScriptConfig{
		Language:            cfg.Language,
		Script:              cfg.Script,
		TriggerType:         generated.ScriptTriggerType(cfg.TriggerType),
		TopicFilters:        cfg.TopicFilters,
		TriggerOnChangeOnly: &triggerOnChange,
		TimerIntervalMs:     &timerMs,
		InstanceMode:        generated.ScriptInstanceMode(cfg.InstanceMode),
		TimeoutMs:           &timeoutMs,
		Description:         &desc,
	}
}

func (r *Resolver) reloadScripts(ctx context.Context) {
	if r.Scripts != nil {
		if err := r.Scripts.Reload(ctx); err != nil {
			r.Logger.Warn("scripts reload failed", "err", err)
		}
	}
}
