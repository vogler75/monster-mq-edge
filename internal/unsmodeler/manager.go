package unsmodeler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/winccoabrowse"
)

var (
	// ErrNoDevice is returned for an unknown device name.
	ErrNoDevice = errors.New("unknown UNS Modeler device")
	// ErrNotEmbedded is returned when the broker does not run in WinCC OA.
	ErrNotEmbedded = errors.New("UNS Modeler needs the broker embedded in WinCC OA (WCCOAmmq)")
	// ErrInvalidDocument rejects a save of a document that is too large or
	// not well-formed JSON.
	ErrInvalidDocument = errors.New("mapping document rejected")
)

// Manager runs the UNS Modeler devices of this node.
type Manager struct {
	store  stores.DeviceConfigStore
	cfgMu  sync.Mutex // serializes read-modify-writes of device configs
	browse *winccoabrowse.Service
	api    oahost.API
	pub    Publisher
	gate   RoleGate
	roots  TopicRoots
	nodeID string
	log    *slog.Logger

	providers  ProviderLookup
	runs       aiRuns
	topics     TopicSource
	subscriber Subscriber

	mu      sync.Mutex
	devices map[string]*device
}

type device struct {
	cfg      stores.DeviceConfig
	settings *Settings
	rt       *RuntimeA
	flow     *RuntimeB
	cnt      *Counters
	applied  time.Time
	applyErr string
}

// NewManager creates the manager. browse is nil when the broker does not
// run embedded in WinCC OA; devices are then stored but never run.
func NewManager(store stores.DeviceConfigStore, browse *winccoabrowse.Service, pub Publisher, gate RoleGate, roots TopicRoots, nodeID string, logger *slog.Logger) *Manager {
	if gate == nil {
		gate = AlwaysActive{}
	}
	m := &Manager{store: store, browse: browse, pub: pub, gate: gate, roots: roots, nodeID: nodeID,
		log: logger.With("logger", "UnsModeler"), devices: map[string]*device{}, runs: aiRuns{last: map[string]*AiRunStatus{}}}
	if browse != nil {
		m.api = browse.API()
	}
	return m
}

// Roots returns the reserved topic roots the device checks.
func (m *Manager) Roots() TopicRoots { return m.roots }

// Gate returns the role gate.
func (m *Manager) Gate() RoleGate { return m.gate }

// Browse returns the browse service (nil standalone).
func (m *Manager) Browse() *winccoabrowse.Service { return m.browse }

func onNode(d stores.DeviceConfig, node string) bool {
	return d.NodeID == node || d.NodeID == "local" || d.NodeID == "*" || d.NodeID == ""
}

// Start runs every enabled device of this node and resumes its applied
// mapping.
func (m *Manager) Start(ctx context.Context) error {
	return m.Reload(ctx)
}

// Reload starts, restarts and stops devices after configuration changes.
func (m *Manager) Reload(ctx context.Context) error {
	all, err := m.store.GetByType(ctx, DeviceType)
	if err != nil {
		return err
	}
	wanted := map[string]stores.DeviceConfig{}
	for _, d := range all {
		if d.Enabled && onNode(d, m.nodeID) {
			wanted[d.Name] = d
		}
	}
	m.mu.Lock()
	var stop []*device
	for name, dev := range m.devices {
		next, keep := wanted[name]
		if !keep || settingsKey(next.Config) != settingsKey(dev.cfg.Config) {
			stop = append(stop, dev)
			delete(m.devices, name)
		}
	}
	var start []stores.DeviceConfig
	for name, d := range wanted {
		if _, running := m.devices[name]; !running {
			start = append(start, d)
		}
	}
	m.mu.Unlock()
	for _, dev := range stop {
		dev.stop()
		m.log.Info("uns modeler device stopped", "device", dev.cfg.Name)
	}
	sort.Slice(start, func(i, j int) bool { return start[i].Name < start[j].Name })
	for _, d := range start {
		m.startDevice(ctx, d)
	}
	return nil
}

func (m *Manager) startDevice(ctx context.Context, d stores.DeviceConfig) {
	s, err := ParseSettings(d.Config)
	if err != nil {
		m.log.Warn("uns modeler device config invalid", "device", d.Name, "err", err)
		return
	}
	dev := &device{cfg: d, settings: s, cnt: &Counters{}}
	m.mu.Lock()
	m.devices[d.Name] = dev
	m.mu.Unlock()
	if m.browse == nil {
		m.log.Warn("uns modeler device not started", "device", d.Name, "err", ErrNotEmbedded)
		return
	}
	if s.Redundancy == RedundancyColdStandby && !m.gate.Active() {
		m.log.Info("uns modeler device not started on STANDBY (COLD_STANDBY)", "device", d.Name)
		return
	}
	if issues := ValidateSettings(s, m.roots); HasErrors(issues) {
		m.log.Warn("uns modeler device settings invalid", "device", d.Name, "issues", issues)
		return
	}
	if s.Direction == DirectionMqttToWinccoa {
		m.startFlow(ctx, dev)
		return
	}
	rt := NewRuntimeA(d.Name, m.api, m.pub, m.deviceGate(s), dev.cnt, m.log)
	m.mu.Lock()
	dev.rt = rt
	m.mu.Unlock()
	raw, err := m.getDoc(ctx, d.Name, SlotApplied)
	if err != nil {
		m.log.Warn("uns modeler applied mapping not readable", "device", d.Name, "err", err)
		return
	}
	if raw == nil {
		m.log.Info("uns modeler device started without applied mapping", "device", d.Name)
		return
	}
	// Resume: no blocking validation; what cannot be connected is skipped
	// and logged until the next apply.
	if err := m.resume(ctx, dev, raw); err != nil {
		m.log.Warn("uns modeler mapping not resumed", "device", d.Name, "err", err)
	}
}

// deviceGate applies the redundancy mode: ALWAYS ignores the role.
func (m *Manager) deviceGate(s *Settings) RoleGate {
	if s.Redundancy == RedundancyAlways {
		return AlwaysActive{}
	}
	return m.gate
}

func (m *Manager) expansion(ctx context.Context, s *Settings, doc *Document) *Expansion {
	td, pd := s.Defaults()
	return Expand(ctx, doc, ExpandOptions{UnsRoot: s.UnsRoot, TopicDefaults: td, PayloadDefaults: pd, AllowWrite: s.AllowWrite}, m.browse)
}

func (m *Manager) resume(ctx context.Context, dev *device, raw []byte) error {
	if issues, err := ValidateSchema(raw); err != nil || len(issues) > 0 {
		return fmt.Errorf("applied document is not valid: %v %v", err, issues)
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		return err
	}
	exp := m.expansion(ctx, dev.settings, doc)
	for _, is := range exp.Issues {
		if is.Severity == SeverityError {
			dev.cnt.Errors.Add(1)
			m.log.Warn("uns binding skipped on resume", "device", dev.cfg.Name, "path", is.Path, "issue", is.Message)
		}
	}
	rt, _ := m.runtimes(dev)
	if err := rt.Apply(ctx, exp, dev.settings.AllowWrite); err != nil {
		return err
	}
	rt.RefreshMeta(ctx)
	return nil
}

// runtimes returns the runtimes of a device under the manager lock.
func (m *Manager) runtimes(dev *device) (*RuntimeA, *RuntimeB) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return dev.rt, dev.flow
}

func (d *device) stop() {
	if d.rt != nil {
		d.rt.Stop()
	}
	if d.flow != nil {
		d.flow.Stop()
	}
}

// startFlow starts the Direction B value flow of the applied document.
func (m *Manager) startFlow(ctx context.Context, dev *device) {
	if !dev.settings.Flow() {
		m.log.Info("uns flow disabled (one-time onboarding)", "device", dev.cfg.Name)
		return
	}
	raw, err := m.getDoc(ctx, dev.cfg.Name, SlotApplied)
	if err != nil || raw == nil {
		return
	}
	if issues, _ := ValidateSchema(raw); len(issues) > 0 {
		m.log.Warn("uns flow not started: applied document is not valid", "device", dev.cfg.Name)
		return
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		return
	}
	targets, problems := flowTargets(ctx, m.browse, doc)
	for _, p := range problems {
		dev.cnt.Errors.Add(1)
		m.log.Warn("uns flow binding skipped", "device", dev.cfg.Name, "issue", p)
	}
	flow, err := NewRuntimeB(dev.cfg.Name, m.api, m.subscriber, m.deviceGate(dev.settings), dev.cnt, m.log, targets)
	if err != nil {
		m.log.Warn("uns flow not started", "device", dev.cfg.Name, "err", err)
	}
	m.mu.Lock()
	dev.flow = flow
	m.mu.Unlock()
	if flow != nil && m.topics != nil {
		flow.Seed(ctx, m.topics)
	}
	m.log.Info("uns flow started", "device", dev.cfg.Name, "topics", len(targets))
}

// restartFlow restarts the Direction B flow after Create in WinCC OA.
func (m *Manager) restartFlow(ctx context.Context, name string) {
	m.mu.Lock()
	dev := m.devices[name]
	m.mu.Unlock()
	if dev == nil {
		return
	}
	m.mu.Lock()
	flow := dev.flow
	dev.flow = nil
	m.mu.Unlock()
	if flow != nil {
		flow.Stop()
	}
	m.startFlow(ctx, dev)
}

// Stop stops every device.
func (m *Manager) Stop() {
	m.runs.mu.Lock()
	for _, st := range m.runs.last {
		if st.Status == "running" {
			st.cancel(context.Canceled)
			st.cancelRequested = true
		}
	}
	m.runs.mu.Unlock()
	m.mu.Lock()
	devs := m.devices
	m.devices = map[string]*device{}
	m.mu.Unlock()
	for _, d := range devs {
		d.stop()
	}
}

func (m *Manager) deviceConfig(ctx context.Context, name string) (*stores.DeviceConfig, *Settings, error) {
	d, err := m.store.Get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if d == nil || d.Type != DeviceType {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoDevice, name)
	}
	s, err := ParseSettings(d.Config)
	if err != nil {
		return nil, nil, err
	}
	return d, s, nil
}

// Env returns the validation environment of a device.
func (m *Manager) Env(name string, s *Settings) ValidationEnv {
	env := ValidationEnv{Settings: s, Roots: m.roots, OtherPublishers: func(topic string) []string { return m.otherPublishers(name, topic) },
		DirectionAPublishes: func(topic string) string {
			others := m.otherPublishers("", topic)
			if len(others) > 0 {
				return others[0]
			}
			return ""
		}}
	if m.browse != nil {
		env.OA = m.browse
	}
	if m.topics != nil {
		src := m.topics
		env.Sample = func(topic string) ([]byte, bool) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return src.Last(ctx, topic)
		}
		env.ArchiveGroupExists = src.ArchiveGroupExists
	}
	return env
}

func (m *Manager) otherPublishers(self, topic string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for name, d := range m.devices {
		if name == self || d.rt == nil {
			continue
		}
		if d.rt.Target(topic) != nil {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Draft returns the stored draft of a device (nil when there is none).
func (m *Manager) Draft(ctx context.Context, name string) (json.RawMessage, error) {
	if _, _, err := m.deviceConfig(ctx, name); err != nil {
		return nil, err
	}
	return m.getDoc(ctx, name, SlotDraft)
}

// Applied returns the applied document of a device (nil when there is none).
func (m *Manager) Applied(ctx context.Context, name string) (json.RawMessage, error) {
	if _, _, err := m.deviceConfig(ctx, name); err != nil {
		return nil, err
	}
	return m.getDoc(ctx, name, SlotApplied)
}

// fatalIssue returns the stage-1 issue that prevents storing a document:
// too large, not valid JSON or nested too deeply.
func fatalIssue(raw []byte) *Issue {
	if len(raw) > MaxDocumentBytes {
		return &Issue{Severity: SeverityError, Path: "", Message: fmt.Sprintf("document has %d bytes, more than the limit of %d", len(raw), MaxDocumentBytes)}
	}
	if _, issues, ok := decodeDocument(raw); !ok {
		return &Issue{Severity: SeverityError, Path: issues[0].Path, Message: issues[0].Message}
	}
	return nil
}

// ValidateDocument validates a document for a device without storing it.
func (m *Manager) ValidateDocument(ctx context.Context, name string, raw []byte) ([]Issue, error) {
	_, s, err := m.deviceConfig(ctx, name)
	if err != nil {
		return nil, err
	}
	res, err := Validate(ctx, raw, m.Env(name, s))
	if err != nil {
		return nil, err
	}
	return res.Issues, nil
}

// SaveDraft stores a draft. Every node without an id gets one. A document
// that is too large or not well-formed JSON is rejected with its stage-1
// issue; any other draft is stored whatever its issues (AC-07). It returns
// the stored document and its issues.
func (m *Manager) SaveDraft(ctx context.Context, name string, raw []byte) (json.RawMessage, []Issue, error) {
	_, s, err := m.deviceConfig(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if is := fatalIssue(raw); is != nil {
		return nil, []Issue{*is}, ErrInvalidDocument
	}
	doc, _, _ := AssignNodeIDs(raw)
	if len(doc) > MaxDocumentBytes {
		return nil, []Issue{*fatalIssue(doc)}, ErrInvalidDocument
	}
	if err := m.saveDoc(ctx, name, SlotDraft, doc); err != nil {
		return nil, []Issue{{Severity: SeverityError, Path: "", Message: "document not stored: " + err.Error()}}, err
	}
	res, err := Validate(ctx, doc, m.Env(name, s))
	if err != nil {
		return doc, nil, err
	}
	return doc, res.Issues, nil
}

// Apply validates the draft and, without errors, stores it as the applied
// document and connects it. Warnings never block.
func (m *Manager) Apply(ctx context.Context, name string) ([]Issue, error) {
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
	raw, err := m.getDoc(ctx, name, SlotDraft)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("the device has no draft to apply")
	}
	if s.Direction != DirectionWinccoaToUns {
		return nil, errors.New("Direction B is applied with createInWinCCOa")
	}
	res, err := Validate(ctx, raw, m.Env(name, s))
	if err != nil {
		return nil, err
	}
	if HasErrors(res.Issues) {
		m.log.Info("uns mapping apply refused: validation errors", "device", name, "issues", len(res.Issues))
		return res.Issues, nil
	}
	if err := m.saveDoc(ctx, name, SlotApplied, raw); err != nil {
		return res.Issues, err
	}
	m.mu.Lock()
	dev := m.devices[name]
	m.mu.Unlock()
	var rt *RuntimeA
	if dev != nil {
		rt, _ = m.runtimes(dev)
	}
	if rt == nil {
		m.log.Info("uns mapping applied; device not running", "device", name)
		return res.Issues, nil
	}
	if err := rt.Apply(ctx, res.Expansion, s.AllowWrite); err != nil {
		m.mu.Lock()
		dev.applyErr = err.Error()
		m.mu.Unlock()
		return res.Issues, err
	}
	m.mu.Lock()
	dev.applied, dev.applyErr = time.Now(), ""
	m.mu.Unlock()
	return res.Issues, nil
}

// Delete stops a device; its documents go with its device record.
func (m *Manager) Delete(ctx context.Context, name string) error {
	m.mu.Lock()
	dev := m.devices[name]
	delete(m.devices, name)
	m.mu.Unlock()
	if dev != nil {
		dev.stop()
	}
	return nil
}

// DeviceStatus is the runtime state of one device.
type DeviceStatus struct {
	Running     bool
	Topics      int
	DPEs        int64
	Published   int64
	Writes      int64
	Errors      int64
	LastApplied time.Time
	ApplyError  string
	Active      bool
}

// Status returns the runtime state of a device.
func (m *Manager) Status(name string) DeviceStatus {
	m.mu.Lock()
	dev := m.devices[name]
	var rt *RuntimeA
	var flow *RuntimeB
	var applied time.Time
	var applyErr string
	if dev != nil {
		rt, flow, applied, applyErr = dev.rt, dev.flow, dev.applied, dev.applyErr
	}
	m.mu.Unlock()
	if dev == nil {
		return DeviceStatus{Active: m.gate.Active()}
	}
	st := DeviceStatus{Running: rt != nil || flow != nil, DPEs: dev.cnt.DPEs.Load(), Published: dev.cnt.Published.Load(), Writes: dev.cnt.Writes.Load(),
		Errors: dev.cnt.Errors.Load(), LastApplied: applied, ApplyError: applyErr, Active: m.deviceGate(dev.settings).Active()}
	if rt != nil {
		st.Topics = len(rt.Topics())
	}
	if flow != nil {
		st.Topics = int(dev.cnt.DPEs.Load())
	}
	return st
}

// LiveTopics returns the live state of every topic of a device.
func (m *Manager) LiveTopics(name string) []TopicStatus {
	m.mu.Lock()
	dev := m.devices[name]
	var rt *RuntimeA
	var flow *RuntimeB
	if dev != nil {
		rt, flow = dev.rt, dev.flow
	}
	m.mu.Unlock()
	if flow != nil {
		return flow.Status()
	}
	if rt == nil {
		return nil
	}
	return rt.Status()
}

// IsSetTopic reports whether topic is a write-through command topic.
func IsSetTopic(topic string) bool { return len(topic) > 6 && topic[len(topic)-6:] == "/..set" }

// Write dispatches a publish on <uns-topic>/..set to the device that maps
// the topic. handled is false when no device maps it.
func (m *Manager) Write(topic string, payload []byte) (handled bool, err error) {
	if !IsSetTopic(topic) {
		return false, nil
	}
	base := topic[:len(topic)-6]
	m.mu.Lock()
	names := make([]string, 0, len(m.devices))
	for n, d := range m.devices {
		if d.rt != nil {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	rts := make([]*RuntimeA, len(names))
	for i, n := range names {
		rts[i] = m.devices[n].rt
	}
	m.mu.Unlock()
	var firstErr error
	for _, rt := range rts {
		h, err := rt.Write(base, payload)
		if !h {
			continue
		}
		handled = true
		if err == nil {
			return true, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return handled, firstErr
}

// SysTopics returns the per-device counters for $SYS.
func (m *Manager) SysTopics() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for name, d := range m.devices {
		p := "unsmodeler/" + name + "/"
		out[p+"dpes"] = itoa(d.cnt.DPEs.Load())
		out[p+"published"] = itoa(d.cnt.Published.Load())
		out[p+"writes"] = itoa(d.cnt.Writes.Load())
		out[p+"errors"] = itoa(d.cnt.Errors.Load())
	}
	return out
}
