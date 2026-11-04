package genai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// anthropicTempTransport drops the "temperature" field from Anthropic
// Messages requests. Newer Claude models reject it ("`temperature` is
// deprecated for this model"), and the langchaingo client always sends the
// field, even as 0. The request goes out with the temperature first; if the
// API refuses it, the request is retried without and every later request
// omits it.
type anthropicTempTransport struct {
	base     http.RoundTripper
	dropTemp atomic.Bool
	keepTemp bool // false: never send temperature (none configured)
}

func newAnthropicTempTransport(sendTemperature bool) *anthropicTempTransport {
	return &anthropicTempTransport{base: http.DefaultTransport, keepTemp: sendTemperature}
}

func (t *anthropicTempTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil || req.Method != http.MethodPost {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}

	send := func(b []byte) (*http.Response, error) {
		r := req.Clone(req.Context())
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		return t.base.RoundTrip(r)
	}

	if !t.keepTemp || t.dropTemp.Load() {
		return send(stripTemperature(body))
	}
	resp, err := send(body)
	if err != nil || resp.StatusCode != http.StatusBadRequest || resp.Body == nil {
		return resp, err
	}
	errBody, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if rerr == nil && isTemperatureRejected(errBody) {
		t.dropTemp.Store(true)
		return send(stripTemperature(body))
	}
	resp.Body = io.NopCloser(bytes.NewReader(errBody))
	return resp, nil
}

func isTemperatureRejected(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "temperature") &&
		(strings.Contains(s, "deprecated") || strings.Contains(s, "not supported") || strings.Contains(s, "unsupported"))
}

func stripTemperature(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["temperature"]; !ok {
		return body
	}
	delete(m, "temperature")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
