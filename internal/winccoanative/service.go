package winccoanative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/oahost"
)

// Verdict classifies a subscription or command decision; the broker hook
// maps it to MQTT reason codes (spec section 4.1 and 8).
type Verdict int

const (
	Accept       Verdict = iota
	NotNative            // not in a reserved branch: ordinary MQTT
	Denied               // ACL / reserved-branch write
	Invalid              // malformed, missing DPE, not a value element, type mismatch
	Unavailable          // OA or remote system unavailable, timeout, overload
	WildcardDeny         // wildcard in a native branch
	SharedDeny           // shared subscription in a native branch
	Disabled             // CNS branch
	BadPayload           // command payload/type conversion
	Retained             // retained command
)

func (v Verdict) String() string {
	return [...]string{"accept", "not-native", "denied", "invalid", "unavailable", "wildcard", "shared", "disabled", "bad-payload", "retained"}[v]
}

// Broker is the MQTT side the service publishes through.
type Broker interface {
	Publish(topic string, payload []byte, retain bool, qos byte) error
	PublishCurrentValue(clientID, filter, topic string, payload []byte, existed bool) error
}

type Options struct {
	// Names are the topic levels of the namespace; zero means DefaultNames.
	Names          Names
	NodeID         string
	NoSource       bool
	ResolveTimeout time.Duration
	WriteTimeout   time.Duration
	ConnectTimeout time.Duration
	BatchSize      int
	MaxInterests   int
	CatalogTTL     time.Duration
	// ReconcileInterval paces retries of unregistered interests and the
	// removal of interests whose session is gone.
	ReconcileInterval time.Duration
	// AllowRootWildcard permits wildcard filters that cover every
	// datapoint (tags/#, tags/+/..., types/#), like '#' for MQTT.
	AllowRootWildcard bool
	// MaxWildcardQueries bounds the number of distinct wildcard queries.
	MaxWildcardQueries int
	// SessionExists reports whether a client session still exists; used to
	// drop interests of sessions that expired while not in memory.
	SessionExists func(clientID string) bool
}

func (o *Options) defaults() {
	o.Names = o.Names.WithDefaults()
	if o.ResolveTimeout <= 0 {
		o.ResolveTimeout = 5 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 5 * time.Second
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 10 * time.Second
	}
	if o.BatchSize <= 0 || o.BatchSize > 100 {
		o.BatchSize = 100
	}
	if o.MaxInterests <= 0 {
		o.MaxInterests = 10000
	}
	if o.CatalogTTL <= 0 {
		o.CatalogTTL = time.Minute
	}
	if o.MaxWildcardQueries <= 0 {
		o.MaxWildcardQueries = 1000
	}
	if o.ReconcileInterval <= 0 {
		o.ReconcileInterval = 30 * time.Second
	}
}

type subKey struct{ client, filter string }

type subEntry struct {
	key     string // dpe key
	topic   string
	waiting bool // initial value not yet delivered
}

type dpeEntry struct {
	key    string // read address, e.g. System1:Pump.speed:_online.._value
	system string
	dp     string
	subs   map[subKey]*subEntry
	batch  *batch
	queued bool
	last   *oahost.Value
	lastTS time.Time
}

type batch struct {
	ref   uint64
	names []string
	live  int
}

type catalogEntry struct {
	res oahost.Resolution
	at  time.Time
}

type cmdRecord struct {
	at     time.Time
	result []byte
}

type Stats struct {
	Interests     int
	DPEs          int
	Batches       int
	Connects      uint64
	Disconnects   uint64
	ConnectErrors uint64
	Published     uint64
	Commands      uint64
	CommandErrors uint64
	Duplicates    uint64
	WildQueries   int
	WildSubs      int
	TopicDPs      int
	TopicSubs     int
	TopicWilds    int
	TopicDirs     int
}

// Service owns the native namespace state.
type Service struct {
	api    oahost.API
	broker Broker
	opts   Options
	logger *slog.Logger

	localSystem string

	mu      sync.Mutex
	subs    map[subKey]*subEntry
	dpes    map[string]*dpeEntry
	batches map[uint64]*batch
	batchOf map[string]*batch
	catalog map[string]catalogEntry
	cmds    map[string]cmdRecord
	queue   []string

	wild        map[string]*wildQuery
	wildSubs    map[subKey]string
	exactTopics map[string]int    // topics with an exact native subscription
	lastSig     map[string]string // topic -> last published change

	// topics branch
	topics   map[string]*topicEntry // Sys:MMQTopic_k... -> connection
	tsubs    map[subKey]*topicSub
	twild    map[subKey]*topicWild
	tdirs    map[string]*topicDir // system -> directory of its topic datapoints
	prepared map[string]bool      // Sys:MMQTopic_k... created and configured in this run
	prepMu   sync.Mutex           // serializes datapoint creation and deletion

	kick    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	unwatch func()
	ready   atomic.Bool
	oaUp    atomic.Bool

	connects, disconnects, connectErrs, published, commands, cmdErrs, dups atomic.Uint64
}

func NewService(api oahost.API, b Broker, opts Options, logger *slog.Logger) *Service {
	opts.defaults()
	return &Service{
		api:         api,
		broker:      b,
		opts:        opts,
		logger:      logger.With("component", "winccoa-native"),
		subs:        map[subKey]*subEntry{},
		dpes:        map[string]*dpeEntry{},
		batches:     map[uint64]*batch{},
		batchOf:     map[string]*batch{},
		catalog:     map[string]catalogEntry{},
		cmds:        map[string]cmdRecord{},
		wild:        map[string]*wildQuery{},
		wildSubs:    map[subKey]string{},
		exactTopics: map[string]int{},
		lastSig:     map[string]string{},
		topics:      map[string]*topicEntry{},
		tsubs:       map[subKey]*topicSub{},
		twild:       map[subKey]*topicWild{},
		tdirs:       map[string]*topicDir{},
		prepared:    map[string]bool{},
		kick:        make(chan struct{}, 1),
	}
}

// Start resolves the local system name and starts the connect worker. It
// must not run on the OA manager thread: it waits for a host completion.
func (s *Service) Start(ctx context.Context) error {
	info, err := s.api.SysInfo(ctx)
	if err != nil {
		return fmt.Errorf("winccoa native: local system: %w", err)
	}
	if info.LocalSystem == "" {
		return errors.New("winccoa native: host reported an empty local system name")
	}
	s.localSystem = info.LocalSystem
	if err := s.ensureTopicType(ctx); err != nil {
		return fmt.Errorf("winccoa native: %w", err)
	}
	s.oaUp.Store(true)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	unSys := s.api.C.WatchSystems(s.onSystem)
	unDP := s.api.C.WatchDatapoints(func(sysDP string) {
		if sys, dp, ok := strings.Cut(sysDP, ":"); ok {
			s.Invalidate(sys, dp)
		}
	})
	unNew := s.api.C.WatchCreated(s.topicCreated)
	s.unwatch = func() { unSys(); unDP(); unNew() }
	s.wg.Add(1)
	go s.worker()
	s.ready.Store(true)
	s.PublishStatus()
	return nil
}

// Stop disconnects every OA registration while the host still dispatches.
func (s *Service) Stop(ctx context.Context) {
	if s.cancel == nil {
		return
	}
	s.ready.Store(false)
	s.cancel()
	s.wg.Wait()
	s.stopWild(ctx)
	if s.unwatch != nil {
		s.unwatch()
	}
	s.mu.Lock()
	refs := make([]uint64, 0, len(s.batches))
	for ref := range s.batches {
		refs = append(refs, ref)
	}
	s.batches = map[uint64]*batch{}
	s.batchOf = map[string]*batch{}
	for _, d := range s.dpes {
		d.batch = nil
	}
	for _, e := range s.topics {
		if e.live {
			refs = append(refs, e.ref)
			e.live = false
		}
	}
	var dirRefs []uint64
	for _, d := range s.tdirs {
		if d.live {
			dirRefs = append(dirRefs, d.ref)
			d.live = false
		}
	}
	s.mu.Unlock()
	for _, ref := range dirRefs {
		if err := s.api.QueryDisconnect(ctx, ref); err != nil {
			s.logger.Warn("topic directory disconnect on stop failed", "ref", ref, "err", err)
		}
	}
	for _, ref := range refs {
		if err := s.api.DpDisconnect(ctx, ref); err != nil {
			s.logger.Warn("dpDisconnect on stop failed", "ref", ref, "err", err)
		}
		s.disconnects.Add(1)
	}
}

func (s *Service) LocalSystem() string { return s.localSystem }

// Validate decides one SUBSCRIBE filter. It may wait for the OA manager
// thread (bounded by ResolveTimeout) and must not be called from it.
func (s *Service) Validate(filter string) (Verdict, string) {
	f, shared := SplitShared(filter)
	switch s.opts.Names.Classify(f) {
	case KindOther:
		return NotNative, ""
	case KindCNS:
		return Disabled, "CNS access is not enabled"
	case KindStatus:
		if shared {
			return SharedDeny, "shared subscription on status topic"
		}
		return Accept, ""
	case KindTopics:
		if shared {
			return SharedDeny, "shared subscriptions are not supported for native topics"
		}
		return s.validateTopic(f)
	}
	if shared {
		return SharedDeny, "shared subscriptions are not supported for native topics"
	}
	if HasWildcard(f) {
		return s.validateWild(f)
	}
	t, err := s.opts.Names.Parse(f)
	if err != nil {
		return Invalid, err.Error()
	}
	if t.Command {
		return Invalid, "command topics cannot be subscribed"
	}
	_, _, v, why := s.resolveTarget(t)
	return v, why
}

// Storage datapoint types of the native stores and the topics branch;
// never exposed as tags.
var protectedTypes = map[string]bool{"MMQConfigs": true, "MMQSessions": true, "MMQRetained": true, "MMQUsers": true, TopicType: true}

// Protected reports datapoints that the namespace never exposes: OA
// internal datapoints (leading underscore, e.g. _Users) and the native
// store datapoints.
func Protected(dp string) bool {
	return strings.HasPrefix(dp, "_") || strings.HasPrefix(dp, "MMQConfigs_") || strings.HasPrefix(dp, "MMQSessions_") || strings.HasPrefix(dp, "MMQRetained_") ||
		strings.HasPrefix(dp, "MMQUsers_") || strings.HasPrefix(dp, TopicType+"_")
}

// CanonicalTopic is the tags-form topic of t (type path and default
// attribute removed). Authorization must hold for it as well as for the
// requested alias, so an alias can never widen access.
func CanonicalTopic(t Target) string {
	c := t
	c.TypeName = ""
	c.Elements = append([]string(nil), t.Elements...)
	if c.Attr == DefaultAttr {
		c.Explicit = false
	}
	return c.Topic()
}

// CanonicalOf returns the canonical tags-form topic of an exact native
// topic; ok is false for other topics and filters with wildcards.
// A shortcut topic of the local system maps to its explicit
// <root>/<systems>/<local>/... form once n.Local is known.
func (n Names) CanonicalOf(topic string) (string, bool) {
	if n.Classify(topic) == KindTopics && !HasWildcard(topic) {
		t, err := n.ParseTopic(topic)
		if err != nil {
			return "", false
		}
		if t.System == "" {
			t.System = n.Local
		}
		return t.MQTTTopic(), true
	}
	if n.Classify(topic) != KindNative || HasWildcard(topic) {
		return "", false
	}
	t, err := n.Parse(topic)
	if err != nil {
		return "", false
	}
	if t.System == "" {
		t.System = n.Local
	}
	return CanonicalTopic(t), true
}

// resolveTarget binds the system and checks the DPE on OA.
func (s *Service) resolveTarget(t Target) (string, oahost.Resolution, Verdict, string) {
	if Protected(t.DP) {
		return "", oahost.Resolution{}, Denied, "datapoint is not exposed"
	}
	if !s.ready.Load() || !s.oaUp.Load() {
		return "", oahost.Resolution{}, Unavailable, "WinCC OA not ready"
	}
	sys := s.sysOf(t.System)
	name := t.DPE(sys)
	res, err := s.lookup(name)
	if err != nil {
		if errors.Is(err, oahost.ErrNotFound) {
			return sys, res, Invalid, "datapoint element not found"
		}
		return sys, res, Unavailable, err.Error()
	}
	if !res.Exists {
		return sys, res, Invalid, "datapoint element not found"
	}
	if protectedTypes[res.TypeName] {
		return sys, res, Denied, "datapoint is not exposed"
	}
	if res.ElemType == 0 {
		return sys, res, Invalid, "not a value element"
	}
	if res.ElemType == 255 {
		return sys, res, Invalid, "element type not supported"
	}
	if res.System != "" && res.System != sys {
		return sys, res, Invalid, "system mismatch"
	}
	if t.TypeName != "" && t.TypeName != res.TypeName {
		return sys, res, Invalid, fmt.Sprintf("datapoint type is %s, not %s", res.TypeName, t.TypeName)
	}
	return sys, res, Accept, ""
}

func (s *Service) lookup(name string) (oahost.Resolution, error) {
	s.mu.Lock()
	if c, ok := s.catalog[name]; ok && time.Since(c.at) < s.opts.CatalogTTL {
		s.mu.Unlock()
		return c.res, nil
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ResolveTimeout)
	defer cancel()
	res, err := s.api.Resolve(ctx, name)
	if err != nil {
		return res, err
	}
	if res.Exists {
		s.mu.Lock()
		s.catalog[name] = catalogEntry{res: res, at: time.Now()}
		s.mu.Unlock()
	}
	return res, nil
}

// Subscribed registers accepted native filters as interests. existed tells
// whether the (client, filter) subscription already existed (MQTT retain
// handling 1).
func (s *Service) Subscribed(clientID, filter string, existed bool) {
	f, shared := SplitShared(filter)
	if !shared && s.opts.Names.Classify(f) == KindTopics {
		if HasWildcard(f) {
			s.subscribedTopicWild(clientID, f, existed, false)
		} else {
			s.subscribedTopic(clientID, f, existed, false)
		}
		return
	}
	if shared || s.opts.Names.Classify(f) != KindNative {
		return
	}
	if HasWildcard(f) {
		s.subscribedWild(clientID, f, existed, false, false)
		return
	}
	t, err := s.opts.Names.Parse(f)
	if err != nil || t.Command {
		return
	}
	key := t.Key(s.sysOf(t.System))
	sk := subKey{clientID, filter}

	s.mu.Lock()
	if old, ok := s.subs[sk]; ok && old.key == key {
		d := s.dpes[key]
		var last *oahost.Value
		var ts time.Time
		if d != nil && d.last != nil {
			v := *d.last
			last, ts = &v, d.lastTS
		}
		s.mu.Unlock()
		if last != nil {
			_ = s.broker.PublishCurrentValue(clientID, filter, f, ValuePayload(*last, ts), true)
		}
		return
	}
	d := s.dpes[key]
	if d == nil {
		if len(s.dpes) >= s.opts.MaxInterests {
			s.mu.Unlock()
			s.logger.Warn("native interest limit reached", "limit", s.opts.MaxInterests, "filter", filter)
			return
		}
		d = &dpeEntry{key: key, system: s.sysOf(t.System), dp: t.DP, subs: map[subKey]*subEntry{}}
		s.dpes[key] = d
	}
	e := &subEntry{key: key, topic: f, waiting: true}
	s.subs[sk] = e
	d.subs[sk] = e
	s.exactTopics[f]++
	var deliver *oahost.Value
	var ts time.Time
	if d.last != nil {
		v := *d.last
		deliver, ts = &v, d.lastTS
		e.waiting = false
	}
	s.attachLocked(d)
	s.mu.Unlock()
	if deliver != nil {
		_ = s.broker.PublishCurrentValue(clientID, filter, f, ValuePayload(*deliver, ts), existed)
	}
	s.wake()
}

// Unsubscribed removes interests for the given filters of a client.
func (s *Service) Unsubscribed(clientID string, filters []string) {
	var drop, dropDirs []uint64
	defer func() { s.disconnectQueryAsync(dropDirs) }()
	for _, filter := range filters {
		if HasWildcard(filter) {
			s.unsubscribedWild(subKey{clientID, filter})
		}
	}
	s.mu.Lock()
	for _, filter := range filters {
		sk := subKey{clientID, filter}
		if ref, ok := s.unsubscribedTopic(sk); ok {
			drop = append(drop, ref)
		}
		refs, dirRef := s.unsubscribedTopicWild(sk)
		drop = append(drop, refs...)
		if dirRef != 0 {
			dropDirs = append(dropDirs, dirRef)
		}
		e, ok := s.subs[sk]
		if !ok {
			continue
		}
		delete(s.subs, sk)
		if s.exactTopics[e.topic]--; s.exactTopics[e.topic] <= 0 {
			delete(s.exactTopics, e.topic)
		}
		d := s.dpes[e.key]
		if d == nil {
			continue
		}
		delete(d.subs, sk)
		if len(d.subs) > 0 {
			continue
		}
		delete(s.dpes, d.key)
		if b := d.batch; b != nil {
			b.live--
			if b.live <= 0 {
				s.dropBatchLocked(b)
				drop = append(drop, b.ref)
			}
		}
	}
	s.mu.Unlock()
	s.disconnectAsync(drop)
}

// disconnectAsync releases batches without blocking the caller, which may
// be an MQTT client goroutine or the host event goroutine.
func (s *Service) disconnectAsync(refs []uint64) {
	if len(refs) == 0 {
		return
	}
	go func() {
		for _, ref := range refs {
			s.disconnect(ref)
		}
	}()
}

func (s *Service) disconnect(ref uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ConnectTimeout)
	defer cancel()
	if err := s.api.DpDisconnect(ctx, ref); err != nil {
		s.logger.Warn("dpDisconnect failed", "ref", ref, "err", err)
		return
	}
	s.disconnects.Add(1)
}

// Restore re-creates interests of persisted subscriptions after a restart.
// It returns the filters that failed revalidation.
func (s *Service) Restore(subs map[string][]string) (invalid map[string][]string) {
	invalid = map[string][]string{}
	for client, filters := range subs {
		for _, filter := range filters {
			v, why := s.Validate(filter)
			switch v {
			case NotNative:
				continue
			case Accept, Unavailable:
				// Unavailable ones are kept: the system may come back and
				// the registration is retried.
				f, _ := SplitShared(filter)
				switch {
				case s.opts.Names.Classify(f) == KindTopics && HasWildcard(f):
					s.subscribedTopicWild(client, f, false, true)
				case s.opts.Names.Classify(f) == KindTopics:
					s.subscribedTopic(client, f, false, true)
				case s.opts.Names.Classify(f) != KindNative:
				case HasWildcard(f):
					s.subscribedWild(client, f, false, true, v == Unavailable)
				default:
					s.addRestored(client, filter)
				}
			default:
				s.logger.Warn("native subscription failed revalidation", "client", client, "filter", filter, "reason", why)
				invalid[client] = append(invalid[client], filter)
			}
		}
	}
	s.wake()
	return invalid
}

func (s *Service) addRestored(clientID, filter string) {
	t, err := s.opts.Names.Parse(filter)
	if err != nil {
		return
	}
	key := t.Key(s.sysOf(t.System))
	sk := subKey{clientID, filter}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sk]; ok {
		return
	}
	d := s.dpes[key]
	if d == nil {
		d = &dpeEntry{key: key, system: s.sysOf(t.System), dp: t.DP, subs: map[subKey]*subEntry{}}
		s.dpes[key] = d
	}
	e := &subEntry{key: key, topic: filter}
	s.subs[sk] = e
	d.subs[sk] = e
	s.exactTopics[filter]++
	s.attachLocked(d)
}

// attachLocked joins a DPE to a live batch that still carries its name
// (so re-subscribing never creates a second OA registration) or queues it.
func (s *Service) attachLocked(d *dpeEntry) {
	if d.batch != nil || d.queued {
		return
	}
	if b := s.batchOf[d.key]; b != nil {
		d.batch = b
		b.live++
		return
	}
	d.queued = true
	s.queue = append(s.queue, d.key)
}

// dropBatchLocked forgets a batch; the caller disconnects it.
func (s *Service) dropBatchLocked(b *batch) {
	delete(s.batches, b.ref)
	for _, n := range b.names {
		if s.batchOf[n] == b {
			delete(s.batchOf, n)
		}
	}
}

func (s *Service) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Service) worker() {
	defer s.wg.Done()
	reconcile := time.NewTicker(s.opts.ReconcileInterval)
	defer reconcile.Stop()
	for {
		s.connectQueued()
		s.connectTopics()
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		case <-reconcile.C:
			s.reconcile()
		}
	}
}

// connectQueued registers queued DPEs in batches of at most BatchSize.
// A failed batch leaves its DPEs queued for the next attempt and does not
// affect batches that succeeded.
func (s *Service) connectQueued() {
	for s.ctx.Err() == nil {
		// A batch never mixes systems: an outage of one system must only
		// drop registrations on that system.
		s.mu.Lock()
		var names []string
		var system string
		rest := s.queue[:0:0]
		for _, key := range s.queue {
			d := s.dpes[key]
			if d == nil || d.batch != nil {
				continue
			}
			if len(names) < s.opts.BatchSize && (system == "" || d.system == system) {
				system = d.system
				names = append(names, key)
				continue
			}
			rest = append(rest, key)
		}
		s.queue = rest
		s.mu.Unlock()
		if len(names) == 0 {
			return
		}
		var flags uint32 = oahost.FlagAnswer
		if s.opts.NoSource {
			flags |= oahost.FlagNoSource
		}
		ref, err := s.api.DpConnect(s.ctx, names, flags, s.onHotlink, s.opts.ConnectTimeout)
		if err != nil {
			s.connectErrs.Add(1)
			s.logger.Warn("native dpConnect batch failed", "count", len(names), "err", err)
			s.mu.Lock()
			for _, n := range names {
				if d := s.dpes[n]; d != nil {
					d.queued = false
				}
			}
			s.mu.Unlock()
			return
		}
		s.connects.Add(1)
		b := &batch{ref: ref, names: names}
		var orphan bool
		s.mu.Lock()
		for _, n := range names {
			if d := s.dpes[n]; d != nil && d.batch == nil {
				d.batch = b
				d.queued = false
				b.live++
			}
		}
		if b.live == 0 {
			orphan = true
		} else {
			s.batches[ref] = b
			for _, n := range names {
				s.batchOf[n] = b
			}
		}
		s.mu.Unlock()
		if orphan {
			s.disconnect(ref)
		}
	}
}

// onHotlink routes one DP_CONNECT event. Answer events (initial values)
// only go to subscribers still waiting for their current value; hotlinks
// are published to every subscribed alias topic.
func (s *Service) onHotlink(m oahost.Message) {
	flags, _ := m.U32(oahost.TagFlags)
	answer := flags&oahost.FlagAnswer != 0
	names, values, err := oahost.HotlinkItems(m)
	if err != nil {
		s.logger.Warn("native hotlink decode failed", "err", err)
		return
	}
	now := time.Now()
	for i, name := range names {
		s.apply(name, values[i], now, answer)
	}
}

type delivery struct {
	client, filter, topic string
}

func (s *Service) apply(key string, v oahost.Value, ts time.Time, answer bool) {
	s.mu.Lock()
	d := s.dpes[key]
	if d == nil {
		s.mu.Unlock()
		return
	}
	val := v
	d.last = &val
	d.lastTS = ts
	var initial []delivery
	topics := map[string]bool{}
	for sk, e := range d.subs {
		if e.waiting {
			initial = append(initial, delivery{sk.client, sk.filter, e.topic})
			e.waiting = false
			continue
		}
		if !answer {
			topics[e.topic] = true
		}
	}
	s.mu.Unlock()
	payload := ValuePayload(v, ts)
	for _, dl := range initial {
		_ = s.broker.PublishCurrentValue(dl.client, dl.filter, dl.topic, payload, false)
	}
	ordered := make([]string, 0, len(topics))
	for t := range topics {
		ordered = append(ordered, t)
	}
	sort.Strings(ordered)
	for _, t := range ordered {
		if err := s.broker.Publish(t, payload, false, 1); err != nil {
			s.logger.Warn("native publish failed", "topic", t, "err", err)
			continue
		}
		s.published.Add(1)
	}
}

// onSystem invalidates state for a system that went away and re-registers
// its interests when it returns. The host already released its side of the
// registrations on that system, so only Go-side routes are dropped.
func (s *Service) onSystem(system string, available bool) {
	if system == s.localSystem {
		s.oaUp.Store(available)
		defer s.PublishStatus()
	}
	s.wildOnSystem(system, available)
	var lost []uint64
	defer func() { s.disconnectAsync(lost) }()
	s.mu.Lock()
	lostTopics, lostDirs := s.topicsOnSystemLocked(system, available)
	lost = append(lost, lostTopics...)
	defer s.disconnectQueryAsync(lostDirs)
	for name := range s.catalog {
		if strings.HasPrefix(name, system+":") {
			delete(s.catalog, name)
		}
	}
	for ref, b := range s.batches {
		hit := false
		for _, n := range b.names {
			if strings.HasPrefix(n, system+":") {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		s.dropBatchLocked(b)
		// The host keeps registrations on a lost system; release them so a
		// reconnect does not leave a second registration behind.
		lost = append(lost, ref)
		for _, n := range b.names {
			if d := s.dpes[n]; d != nil && d.batch == b {
				d.batch = nil
			}
		}
	}
	for _, d := range s.dpes {
		if d.system != system {
			continue
		}
		// A value from before the outage must not be replayed as current.
		d.last = nil
		for _, e := range d.subs {
			e.waiting = true
		}
		if available && d.batch == nil && !d.queued {
			d.queued = true
			s.queue = append(s.queue, d.key)
		}
	}
	s.mu.Unlock()
	if available {
		s.wake()
	}
}

// Invalidate drops cached resolution and values of a deleted or changed DP
// (reported by the host). Interests stay, but get no value until the DP
// resolves again.
func (s *Service) Invalidate(system, dp string) {
	prefix := system + ":" + dp + "."
	var drop []uint64
	s.mu.Lock()
	if ref, ok := s.invalidateTopicLocked(system + ":" + dp); ok {
		drop = append(drop, ref)
	}
	for name := range s.catalog {
		if strings.HasPrefix(name, prefix) {
			delete(s.catalog, name)
		}
	}
	for _, d := range s.dpes {
		if !strings.HasPrefix(d.key, prefix) {
			continue
		}
		delete(s.batchOf, d.key)
		d.last = nil
		for _, e := range d.subs {
			e.waiting = true
		}
		if b := d.batch; b != nil {
			d.batch = nil
			b.live--
			if b.live <= 0 {
				s.dropBatchLocked(b)
				drop = append(drop, b.ref)
			}
		}
	}
	s.mu.Unlock()
	s.disconnectAsync(drop)
}

// reconcile retries unregistered interests (their DPE may exist again) and
// drops interests whose session no longer exists.
func (s *Service) reconcile() {
	type stale struct {
		client  string
		filters []string
	}
	var gone []stale
	s.mu.Lock()
	if s.opts.SessionExists != nil {
		byClient := map[string][]string{}
		for sk := range s.subs {
			byClient[sk.client] = append(byClient[sk.client], sk.filter)
		}
		for sk := range s.tsubs {
			byClient[sk.client] = append(byClient[sk.client], sk.filter)
		}
		for sk := range s.twild {
			byClient[sk.client] = append(byClient[sk.client], sk.filter)
		}
		s.mu.Unlock()
		for c, fs := range byClient {
			if !s.opts.SessionExists(c) {
				gone = append(gone, stale{c, fs})
			}
		}
		s.mu.Lock()
	}
	var retry []string
	for key, d := range s.dpes {
		if d.batch == nil && !d.queued {
			retry = append(retry, key)
		}
	}
	for _, e := range s.topics {
		if !e.live && !e.inflight {
			e.due = true
		}
	}
	for _, d := range s.tdirs {
		if !d.live && !d.inflight {
			d.due = true
		}
	}
	s.mu.Unlock()
	for _, g := range gone {
		s.Unsubscribed(g.client, g.filters)
	}
	for _, key := range retry {
		t, err := s.targetForKey(key)
		if err != nil {
			continue
		}
		if _, _, v, _ := s.resolveTarget(t); v != Accept {
			continue
		}
		s.mu.Lock()
		if d := s.dpes[key]; d != nil && d.batch == nil && !d.queued {
			d.queued = true
			s.queue = append(s.queue, key)
		}
		s.mu.Unlock()
	}
	s.wake()
	s.pruneCommands()
}

func (s *Service) targetForKey(key string) (Target, error) {
	s.mu.Lock()
	var topic string
	if d := s.dpes[key]; d != nil {
		for _, e := range d.subs {
			topic = e.topic
			break
		}
	}
	s.mu.Unlock()
	if topic == "" {
		return Target{}, errors.New("no topic")
	}
	return s.opts.Names.Parse(topic)
}

// CommandResult is the JSON published to the reply topic.
type CommandResult struct {
	ID     string `json:"id,omitempty"`
	Topic  string `json:"topic"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Reply is where a command result goes; Topic empty means no result.
type Reply struct {
	Topic       string
	Correlation []byte
}

// ReplyPublisher publishes a command result (MQTT 5 properties included).
type ReplyPublisher func(r Reply, payload []byte)

// Command validates a write synchronously and executes it asynchronously.
// The returned verdict decides the PUBACK; Accept means the command was
// submitted (or was a duplicate whose earlier result was re-sent).
func (s *Service) Command(clientID, topic string, payload []byte, retain bool, reply Reply, send ReplyPublisher) (Verdict, string) {
	t, err := s.opts.Names.Parse(topic)
	if err != nil {
		return Invalid, err.Error()
	}
	if !t.Command {
		return Denied, "native value topics are broker-owned; publish to .../set"
	}
	if retain {
		return Retained, "retained commands are rejected"
	}
	cmd, err := ParseCommand(payload)
	if err != nil {
		return BadPayload, err.Error()
	}
	if reply.Topic == "" && cmd.ReplyTo != "" {
		reply.Topic = cmd.ReplyTo
	}
	sys, res, v, why := s.resolveTarget(t)
	if v != Accept {
		return v, why
	}
	val, err := ConvertValue(cmd.Value, oahost.Kind(res.ElemType))
	if err != nil {
		return BadPayload, err.Error()
	}
	if cmd.ID != "" {
		dk := clientID + "\x00" + cmd.ID
		s.mu.Lock()
		rec, seen := s.cmds[dk]
		if !seen {
			s.cmds[dk] = cmdRecord{at: time.Now()}
		}
		s.mu.Unlock()
		if seen {
			s.dups.Add(1)
			if rec.result != nil && reply.Topic != "" && send != nil {
				send(reply, rec.result)
			}
			return Accept, "duplicate"
		}
	}
	s.commands.Add(1)
	addr := t.WriteAddress(sys)
	readKey := t.Key(sys)
	go s.execute(clientID, topic, addr, readKey, val, cmd, reply, send)
	return Accept, ""
}

func (s *Service) execute(clientID, topic, addr, readKey string, val oahost.Value, cmd Command, reply Reply, send ReplyPublisher) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.WriteTimeout)
	defer cancel()
	err := s.api.DpSet(ctx, []string{addr}, []oahost.Value{val}, s.opts.WriteTimeout)
	res := CommandResult{ID: cmd.ID, Topic: topic, Status: "confirmed"}
	switch {
	case err == nil:
		if s.opts.NoSource && strings.HasSuffix(readKey, ":"+DefaultAttr) {
			s.apply(readKey, val, time.Now(), false)
		}
	case errors.Is(err, oahost.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		// The host drops a request whose deadline passed before execution,
		// but a timeout after submission cannot prove it did not execute.
		res.Status = "timeout"
		res.Error = "no OA confirmation within deadline; outcome unknown"
		s.cmdErrs.Add(1)
	default:
		res.Status = "failed"
		res.Error = err.Error()
		s.cmdErrs.Add(1)
	}
	out, _ := json.Marshal(res)
	if cmd.ID != "" {
		dk := clientID + "\x00" + cmd.ID
		s.mu.Lock()
		s.cmds[dk] = cmdRecord{at: time.Now(), result: out}
		s.mu.Unlock()
	}
	if reply.Topic != "" && send != nil {
		send(reply, out)
	}
	if err != nil {
		s.logger.Info("native write not confirmed", "client", clientID, "topic", topic, "status", res.Status, "err", err)
	}
}

func (s *Service) pruneCommands() {
	cutoff := time.Now().Add(-10 * time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range s.cmds {
		if r.at.Before(cutoff) {
			delete(s.cmds, k)
		}
	}
	if len(s.cmds) > 10000 {
		type kv struct {
			k  string
			at time.Time
		}
		all := make([]kv, 0, len(s.cmds))
		for k, r := range s.cmds {
			all = append(all, kv{k, r.at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for _, e := range all[:len(all)-10000] {
			delete(s.cmds, e.k)
		}
	}
}

// PublishStatus publishes the retained broker status on both status topics.
func (s *Service) PublishStatus() {
	st := map[string]any{
		"nodeId":    s.opts.NodeID,
		"system":    s.localSystem,
		"oa":        map[bool]string{true: "connected", false: "disconnected"}[s.oaUp.Load()],
		"ready":     s.ready.Load() && s.oaUp.Load(),
		"role":      "STANDALONE",
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	b, _ := json.Marshal(st)
	if s.localSystem != "" {
		for _, topic := range s.statusTopics() {
			_ = s.broker.Publish(topic, b, true, 1)
		}
	}
}

// statusTopics are this broker's status topics: <root>/<systems>/<local
// system>, and the root itself with the local shortcut.
func (s *Service) statusTopics() []string {
	topics := []string{s.opts.Names.StatusTopic(s.localSystem)}
	if !s.opts.Names.NoShortcut {
		topics = append(topics, s.opts.Names.StatusTopic(""))
	}
	return topics
}

// sysOf binds a parsed system to a WinCC OA system name ("" is the local
// shortcut).
func (s *Service) sysOf(system string) string {
	if system == "" {
		return s.localSystem
	}
	return system
}

// Names returns the topic levels the service uses, with the local system
// once it is known.
func (s *Service) Names() Names {
	n := s.opts.Names
	n.Local = s.localSystem
	return n
}

// StaleStatusClear reports whether a publish removes a retained status
// this broker does not own: a retained empty payload on the status topic
// of a system other than the local one (e.g. left behind after the
// project's system name changed).
func (s *Service) StaleStatusClear(topic string, retain bool, payload []byte) bool {
	if !retain || len(payload) != 0 || s.opts.Names.Classify(topic) != KindStatus {
		return false
	}
	for _, own := range s.statusTopics() {
		if topic == own {
			return false
		}
	}
	return true
}

func (s *Service) Stats() Stats {
	s.mu.Lock()
	st := Stats{Interests: len(s.subs), DPEs: len(s.dpes), Batches: len(s.batches), WildQueries: len(s.wild), WildSubs: len(s.wildSubs), TopicDPs: len(s.topics), TopicSubs: len(s.tsubs), TopicWilds: len(s.twild), TopicDirs: len(s.tdirs)}
	s.mu.Unlock()
	st.Connects = s.connects.Load()
	st.Disconnects = s.disconnects.Load()
	st.ConnectErrors = s.connectErrs.Load()
	st.Published = s.published.Load()
	st.Commands = s.commands.Load()
	st.CommandErrors = s.cmdErrs.Load()
	st.Duplicates = s.dups.Load()
	return st
}
