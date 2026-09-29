package winccoanative

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"monstermq.io/edge/internal/oahost"
)

// MaxCommandPayload bounds a command payload (spec section 7).
const MaxCommandPayload = 64 << 10

var ErrValue = errors.New("invalid command value")

// Command is a parsed write request payload.
type Command struct {
	ID      string
	ReplyTo string
	Value   json.RawMessage
}

// ParseCommand accepts {"value":..,"id":..,"replyTo":..} or a bare JSON
// value. Unknown object members are rejected so a typo cannot be silently
// written as a different value.
func ParseCommand(payload []byte) (Command, error) {
	var c Command
	if len(payload) > MaxCommandPayload {
		return c, fmt.Errorf("%w: payload exceeds %d bytes", ErrValue, MaxCommandPayload)
	}
	if !utf8.Valid(payload) {
		return c, fmt.Errorf("%w: payload is not UTF-8", ErrValue)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return c, fmt.Errorf("%w: empty payload", ErrValue)
	}
	if trimmed[0] == '{' {
		var obj map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			return c, fmt.Errorf("%w: %v", ErrValue, err)
		}
		for k := range obj {
			switch k {
			case "value", "id", "replyTo":
			default:
				return c, fmt.Errorf("%w: unknown field %q", ErrValue, k)
			}
		}
		raw, ok := obj["value"]
		if !ok {
			return c, fmt.Errorf("%w: missing value", ErrValue)
		}
		c.Value = raw
		if id, ok := obj["id"]; ok {
			if err := json.Unmarshal(id, &c.ID); err != nil {
				return c, fmt.Errorf("%w: id must be a string", ErrValue)
			}
		}
		if rt, ok := obj["replyTo"]; ok {
			if err := json.Unmarshal(rt, &c.ReplyTo); err != nil {
				return c, fmt.Errorf("%w: replyTo must be a string", ErrValue)
			}
		}
		return c, nil
	}
	if !json.Valid(trimmed) {
		return c, fmt.Errorf("%w: not JSON", ErrValue)
	}
	c.Value = trimmed
	return c, nil
}

// ConvertValue converts a JSON command value to the element's OA type. It
// never coerces across type families (a string is not parsed as a number).
func ConvertValue(raw json.RawMessage, kind oahost.Kind) (oahost.Value, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return oahost.Value{}, fmt.Errorf("%w: null is not writable", ErrValue)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return oahost.Value{}, fmt.Errorf("%w: %v", ErrValue, err)
	}
	switch kind {
	case oahost.KindBool:
		b, ok := v.(bool)
		if !ok {
			return oahost.Value{}, fmt.Errorf("%w: expected boolean", ErrValue)
		}
		return oahost.Value{Kind: kind, Bool: b}, nil
	case oahost.KindInt:
		n, ok := v.(json.Number)
		if !ok {
			return oahost.Value{}, fmt.Errorf("%w: expected integer", ErrValue)
		}
		i, err := strconv.ParseInt(n.String(), 10, 32)
		if err != nil {
			return oahost.Value{}, fmt.Errorf("%w: integer out of range or not integral: %s", ErrValue, n)
		}
		return oahost.Value{Kind: kind, Int: i}, nil
	case oahost.KindUint, oahost.KindBit32:
		n, ok := v.(json.Number)
		if !ok {
			return oahost.Value{}, fmt.Errorf("%w: expected unsigned integer", ErrValue)
		}
		u, err := strconv.ParseUint(n.String(), 10, 32)
		if err != nil {
			return oahost.Value{}, fmt.Errorf("%w: unsigned integer out of range or not integral: %s", ErrValue, n)
		}
		return oahost.Value{Kind: kind, Uint: u}, nil
	case oahost.KindFloat:
		n, ok := v.(json.Number)
		if !ok {
			return oahost.Value{}, fmt.Errorf("%w: expected number", ErrValue)
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return oahost.Value{}, fmt.Errorf("%w: number out of range: %s", ErrValue, n)
		}
		return oahost.Value{Kind: kind, Float: f}, nil
	case oahost.KindString:
		s, ok := v.(string)
		if !ok {
			return oahost.Value{}, fmt.Errorf("%w: expected string", ErrValue)
		}
		return oahost.Value{Kind: kind, Str: s}, nil
	case oahost.KindTime:
		switch x := v.(type) {
		case string:
			t, err := time.Parse(time.RFC3339Nano, x)
			if err != nil {
				return oahost.Value{}, fmt.Errorf("%w: time must be RFC 3339: %v", ErrValue, err)
			}
			return oahost.Value{Kind: kind, Time: t.UTC()}, nil
		case json.Number:
			ms, err := strconv.ParseInt(x.String(), 10, 64)
			if err != nil {
				return oahost.Value{}, fmt.Errorf("%w: time in ms must be an integer", ErrValue)
			}
			return oahost.Value{Kind: kind, Time: time.UnixMilli(ms).UTC()}, nil
		}
		return oahost.Value{}, fmt.Errorf("%w: expected RFC 3339 string or epoch ms", ErrValue)
	case oahost.KindBytes:
		s, ok := v.(string)
		if !ok {
			return oahost.Value{}, fmt.Errorf("%w: blob must be a base64 string", ErrValue)
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return oahost.Value{}, fmt.Errorf("%w: blob must be base64: %v", ErrValue, err)
		}
		return oahost.Value{Kind: kind, Bytes: b}, nil
	}
	return oahost.Value{}, fmt.Errorf("%w: element type %d is not writable through MQTT", ErrValue, kind)
}

// ValuePayload is the JSON published for a native value:
// {"time":"<RFC 3339 ms>","value":<json>}. It is built by hand because it
// runs for every hotlink.
func ValuePayload(v oahost.Value, ts time.Time) []byte {
	out := make([]byte, 0, 64)
	out = append(out, `{"time":"`...)
	out = ts.UTC().AppendFormat(out, "2006-01-02T15:04:05.000Z")
	out = append(out, `","value":`...)
	switch v.Kind {
	case oahost.KindBool:
		out = strconv.AppendBool(out, v.Bool)
	case oahost.KindInt:
		out = strconv.AppendInt(out, v.Int, 10)
	case oahost.KindUint, oahost.KindBit32:
		out = strconv.AppendUint(out, v.Uint, 10)
	case oahost.KindFloat:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			out = append(out, "null"...)
		} else {
			out = appendJSONFloat(out, v.Float)
		}
	default:
		b, _ := json.Marshal(v.JSON())
		out = append(out, b...)
	}
	return append(out, '}')
}

// appendJSONFloat formats like encoding/json: plain notation unless the
// magnitude is below 1e-6 or at least 1e21, with a trimmed exponent.
func appendJSONFloat(b []byte, f float64) []byte {
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if format == 'e' {
		// clean up e-09 to e-9
		n := len(b)
		if n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}
