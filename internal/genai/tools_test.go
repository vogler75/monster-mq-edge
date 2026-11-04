package genai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/googleai"
)

// fakeModel answers with scripted responses and records the messages.
type fakeModel struct {
	mu      sync.Mutex
	answers []*llms.ContentResponse
	calls   [][]llms.MessageContent
	tools   [][]llms.Tool
}

func (f *fakeModel) GenerateContent(_ context.Context, msgs []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o llms.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	f.calls = append(f.calls, append([]llms.MessageContent(nil), msgs...))
	f.tools = append(f.tools, o.Tools)
	if len(f.answers) == 0 {
		return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "done"}}}, nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}

func (f *fakeModel) Call(context.Context, string, ...llms.CallOption) (string, error) { return "", nil }

func toolCallAnswer(calls ...[2]string) *llms.ContentResponse {
	var tcs []llms.ToolCall
	for i, c := range calls {
		tcs = append(tcs, llms.ToolCall{ID: "c" + string(rune('0'+i)), Type: "function", FunctionCall: &llms.FunctionCall{Name: c[0], Arguments: c[1]}})
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{ToolCalls: tcs, GenerationInfo: map[string]any{"PromptTokens": 100, "CompletionTokens": 10}}}}
}

func testTools(log *[]string) []Tool {
	return []Tool{
		{Name: "listTypes", Description: "List types", Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"system":  map[string]any{"type": "string", "description": "system"},
			"pattern": map[string]any{"type": "string", "description": "pattern"},
		}}, Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			*log = append(*log, "listTypes "+string(args))
			return `["PumpType"]`, nil
		}},
		{Name: "validate", Description: "Validate", Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"proposalJson": map[string]any{"type": "string", "description": "document"},
		}}, Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			*log = append(*log, "validate")
			return "", errors.New("2 issues")
		}},
	}
}

func TestToolLoopFake(t *testing.T) {
	m := &fakeModel{answers: []*llms.ContentResponse{
		toolCallAnswer([2]string{"listTypes", `{"pattern":"P*"}`}, [2]string{"validate", `{"proposalJson":"{}"}`}),
		toolCallAnswer([2]string{"nope", ``}),
	}}
	var log []string
	var events []ToolEvent
	res, err := runToolLoop(context.Background(), m, "fake", nil, ToolRequest{System: "sys", Prompt: "go", MaxSteps: 10, OnEvent: func(e ToolEvent) { events = append(events, e) }}, testTools(&log))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.ToolCalls != 3 || res.Steps != 3 || res.InputTokens != 200 {
		t.Fatalf("response %+v", res)
	}
	if strings.Join(log, "|") != `listTypes {"pattern":"P*"}|validate` {
		t.Fatalf("handlers %v", log)
	}
	// Each tool call is answered by an assistant message with the call and a
	// tool message with its result, after the system and user messages.
	last := m.calls[2]
	if last[0].Role != llms.ChatMessageTypeSystem || last[1].Role != llms.ChatMessageTypeHuman || len(last) != 8 {
		t.Fatalf("messages %d", len(last))
	}
	for i := 2; i < len(last); i += 2 {
		if last[i].Role != llms.ChatMessageTypeAI || last[i+1].Role != llms.ChatMessageTypeTool {
			t.Fatalf("message %d roles %s %s", i, last[i].Role, last[i+1].Role)
		}
	}
	if r := last[5].Parts[0].(llms.ToolCallResponse); r.Content != "error: 2 issues" {
		t.Fatalf("tool error result %q", r.Content)
	}
	if r := last[7].Parts[0].(llms.ToolCallResponse); !strings.Contains(r.Content, "unknown tool") {
		t.Fatalf("unknown tool %q", r.Content)
	}
	if len(m.tools[0]) != 2 {
		t.Fatal("tools not passed")
	}
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds["tool_call"] != 3 || kinds["tool_result"] != 2 || kinds["text"] != 1 {
		t.Fatalf("events %v", kinds)
	}
}

func TestToolLoopLimits(t *testing.T) {
	loop := func() *fakeModel {
		var a []*llms.ContentResponse
		for i := 0; i < 20; i++ {
			a = append(a, toolCallAnswer([2]string{"listTypes", `{}`}))
		}
		return &fakeModel{answers: a}
	}
	var log []string
	if _, err := runToolLoop(context.Background(), loop(), "f", nil, ToolRequest{Prompt: "x", MaxSteps: 3}, testTools(&log)); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("max steps: %v", err)
	}
	if _, err := runToolLoop(context.Background(), loop(), "f", nil, ToolRequest{Prompt: "x", MaxSteps: 100, TokenBudget: 250}, testTools(&log)); !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("budget: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runToolLoop(ctx, loop(), "f", nil, ToolRequest{Prompt: "x", MaxSteps: 100}, testTools(&log)); err == nil {
		t.Fatal("cancelled run succeeded")
	}
}

func TestToolDeclarationsFlat(t *testing.T) {
	var log []string
	for _, tl := range testTools(&log) {
		if err := CheckToolDeclaration(tl); err != nil {
			t.Fatal(err)
		}
	}
	bad := Tool{Name: "x", Parameters: map[string]any{"type": "object", "properties": map[string]any{"list": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}}
	if CheckToolDeclaration(bad) == nil {
		t.Fatal("array parameter accepted")
	}
}

func TestToolLoopCancellationFromFinal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &fakeModel{}
	_, err := runToolLoop(ctx, m, "fake", nil, ToolRequest{
		Final: func(string) (string, bool) {
			cancel()
			return "invalid document", false
		},
	}, nil)
	if !errors.Is(err, context.Canceled) || len(m.calls) != 1 {
		t.Fatalf("cancellation: %v, %d model calls", err, len(m.calls))
	}
}

// stubServer answers the first request with a tool call and the second
// with text, recording the request bodies.
type stubServer struct {
	mu     sync.Mutex
	bodies []string
	first  string
	final  string
}

func (s *stubServer) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(b))
	n := len(s.bodies)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if n == 1 {
		_, _ = io.WriteString(w, s.first)
	} else {
		_, _ = io.WriteString(w, s.final)
	}
}

func runBackend(t *testing.T, p Provider, stub *stubServer, wantInBody ...string) {
	t.Helper()
	tc, ok := p.(ToolCaller)
	if !ok {
		t.Fatal("provider is no ToolCaller")
	}
	var log []string
	res, err := tc.GenerateWithTools(context.Background(), ToolRequest{System: "You browse.", Prompt: "List types", MaxSteps: 5}, testTools(&log))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "final") || res.ToolCalls != 1 || len(log) != 1 {
		t.Fatalf("result %+v log %v", res, log)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.bodies) != 2 {
		t.Fatalf("requests %d", len(stub.bodies))
	}
	for _, w := range wantInBody {
		if !strings.Contains(stub.bodies[0], w) {
			t.Fatalf("first request lacks %q: %s", w, stub.bodies[0])
		}
	}
	if !strings.Contains(stub.bodies[1], "PumpType") {
		t.Fatalf("tool result not sent back: %s", stub.bodies[1])
	}
}

const openAIToolAnswer = `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"listTypes","arguments":"{\"pattern\":\"P*\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
const openAIFinal = `{"id":"y","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`

func TestToolBackendOpenAICompatible(t *testing.T) {
	for _, prov := range []string{"openai", "llamacpp", "openrouter", "ollama"} {
		t.Run(prov, func(t *testing.T) {
			stub := &stubServer{first: openAIToolAnswer, final: openAIFinal}
			srv := httptest.NewServer(http.HandlerFunc(stub.handler))
			defer srv.Close()
			params := ProviderParams{Provider: prov, Model: "m", ApiKey: "k", Endpoint: srv.URL}
			if prov == "ollama" {
				params = ProviderParams{Provider: prov, Model: "m", BaseUrl: srv.URL}
			}
			p, err := CreateProvider(context.Background(), params)
			if err != nil {
				t.Fatal(err)
			}
			runBackend(t, p, stub, `"tools"`, `"listTypes"`, `"pattern"`)
		})
	}
}

func TestToolBackendClaude(t *testing.T) {
	stub := &stubServer{
		first: `{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"t1","name":"listTypes","input":{"pattern":"P*"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`,
		final: `{"id":"m2","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"final answer"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":5}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p, err := CreateProvider(context.Background(), ProviderParams{Provider: "claude", Model: "c", ApiKey: "k", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	runBackend(t, p, stub, `"tools"`, `"input_schema"`, `"listTypes"`)
}

// rewriteTransport sends every request to the stub server.
type rewriteTransport struct{ target string }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	t, _ := http.NewRequest(req.Method, r.target+u.Path+"?"+u.RawQuery, req.Body)
	t.Header = req.Header
	return http.DefaultTransport.RoundTrip(t)
}

func TestToolBackendGemini(t *testing.T) {
	stub := &stubServer{
		first: `[{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"listTypes","args":{"pattern":"P*"}}}]},"finishReason":1,"index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}]`,
		final: `[{"candidates":[{"content":{"role":"model","parts":[{"text":"final answer"}]},"finishReason":1,"index":0}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":5,"totalTokenCount":25}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	model, err := googleai.New(context.Background(), googleai.WithAPIKey("k"), googleai.WithRest(), googleai.WithDefaultModel("gemini-test"),
		googleai.WithHTTPClient(&http.Client{Transport: rewriteTransport{target: srv.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	p := NewLangChainAdapter(model, "gemini", "gemini-test", "", 0, 0)
	// The REST stream reader of the Google client misreads the end of the
	// stream with this Go version; production uses gRPC. Only the request
	// conversion of the tool declarations is checked here.
	var log []string
	_, _ = p.GenerateWithTools(context.Background(), ToolRequest{System: "You browse.", Prompt: "List types", MaxSteps: 5}, testTools(&log))
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.bodies) == 0 {
		t.Fatal("no request")
	}
	var req struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]struct {
						Type int `json:"type"`
					} `json:"properties"`
				} `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(stub.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	var decls []struct {
		Name       string `json:"name"`
		Parameters struct {
			Properties map[string]struct {
				Type int `json:"type"`
			} `json:"properties"`
		} `json:"parameters"`
	}
	for _, tl := range req.Tools {
		decls = append(decls, tl.FunctionDeclarations...)
	}
	if len(decls) != 2 || decls[0].Name != "listTypes" || decls[0].Parameters.Properties["pattern"].Type != 1 {
		t.Fatalf("declarations %s", stub.bodies[0])
	}
}

// TestToolLiveGemini runs a real tool loop against Gemini when
// GEMINI_API_KEY is set (AC-20 live run).
func TestToolLiveGemini(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set")
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}
	p, err := CreateProvider(context.Background(), ProviderParams{Provider: "gemini", Model: model, ApiKey: key})
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	ctx, cancel := WithTimeout(context.Background(), 120)
	defer cancel()
	res, err := p.(ToolCaller).GenerateWithTools(ctx, ToolRequest{
		System:   "You answer questions about a WinCC OA project. Use the tools; never guess.",
		Prompt:   "Which datapoint types match the pattern P*? Call listTypes with pattern P* and answer with the type names only.",
		MaxSteps: 5,
	}, testTools(&log))
	if err != nil {
		t.Fatal(err)
	}
	if len(log) == 0 || !strings.Contains(res.Text, "PumpType") {
		t.Fatalf("tools %v answer %q", log, res.Text)
	}
	t.Logf("answer %q after %d tool calls, %d+%d tokens", res.Text, res.ToolCalls, res.InputTokens, res.OutputTokens)
}

func TestSignatureTransport(t *testing.T) {
	tr := newSignatureTransport()
	tr.remember([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","extra_content":{"google":{"thought_signature":"S"}}}]}}]}`))
	out := tr.inject([]byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function"}]},{"role":"tool","tool_call_id":"c1"}]}`))
	if !strings.Contains(string(out), `"extra_content":{"google":{"thought_signature":"S"}}`) {
		t.Fatalf("not injected: %s", out)
	}
}
