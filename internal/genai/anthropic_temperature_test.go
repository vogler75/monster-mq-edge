package genai

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicTempTransportRetriesWithoutTemperature(t *testing.T) {
	var bodies []string
	tr := newAnthropicTempTransport(true)
	tr.base = rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "temperature") {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("`temperature` is deprecated for this model."))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	do := func() int {
		req, _ := http.NewRequest(http.MethodPost, "http://x/v1/messages", strings.NewReader(`{"model":"m","temperature":0.7}`))
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if do() != 200 || len(bodies) != 2 {
		t.Fatalf("expected retry, got bodies %v", bodies)
	}
	if do() != 200 || len(bodies) != 3 || strings.Contains(bodies[2], "temperature") {
		t.Fatalf("expected temperature dropped afterwards, got %v", bodies)
	}
}

func TestStripThinkingBlocks(t *testing.T) {
	in := `{"id":"x","content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"text","text":"hi"}]}`
	out := string(stripThinkingBlocks([]byte(in)))
	if strings.Contains(out, "thinking") || !strings.Contains(out, `"text":"hi"`) {
		t.Fatalf("unexpected: %s", out)
	}
}
