// Package oastore keeps broker configuration and session metadata in
// WinCC OA datapoints (MMQConfigs / MMQSessions), spec-winccoa-native.md
// section 5. The broker is the only writer of these datapoints; the stores
// load them once at startup and then write through with OA confirmation.
package oastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/oahost"
)

const (
	ConfigType  = "MMQConfigs"
	SessionType = "MMQSessions"

	envelopeVersion = 1
	// MaxEnvelope bounds one stored record (spec section 7).
	MaxEnvelope = 256 << 10
	// enumBatch bounds names per dpGet during enumeration.
	enumBatch = 1000
)

var (
	ErrCollision = errors.New("oastore: datapoint name collision")
	ErrCorrupt   = errors.New("oastore: stored record is unreadable; refusing to overwrite")
	ErrVersion   = errors.New("oastore: unsupported record version")
)

type envelope struct {
	V    int             `json:"v"`
	Kind string          `json:"kind"`
	Key  string          `json:"key"`
	Rev  int64           `json:"rev"`
	Data json.RawMessage `json:"data"`
}

// DPName derives the collision-checked datapoint name for a record. The
// original key is kept in the envelope and verified on every read.
func DPName(prefix, kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return prefix + "_k" + hex.EncodeToString(sum[:])[:24]
}

type record struct {
	dp  string
	env envelope
}

// recordSet is the cache of one DPT (MMQConfigs or MMQSessions).
type recordSet struct {
	api     oahost.API
	dpt     string
	element string // element holding the envelope
	timeout time.Duration
	logger  *slog.Logger

	wmu     sync.Mutex // serializes writes (read-modify-write included)
	mu      sync.Mutex // guards the maps below
	loaded  bool
	records map[string]map[string]*record // kind -> key -> record
	broken  map[string]string             // dp -> reason
}

func newRecordSet(api oahost.API, dpt, element string, timeout time.Duration, logger *slog.Logger) *recordSet {
	return &recordSet{
		api: api, dpt: dpt, element: element, timeout: timeout, logger: logger,
		records: map[string]map[string]*record{},
		broken:  map[string]string{},
	}
}

func (r *recordSet) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, r.timeout)
}

// load enumerates every datapoint of the DPT once. Unreadable records are
// remembered as broken so they are never overwritten silently.
func (r *recordSet) load(parent context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return nil
	}
	ctx, cancel := r.ctx(parent)
	defer cancel()
	names, err := r.api.DpNames(ctx, r.dpt+"_*", r.dpt, r.timeout)
	if err != nil {
		return fmt.Errorf("oastore: enumerate %s: %w", r.dpt, err)
	}
	for i := 0; i < len(names); i += enumBatch {
		end := min(i+enumBatch, len(names))
		batch := names[i:end]
		addrs := make([]string, len(batch))
		for j, n := range batch {
			addrs[j] = n + "." + r.element + ":_online.._value"
		}
		vals, err := r.api.DpGet(ctx, addrs, r.timeout)
		if err != nil {
			return fmt.Errorf("oastore: read %s: %w", r.dpt, err)
		}
		for j, v := range vals {
			dp := stripSystem(batch[j])
			if v.Kind != oahost.KindString || v.Str == "" {
				continue // created but never committed: no record
			}
			env, err := decodeEnvelope(v.Str)
			if err != nil {
				r.broken[dp] = err.Error()
				r.logger.Warn("oastore: unreadable record kept untouched", "dp", dp, "err", err)
				continue
			}
			if DPName(r.dpt, env.Kind, env.Key) != dp {
				r.broken[dp] = "name does not match stored key"
				r.logger.Warn("oastore: record name mismatch kept untouched", "dp", dp, "key", env.Key)
				continue
			}
			r.kind(env.Kind)[env.Key] = &record{dp: dp, env: env}
		}
	}
	r.loaded = true
	return nil
}

func stripSystem(name string) string {
	if i := strings.Index(name, ":"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func decodeEnvelope(s string) (envelope, error) {
	var env envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return env, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if env.V != envelopeVersion {
		return env, fmt.Errorf("%w: %d", ErrVersion, env.V)
	}
	if env.Kind == "" || env.Key == "" {
		return env, fmt.Errorf("%w: missing kind or key", ErrCorrupt)
	}
	return env, nil
}

func (r *recordSet) kind(k string) map[string]*record {
	m := r.records[k]
	if m == nil {
		m = map[string]*record{}
		r.records[k] = m
	}
	return m
}

func (r *recordSet) get(ctx context.Context, kind, key string, out any) (bool, error) {
	if err := r.load(ctx); err != nil {
		return false, err
	}
	r.mu.Lock()
	rec := r.kind(kind)[key]
	var data json.RawMessage
	if rec != nil {
		data = rec.env.Data
	}
	r.mu.Unlock()
	if rec == nil {
		return false, nil
	}
	return true, json.Unmarshal(data, out)
}

// all decodes every record of kind, ordered by key.
func (r *recordSet) all(ctx context.Context, kind string, each func(key string, data json.RawMessage) error) error {
	if err := r.load(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	keys := make([]string, 0, len(r.kind(kind)))
	datas := map[string]json.RawMessage{}
	for k, rec := range r.kind(kind) {
		keys = append(keys, k)
		datas[k] = rec.env.Data
	}
	r.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		if err := each(k, datas[k]); err != nil {
			return err
		}
	}
	return nil
}

// update performs a serialized read-modify-write of kind/key: fn receives
// the current data (nil when absent) and returns the new value, or
// skip=true to leave the record unchanged. extra holds further element
// values written in the same confirmed dpSet.
func (r *recordSet) update(parent context.Context, kind, key string, fn func(cur json.RawMessage) (next any, extra map[string]oahost.Value, skip bool, err error)) error {
	if err := r.load(parent); err != nil {
		return err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	dp := DPName(r.dpt, kind, key)
	r.mu.Lock()
	why, bad := r.broken[dp]
	prev := r.kind(kind)[key]
	var cur json.RawMessage
	if prev != nil {
		cur = prev.env.Data
	}
	r.mu.Unlock()
	if bad {
		return fmt.Errorf("%w: %s (%s)", ErrCorrupt, dp, why)
	}
	next, extra, skip, err := fn(cur)
	if err != nil || skip {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	env := envelope{V: envelopeVersion, Kind: kind, Key: key, Rev: 1, Data: raw}
	if prev != nil {
		env.Rev = prev.env.Rev + 1
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(body) > MaxEnvelope {
		return fmt.Errorf("%w: record %s/%s is %d bytes (max %d)", oahost.ErrTooLarge, kind, key, len(body), MaxEnvelope)
	}
	ctx, cancel := r.ctx(parent)
	defer cancel()
	if prev == nil {
		// Lifecycle calls take the DP name without a trailing dot.
		if err := r.api.DpCreate(ctx, dp, r.dpt, r.timeout); err != nil && !isExists(err) {
			return fmt.Errorf("%w: create %s: %v", oahost.ErrPersist, dp, err)
		}
	}
	names := []string{dp + "." + r.element + ":_original.._value"}
	values := []oahost.Value{{Kind: oahost.KindString, Str: string(body)}}
	for el, v := range extra {
		names = append(names, dp+"."+el+":_original.._value")
		values = append(values, v)
	}
	names = append(names, dp+".updated:_original.._value")
	values = append(values, oahost.Value{Kind: oahost.KindTime, Time: time.Now().UTC()})
	if err := r.api.DpSet(ctx, names, values, r.timeout); err != nil {
		return fmt.Errorf("%w: write %s: %v", oahost.ErrPersist, dp, err)
	}
	r.mu.Lock()
	r.kind(kind)[key] = &record{dp: dp, env: env}
	r.mu.Unlock()
	return nil
}

// put replaces kind/key with data.
func (r *recordSet) put(ctx context.Context, kind, key string, data any, extra map[string]oahost.Value) error {
	return r.update(ctx, kind, key, func(json.RawMessage) (any, map[string]oahost.Value, bool, error) {
		return data, extra, false, nil
	})
}

func isExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exists")
}

func (r *recordSet) del(parent context.Context, kind, key string) error {
	if err := r.load(parent); err != nil {
		return err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	r.mu.Lock()
	rec := r.kind(kind)[key]
	r.mu.Unlock()
	if rec == nil {
		return nil
	}
	ctx, cancel := r.ctx(parent)
	defer cancel()
	// Lifecycle call: DP name without a trailing dot.
	if err := r.api.DpDelete(ctx, rec.dp, r.timeout); err != nil && !errors.Is(err, oahost.ErrNotFound) {
		return fmt.Errorf("%w: delete %s: %v", oahost.ErrPersist, rec.dp, err)
	}
	r.mu.Lock()
	delete(r.kind(kind), key)
	r.mu.Unlock()
	return nil
}
