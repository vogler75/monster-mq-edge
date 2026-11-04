package genai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// Tool is a function the model may call. Parameters is the JSON Schema of
// the arguments: an object with flat scalar properties only (string,
// integer, number, boolean), because the Gemini conversion of langchaingo
// keeps only type and description of each top-level property.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// ToolRequest is one tool-calling run.
type ToolRequest struct {
	System string
	Prompt string
	// MaxSteps bounds the tool calls; TokenBudget bounds input plus output
	// tokens over all model calls (0: unbounded).
	MaxSteps    int
	TokenBudget int
	// OnEvent receives every model answer, tool call and tool result.
	OnEvent func(ToolEvent)
	// Final checks an answer without tool calls. With done false the
	// feedback is sent back as the next user message and the run goes on
	// (it counts as a step against MaxSteps).
	Final func(text string) (feedback string, done bool)
}

// ToolEvent is one entry of the run log.
type ToolEvent struct {
	Step   int
	Kind   string // "text", "tool_call", "tool_result", "usage", "feedback"
	Name   string
	Args   string
	Result string
	Error  string
	Tokens int
}

// ToolResponse is the final answer of a tool-calling run.
type ToolResponse struct {
	Text         string
	Model        string
	Steps        int
	ToolCalls    int
	InputTokens  int
	OutputTokens int
}

var (
	// ErrNoToolSupport reports a provider or model without tool calling.
	ErrNoToolSupport = errors.New("the provider or model does not support tool calling")
	ErrMaxSteps      = errors.New("the AI run reached its maximum number of tool calls")
	ErrTokenBudget   = errors.New("the AI run exceeded its token budget")
)

// ToolCaller is implemented by providers that support tool calling.
type ToolCaller interface {
	GenerateWithTools(ctx context.Context, req ToolRequest, tools []Tool) (*ToolResponse, error)
}

// CheckToolDeclaration reports a tool declaration that a backend would not
// accept: parameters must be an object of flat scalar properties.
func CheckToolDeclaration(t Tool) error {
	if t.Name == "" {
		return errors.New("tool without name")
	}
	if t.Parameters == nil {
		return nil
	}
	if ty, _ := t.Parameters["type"].(string); ty != "object" {
		return fmt.Errorf("tool %s: parameters must be an object", t.Name)
	}
	props, _ := t.Parameters["properties"].(map[string]any)
	for name, p := range props {
		pm, ok := p.(map[string]any)
		if !ok {
			return fmt.Errorf("tool %s: property %s is not a schema", t.Name, name)
		}
		switch pm["type"] {
		case "string", "integer", "number", "boolean":
		default:
			return fmt.Errorf("tool %s: property %s has type %v; only flat scalar parameters are allowed", t.Name, name, pm["type"])
		}
		for k := range pm {
			if k != "type" && k != "description" {
				return fmt.Errorf("tool %s: property %s uses %q, which the Gemini conversion drops", t.Name, name, k)
			}
		}
	}
	return nil
}

func toLLMTools(tools []Tool) []llms.Tool {
	out := make([]llms.Tool, len(tools))
	for i, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out[i] = llms.Tool{Type: "function", Function: &llms.FunctionDefinition{Name: t.Name, Description: t.Description, Parameters: params}}
	}
	return out
}

// usageOf reads the token usage of a model answer; the key names differ
// per backend.
func usageOf(resp *llms.ContentResponse) (in, out int) {
	for _, c := range resp.Choices {
		info := c.GenerationInfo
		pick := func(keys ...string) int {
			for _, k := range keys {
				switch v := info[k].(type) {
				case int:
					return v
				case int32:
					return int(v)
				case int64:
					return int(v)
				case float64:
					return int(v)
				}
			}
			return 0
		}
		i := pick("PromptTokens", "InputTokens", "input_tokens", "prompt_eval_count")
		o := pick("CompletionTokens", "OutputTokens", "output_tokens", "eval_count")
		// Every choice of one answer carries the same usage.
		if i > in {
			in = i
		}
		if o > out {
			out = o
		}
	}
	return in, out
}

// runToolLoop runs the generate–call–respond loop on model. Each tool call
// is sent back as one assistant message with the call followed by one tool
// message with its result, which every langchaingo backend in use accepts.
func runToolLoop(ctx context.Context, model llms.Model, modelName string, opts []llms.CallOption, req ToolRequest, tools []Tool) (*ToolResponse, error) {
	for _, t := range tools {
		if err := CheckToolDeclaration(t); err != nil {
			return nil, err
		}
	}
	byName := map[string]Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	emit := func(e ToolEvent) {
		if req.OnEvent != nil {
			req.OnEvent(e)
		}
	}
	var msgs []llms.MessageContent
	if req.System != "" {
		msgs = append(msgs, llms.TextParts(llms.ChatMessageTypeSystem, req.System))
	}
	msgs = append(msgs, llms.TextParts(llms.ChatMessageTypeHuman, req.Prompt))
	callOpts := append([]llms.CallOption{llms.WithTools(toLLMTools(tools))}, opts...)
	res := &ToolResponse{Model: modelName}
	for step := 1; ; step++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Steps = step
		resp, err := model.GenerateContent(ctx, msgs, callOpts...)
		if err != nil {
			if step == 1 && looksLikeNoToolSupport(err) {
				return res, fmt.Errorf("%w: %v", ErrNoToolSupport, err)
			}
			return res, fmt.Errorf("AI request failed: %w", err)
		}
		in, out := usageOf(resp)
		res.InputTokens += in
		res.OutputTokens += out
		emit(ToolEvent{Step: step, Kind: "usage", Tokens: in + out})
		var calls []llms.ToolCall
		var text strings.Builder
		for _, c := range resp.Choices {
			calls = append(calls, c.ToolCalls...)
			text.WriteString(c.Content)
		}
		if t := strings.TrimSpace(text.String()); t != "" {
			emit(ToolEvent{Step: step, Kind: "text", Result: t})
		}
		if len(calls) == 0 {
			res.Text = text.String()
			if req.Final == nil {
				return res, nil
			}
			feedback, done := req.Final(res.Text)
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if done {
				return res, nil
			}
			if req.MaxSteps > 0 && res.ToolCalls >= req.MaxSteps {
				return res, fmt.Errorf("%w (%d)", ErrMaxSteps, req.MaxSteps)
			}
			res.ToolCalls++
			if req.TokenBudget > 0 && res.InputTokens+res.OutputTokens > req.TokenBudget {
				return res, fmt.Errorf("%w (%d of %d tokens)", ErrTokenBudget, res.InputTokens+res.OutputTokens, req.TokenBudget)
			}
			emit(ToolEvent{Step: step, Kind: "feedback", Result: feedback})
			answer := res.Text
			if strings.TrimSpace(answer) == "" {
				answer = "(empty answer)"
			}
			msgs = append(msgs, llms.TextParts(llms.ChatMessageTypeAI, answer), llms.TextParts(llms.ChatMessageTypeHuman, feedback))
			continue
		}
		if req.TokenBudget > 0 && res.InputTokens+res.OutputTokens > req.TokenBudget {
			return res, fmt.Errorf("%w (%d of %d tokens)", ErrTokenBudget, res.InputTokens+res.OutputTokens, req.TokenBudget)
		}
		for _, call := range calls {
			if req.MaxSteps > 0 && res.ToolCalls >= req.MaxSteps {
				return res, fmt.Errorf("%w (%d)", ErrMaxSteps, req.MaxSteps)
			}
			res.ToolCalls++
			if call.FunctionCall == nil {
				continue
			}
			if call.ID == "" {
				call.ID = fmt.Sprintf("call_%d_%d", step, res.ToolCalls)
			}
			call.Type = "function"
			name, args := call.FunctionCall.Name, call.FunctionCall.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
				call.FunctionCall.Arguments = args
			}
			emit(ToolEvent{Step: step, Kind: "tool_call", Name: name, Args: args})
			var result string
			t, ok := byName[name]
			if !ok {
				result = fmt.Sprintf("error: unknown tool %q", name)
			} else {
				out, err := t.Handler(ctx, json.RawMessage(args))
				if err != nil {
					result = "error: " + err.Error()
					emit(ToolEvent{Step: step, Kind: "tool_result", Name: name, Result: result, Error: err.Error()})
				} else {
					result = out
					emit(ToolEvent{Step: step, Kind: "tool_result", Name: name, Result: out})
				}
			}
			msgs = append(msgs,
				llms.MessageContent{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{call}},
				llms.MessageContent{Role: llms.ChatMessageTypeTool, Parts: []llms.ContentPart{llms.ToolCallResponse{ToolCallID: call.ID, Name: name, Content: result}}},
			)
			if err := ctx.Err(); err != nil {
				return res, err
			}
		}
	}
}

func looksLikeNoToolSupport(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "does not support tools") || strings.Contains(s, "tool use is not supported") ||
		strings.Contains(s, "tools is not supported") || strings.Contains(s, "function calling is not enabled") ||
		strings.Contains(s, "does not support function")
}

// GenerateWithTools implements ToolCaller for the langchaingo adapter.
func (a *LangChainAdapter) GenerateWithTools(ctx context.Context, req ToolRequest, tools []Tool) (*ToolResponse, error) {
	model := a.model
	if a.toolModel != nil {
		model = a.toolModel
	}
	if model == nil {
		return nil, errors.New("provider model is not initialized")
	}
	var opts []llms.CallOption
	if a.temperature > 0 {
		opts = append(opts, llms.WithTemperature(a.temperature))
	}
	if a.maxTokens > 0 {
		opts = append(opts, llms.WithMaxTokens(a.maxTokens))
	}
	return runToolLoop(ctx, model, a.modelName, opts, req, tools)
}

// WithTimeout bounds a run; a helper for callers that hold a seconds value.
func WithTimeout(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}
