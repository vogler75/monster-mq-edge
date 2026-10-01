package winccoanative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"monstermq.io/edge/internal/oahost"
)

// TopicType is the datapoint type of the topics branch: one datapoint per
// MQTT topic and system, named by TopicDP or TopicDPName, so every system
// that runs a broker can replicate topics through WinCC OA.
const TopicType = "MMQTopic"

// Elements of MMQTopic. value carries non-retained publishes and has the
// last value storage turned off; retained carries the retained message and
// survives a restart.
const (
	topicElTopic    = "topic"
	topicElValue    = "value"
	topicElRetained = "retained"
)

var topicElements = []string{topicElTopic, topicElValue, topicElRetained}

func topicElementKinds() []uint32 {
	return []uint32{uint32(oahost.KindString), uint32(oahost.KindBytes), uint32(oahost.KindBytes)}
}

// lastValueStorageOff is the config attribute that keeps the value element
// out of the last value database.
const lastValueStorageOff = "_original.._last_value_storage_off"

// TopicDP is the hashed datapoint name of an MQTT topic below the topics
// branch: MMQTopic_k<24 hex digits of SHA-256>.
func TopicDP(topic string) string {
	sum := sha256.Sum256([]byte("topic\x00" + topic))
	return TopicType + "_k" + hex.EncodeToString(sum[:])[:24]
}

// maxTopicDPName bounds a readable datapoint name; longer topics get the
// hashed name.
const maxTopicDPName = 128

// TopicDPName is the readable datapoint name of a topic: MMQTopic_ and the
// topic itself, e.g. plant/line-1/temp -> MMQTopic_plant/line-1/temp. The
// characters WinCC OA forbids in datapoint names (blank . : , ; * ? [ ] { }
// $ @, control characters), the quotes " ' \ and the escape character %
// are written as %XX (uppercase hex), so different topics never get the
// same name. Topics whose name would exceed maxTopicDPName characters get
// the hashed name (TopicDP).
func TopicDPName(topic string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.WriteString(TopicType + "_")
	for i := 0; i < len(topic); i++ {
		c := topic[i]
		if c < 0x20 || c == 0x7F || strings.IndexByte(" .:,;*?[]{}$@%\"'\\", c) >= 0 {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		} else {
			b.WriteByte(c)
		}
		if b.Len() > maxTopicDPName {
			return TopicDP(topic)
		}
	}
	return b.String()
}

// topicDP is the datapoint name of a topic in the configured naming.
func (s *Service) topicDP(topic string) string {
	if s.opts.TopicDPNames {
		return TopicDPName(topic)
	}
	return TopicDP(topic)
}

// ErrTopicCollision reports a datapoint that holds another topic.
var ErrTopicCollision = errors.New("topic datapoint belongs to another topic")

// TopicTarget is a parsed topic of the topics branch.
type TopicTarget struct {
	System string // WinCC OA system name; empty for the local shortcut
	Topic  string // MQTT topic below <topics>/, kept verbatim

	names Names
}

// ParseTopic parses an exact topic (no wildcards) of the topics branch.
func (n Names) ParseTopic(topic string) (TopicTarget, error) {
	if HasWildcard(topic) {
		return TopicTarget{names: n}, fmt.Errorf("%w: wildcard", ErrMalformed)
	}
	return n.ParseTopicFilter(topic)
}

// ParseTopicFilter parses a topic or filter of the topics branch; with
// wildcards, Topic is the filter below <topics>/.
func (n Names) ParseTopicFilter(topic string) (TopicTarget, error) {
	t := TopicTarget{names: n}
	if n.Classify(topic) != KindTopics {
		return t, fmt.Errorf("%w: not a topics topic", ErrMalformed)
	}
	sys, segs, err := n.scope(topic)
	if err != nil {
		return t, err
	}
	if len(segs) == 0 || segs[0] != n.Topics {
		return t, fmt.Errorf("%w: expected %s", ErrMalformed, n.Topics)
	}
	t.System = sys
	t.Topic = strings.Join(segs[1:], "/")
	if len(segs) < 2 || t.Topic == "" {
		return t, fmt.Errorf("%w: missing topic below %s", ErrMalformed, n.Topics)
	}
	return t, nil
}

// MQTTTopic renders the full topic of t; with a system set it is the
// explicit <root>/<systems>/<system>/<topics>/... form.
func (t TopicTarget) MQTTTopic() string {
	n := t.names.WithDefaults()
	if t.System == "" {
		return n.Root + "/" + n.Topics + "/" + t.Topic
	}
	return n.Root + "/" + n.Systems + "/" + encodeSegment(t.System, false) + "/" + n.Topics + "/" + t.Topic
}

// topicSub is one subscription of a client filter on a topic datapoint.
type topicSub struct {
	key     string // topicEntry key
	topic   string // subscribed topic (alias form as subscribed)
	waiting bool   // retained value not yet delivered
}

// topicEntry is the connection of one MMQTopic datapoint, shared by every
// subscription on it.
type topicEntry struct {
	key      string // Sys:MMQTopic_...
	system   string
	topic    string // topic below topics/ the datapoint must hold
	foreign  bool   // its topic element holds another topic: nothing is delivered
	subs     map[subKey]*topicSub
	ref      uint64
	live     bool // connected
	inflight bool // connect in progress
	due      bool // a connect attempt is wanted
	known    bool // the retained value is known (answer received)
	retained []byte
}

func (e *topicEntry) topicAddr() string    { return e.key + "." + topicElTopic + ":" + DefaultAttr }
func (e *topicEntry) valueAddr() string    { return e.key + "." + topicElValue + ":" + DefaultAttr }
func (e *topicEntry) retainedAddr() string { return e.key + "." + topicElRetained + ":" + DefaultAttr }

// topicWild is one wildcard subscription of the topics branch. It is served
// by the topic directory of its system: every datapoint whose topic matches
// gets the subscription like an exact one, under the concrete topic.
type topicWild struct {
	system   string      // bound system
	target   TopicTarget // System as subscribed ("" for the shortcut), Topic = filter below topics/
	restored bool
}

// topicDir is the directory of the MMQTopic datapoints of one system: a
// dpQueryConnectSingle on their topic element, which also reports
// datapoints created later. It exists while the system has wildcard
// subscriptions.
type topicDir struct {
	system   string
	ref      uint64
	live     bool
	inflight bool
	due      bool
	tree     *topicTree            // topic -> Sys:MMQTopic_k...
	wilds    map[subKey]*topicWild // wildcard subscriptions on this system
}

func (d *topicDir) query(local string) string {
	q := "SELECT '" + DefaultAttr + "' FROM '" + TopicType + "_*." + topicElTopic + "' WHERE _DPT = \"" + TopicType + "\""
	if d.system != local {
		q += " REMOTE '" + d.system + "'"
	}
	return q
}

// topicDelivery is a retained message for one subscription.
type topicDelivery struct {
	client, filter, topic string
	payload               []byte
}

// ensureTopicType creates the MMQTopic type on the local system when it is
// missing. Remote systems must have it already: their broker creates it.
func (s *Service) ensureTopicType(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.api.EnsureType(ctx, TopicType, topicElements, topicElementKinds()); err != nil {
		return fmt.Errorf("datapoint type %s: %w (an existing type with a different layout is not changed)", TopicType, err)
	}
	return nil
}

// validateTopic decides a SUBSCRIBE filter in the topics branch.
func (s *Service) validateTopic(f string) (Verdict, string) {
	t, err := s.opts.Names.ParseTopicFilter(f)
	if err != nil {
		return Invalid, err.Error()
	}
	if !s.ready.Load() || !s.oaUp.Load() {
		return Unavailable, "WinCC OA not ready"
	}
	if sys := s.sysOf(t.System); sys != s.localSystem {
		if _, err := s.lookup(sys + ":MMQ_system_probe."); err != nil {
			return Unavailable, err.Error()
		}
	}
	return Accept, ""
}

// subscribedTopic registers an accepted topics filter. The datapoint may
// not exist yet: the interest waits until it is created.
func (s *Service) subscribedTopic(clientID, filter string, existed, restored bool) {
	t, err := s.opts.Names.ParseTopic(filter)
	if err != nil {
		return
	}
	key := s.sysOf(t.System) + ":" + s.topicDP(t.Topic)
	sk := subKey{clientID, filter}
	s.mu.Lock()
	if old, ok := s.tsubs[sk]; ok && old.key == key {
		e := s.topics[key]
		var payload []byte
		if e != nil && e.known && len(e.retained) > 0 {
			payload = e.retained
		}
		s.mu.Unlock()
		if payload != nil && !restored {
			_ = s.broker.PublishCurrentValue(clientID, filter, filter, payload, true)
		}
		return
	}
	e := s.topics[key]
	if e == nil {
		e = &topicEntry{key: key, system: s.sysOf(t.System), topic: t.Topic, subs: map[subKey]*topicSub{}}
		s.topics[key] = e
	}
	sub := &topicSub{key: key, topic: filter, waiting: !restored}
	s.tsubs[sk] = sub
	e.subs[sk] = sub
	var payload []byte
	if e.known && sub.waiting {
		sub.waiting = false
		payload = e.retained
	}
	if !e.live && !e.inflight {
		e.due = true
	}
	s.mu.Unlock()
	if len(payload) > 0 {
		_ = s.broker.PublishCurrentValue(clientID, filter, filter, payload, existed)
	}
	s.wake()
}

// subscribedTopicWild registers an accepted wildcard filter of the topics
// branch and attaches it to every known matching datapoint.
func (s *Service) subscribedTopicWild(clientID, filter string, existed, restored bool) {
	t, err := s.opts.Names.ParseTopicFilter(filter)
	if err != nil {
		return
	}
	sk := subKey{clientID, filter}
	sys := s.sysOf(t.System)
	var initial []topicDelivery
	s.mu.Lock()
	if _, ok := s.twild[sk]; ok {
		// Repeated SUBSCRIBE: the retained messages again.
		for _, e := range s.topics {
			if sub := e.subs[sk]; sub != nil && e.known && len(e.retained) > 0 {
				initial = append(initial, topicDelivery{clientID, filter, sub.topic, e.retained})
			}
		}
		s.mu.Unlock()
		if !restored {
			for _, d := range initial {
				_ = s.broker.PublishCurrentValue(d.client, d.filter, d.topic, d.payload, true)
			}
		}
		return
	}
	w := &topicWild{system: sys, target: t, restored: restored}
	s.twild[sk] = w
	d := s.tdirs[sys]
	if d == nil {
		d = &topicDir{system: sys, due: true, tree: newTopicTree(), wilds: map[subKey]*topicWild{}}
		s.tdirs[sys] = d
	}
	d.wilds[sk] = w
	d.tree.Match(t.Topic, func(topic, sysDP string) {
		if dl := s.attachWildLocked(sk, w, sysDP, topic, !restored); dl != nil {
			initial = append(initial, *dl)
		}
	})
	s.mu.Unlock()
	for _, dl := range initial {
		_ = s.broker.PublishCurrentValue(dl.client, dl.filter, dl.topic, dl.payload, existed)
	}
	s.wake()
}

// attachWildLocked adds a wildcard subscription to the datapoint of a
// matching topic. It returns the retained message when it is already known
// and the subscription waits for it (caller holds s.mu).
func (s *Service) attachWildLocked(sk subKey, w *topicWild, sysDP, topic string, waiting bool) *topicDelivery {
	e := s.topics[sysDP]
	if e == nil {
		e = &topicEntry{key: sysDP, system: w.system, topic: topic, subs: map[subKey]*topicSub{}}
		s.topics[sysDP] = e
	}
	if _, ok := e.subs[sk]; ok {
		return nil
	}
	concrete := TopicTarget{System: w.target.System, Topic: topic, names: s.opts.Names}.MQTTTopic()
	sub := &topicSub{key: sysDP, topic: concrete, waiting: waiting}
	e.subs[sk] = sub
	if !e.live && !e.inflight {
		e.due = true
	}
	if e.known && sub.waiting {
		sub.waiting = false
		if len(e.retained) > 0 {
			return &topicDelivery{sk.client, sk.filter, concrete, e.retained}
		}
	}
	return nil
}

// unsubscribedTopicWild removes a wildcard subscription of the topics
// branch. It returns the datapoint connections and the directory query
// that are no longer needed (caller holds s.mu).
func (s *Service) unsubscribedTopicWild(sk subKey) (refs []uint64, dirRef uint64) {
	w, ok := s.twild[sk]
	if !ok {
		return nil, 0
	}
	delete(s.twild, sk)
	for key, e := range s.topics {
		if _, ok := e.subs[sk]; !ok {
			continue
		}
		delete(e.subs, sk)
		if len(e.subs) == 0 {
			delete(s.topics, key)
			if e.live {
				refs = append(refs, e.ref)
			}
		}
	}
	if d := s.tdirs[w.system]; d != nil {
		delete(d.wilds, sk)
		if len(d.wilds) > 0 {
			return refs, 0
		}
		delete(s.tdirs, w.system)
		if d.live {
			dirRef = d.ref
		}
	}
	return refs, dirRef
}

// registerDir starts the directory query of a system.
func (s *Service) registerDir(d *topicDir) {
	ref, err := s.api.QueryConnect(s.ctx, d.query(s.localSystem), true, func(m oahost.Message) { s.onDirRows(d, m) }, s.opts.ConnectTimeout)
	s.mu.Lock()
	d.inflight = false
	alive := s.tdirs[d.system] == d
	if err == nil && alive {
		d.ref = ref
		d.live = true
	}
	s.mu.Unlock()
	switch {
	case err != nil && errors.Is(err, oahost.ErrUnavailable):
	case err != nil:
		s.logger.Warn("topic directory query failed", "system", d.system, "err", err)
	case !alive:
		s.disconnectQueryAsync([]uint64{ref})
	}
}

// onDirRows adds the datapoints of a directory answer or hotlink and
// attaches the matching wildcard subscriptions. A datapoint found by a
// hotlink was created after the subscription: its retained message is
// delivered like the initial one.
func (s *Service) onDirRows(d *topicDir, m oahost.Message) {
	flags, _ := m.U32(oahost.TagFlags)
	answer := flags&oahost.FlagAnswer != 0
	rows, err := oahost.QueryRows(m)
	if err != nil || len(rows) < 1 {
		return
	}
	var initial []topicDelivery
	s.mu.Lock()
	if s.tdirs[d.system] != d {
		s.mu.Unlock()
		return
	}
	for _, row := range rows[1:] {
		if len(row) < 2 || row[0].Kind != oahost.KindString || row[1].Kind != oahost.KindString {
			continue
		}
		sys, rest, ok := strings.Cut(row[0].Str, ":")
		dp, _, _ := strings.Cut(rest, ".")
		topic := row[1].Str
		if !ok || topic == "" || s.topicDP(topic) != dp {
			continue // not configured yet, or not a topic datapoint of this broker kind
		}
		sysDP := sys + ":" + dp
		d.tree.Put(topic, sysDP)
		initial = append(initial, s.attachMatchingLocked(d, sysDP, topic, answer)...)
	}
	s.mu.Unlock()
	for _, dl := range initial {
		_ = s.broker.PublishCurrentValue(dl.client, dl.filter, dl.topic, dl.payload, false)
	}
	s.wake()
}

// attachMatchingLocked attaches every wildcard subscription of a system
// that matches topic (caller holds s.mu).
func (s *Service) attachMatchingLocked(d *topicDir, sysDP, topic string, answer bool) []topicDelivery {
	var out []topicDelivery
	for sk, w := range d.wilds {
		if !TopicMatches(w.target.Topic, topic) {
			continue
		}
		if dl := s.attachWildLocked(sk, w, sysDP, topic, !(w.restored && answer)); dl != nil {
			out = append(out, *dl)
		}
	}
	return out
}

// topicCreatedHere connects the subscriptions of a datapoint this broker
// just created before the first value is written, so its first message is
// not lost.
func (s *Service) topicCreatedHere(sysDP, topic string) {
	sys, _, _ := strings.Cut(sysDP, ":")
	var initial []topicDelivery
	s.mu.Lock()
	if d := s.tdirs[sys]; d != nil {
		d.tree.Put(topic, sysDP)
		initial = s.attachMatchingLocked(d, sysDP, topic, false)
	}
	e := s.topics[sysDP]
	run := e != nil && !e.live && !e.inflight && len(e.subs) > 0
	if run {
		e.due = false
		e.inflight = true
	}
	s.mu.Unlock()
	for _, dl := range initial {
		_ = s.broker.PublishCurrentValue(dl.client, dl.filter, dl.topic, dl.payload, false)
	}
	if run {
		s.connectTopic(e)
	}
}

func (s *Service) disconnectQueryAsync(refs []uint64) {
	if len(refs) == 0 {
		return
	}
	go func() {
		for _, ref := range refs {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.ConnectTimeout)
			if err := s.api.QueryDisconnect(ctx, ref); err != nil {
				s.logger.Warn("topic directory disconnect failed", "ref", ref, "err", err)
			}
			cancel()
		}
	}()
}

// unsubscribedTopic removes one topics subscription; the last one
// disconnects the datapoint.
func (s *Service) unsubscribedTopic(sk subKey) (uint64, bool) {
	sub, ok := s.tsubs[sk]
	if !ok {
		return 0, false
	}
	delete(s.tsubs, sk)
	e := s.topics[sub.key]
	if e == nil {
		return 0, false
	}
	delete(e.subs, sk)
	if len(e.subs) > 0 {
		return 0, false
	}
	delete(s.topics, e.key)
	if e.live {
		return e.ref, true
	}
	return 0, false
}

// connectTopics connects the datapoints of topics interests that want a
// connect attempt. A datapoint that does not exist yet stays waiting for
// its creation (reported by the host) or the next reconcile.
func (s *Service) connectTopics() {
	s.mu.Lock()
	var dirs []*topicDir
	for _, d := range s.tdirs {
		if d.due && !d.live && !d.inflight {
			d.due = false
			d.inflight = true
			dirs = append(dirs, d)
		}
	}
	s.mu.Unlock()
	for _, d := range dirs {
		if s.ctx.Err() != nil {
			return
		}
		s.registerDir(d)
	}
	s.mu.Lock()
	var todo []*topicEntry
	for _, e := range s.topics {
		if e.due && !e.live && !e.inflight && len(e.subs) > 0 {
			e.due = false
			e.inflight = true
			todo = append(todo, e)
		}
	}
	s.mu.Unlock()
	sort.Slice(todo, func(i, j int) bool { return todo[i].key < todo[j].key })
	for _, e := range todo {
		if s.ctx.Err() != nil {
			return
		}
		s.connectTopic(e)
	}
}

func (s *Service) connectTopic(e *topicEntry) {
	// The topic element comes first: its answer tells whether the
	// datapoint holds this topic before any value is delivered.
	names := []string{e.topicAddr(), e.valueAddr(), e.retainedAddr()}
	// Never NoSource: the hotlink of the own write is how local
	// subscribers get the message.
	ref, err := s.api.DpConnect(s.ctx, names, oahost.FlagAnswer, func(m oahost.Message) { s.onTopicHotlink(e, m) }, s.opts.ConnectTimeout)
	s.mu.Lock()
	e.inflight = false
	alive := s.topics[e.key] == e && len(e.subs) > 0
	if err == nil && alive {
		e.ref = ref
		e.live = true
	}
	s.mu.Unlock()
	switch {
	case err != nil && (errors.Is(err, oahost.ErrNotFound) || errors.Is(err, oahost.ErrUnavailable)):
		// Not created yet, or its system is not connected.
	case err != nil:
		s.connectErrs.Add(1)
		s.logger.Warn("topic dpConnect failed", "dp", e.key, "err", err)
	case !alive:
		s.disconnect(ref)
	default:
		s.connects.Add(1)
	}
}

// onTopicHotlink routes the events of one topic datapoint. The answer of
// the retained element is the retained message for waiting subscribers;
// the answer of the value element is never delivered (no last value).
// Every hotlink is published to the subscribed topics, retain flag unset.
func (s *Service) onTopicHotlink(e *topicEntry, m oahost.Message) {
	flags, _ := m.U32(oahost.TagFlags)
	answer := flags&oahost.FlagAnswer != 0
	names, values, err := oahost.HotlinkItems(m)
	if err != nil {
		s.logger.Warn("topic hotlink decode failed", "err", err)
		return
	}
	for i, name := range names {
		if name == e.topicAddr() {
			s.checkTopicOwner(e, values[i].Str)
			continue
		}
		retained := name == e.retainedAddr()
		if !retained && name != e.valueAddr() {
			continue
		}
		payload := values[i].Bytes
		var initial []delivery
		var topics []string
		s.mu.Lock()
		if s.topics[e.key] != e {
			s.mu.Unlock()
			return
		}
		if e.foreign {
			s.mu.Unlock()
			continue
		}
		if retained {
			e.retained = append([]byte(nil), payload...)
			e.known = true
		}
		for sk, sub := range e.subs {
			switch {
			case retained && answer:
				if sub.waiting {
					sub.waiting = false
					if len(payload) > 0 {
						initial = append(initial, delivery{sk.client, sk.filter, sub.topic})
					}
				}
			case !answer:
				topics = append(topics, sub.topic)
			}
		}
		s.mu.Unlock()
		for _, d := range initial {
			_ = s.broker.PublishCurrentValue(d.client, d.filter, d.topic, payload, false)
		}
		sort.Strings(topics)
		for j, t := range topics {
			if j > 0 && topics[j-1] == t {
				continue
			}
			if err := s.broker.Publish(t, payload, false, 1); err != nil {
				s.logger.Warn("topic publish failed", "topic", t, "err", err)
				continue
			}
			s.published.Add(1)
		}
	}
}

// checkTopicOwner marks a datapoint whose topic element holds another
// topic (a name collision); its values are never delivered. An empty topic
// element is a datapoint that is still being configured.
func (s *Service) checkTopicOwner(e *topicEntry, topic string) {
	if topic == "" {
		return
	}
	s.mu.Lock()
	was := e.foreign
	e.foreign = topic != e.topic
	now := e.foreign
	s.mu.Unlock()
	if now && !was {
		s.logger.Warn("topic datapoint holds another topic; not delivered", "dp", e.key, "topic", e.topic, "holds", topic)
	}
}

// topicsOnSystemLocked drops the connections and the directory query of a
// lost system and registers them again when it returns (caller holds s.mu).
func (s *Service) topicsOnSystemLocked(system string, available bool) (lost, lostDirs []uint64) {
	if d := s.tdirs[system]; d != nil {
		if d.live {
			lostDirs = append(lostDirs, d.ref)
			d.live = false
		}
		if available {
			d.due = true
		}
	}
	for dp := range s.prepared {
		if strings.HasPrefix(dp, system+":") {
			delete(s.prepared, dp)
		}
	}
	for _, e := range s.topics {
		if e.system != system {
			continue
		}
		if e.live {
			lost = append(lost, e.ref)
			e.live = false
		}
		e.known = false
		e.foreign = false
		e.retained = nil
		for _, sub := range e.subs {
			sub.waiting = true
		}
		if available {
			e.due = true
		}
	}
	return lost, lostDirs
}

// invalidateTopicLocked handles a deleted topic datapoint: its interests
// wait for the datapoint to be created again (caller holds s.mu).
func (s *Service) invalidateTopicLocked(sysDP string) (uint64, bool) {
	delete(s.prepared, sysDP)
	e := s.topics[sysDP]
	if e == nil {
		return 0, false
	}
	e.known = false
	e.foreign = false
	e.retained = nil
	for _, sub := range e.subs {
		sub.waiting = true
	}
	if e.live {
		e.live = false
		return e.ref, true
	}
	return 0, false
}

// topicCreated is told that a topic datapoint was created (by this or
// another broker) so waiting interests connect now.
func (s *Service) topicCreated(sysDP string) {
	s.mu.Lock()
	e := s.topics[sysDP]
	if e != nil && !e.live && !e.inflight {
		e.due = true
	}
	s.mu.Unlock()
	if e != nil {
		s.wake()
	}
}

// PublishTopic writes an MQTT publish of the topics branch to its datapoint
// and returns when WinCC OA confirmed it. A non-retained publish goes to the
// value element, a retained one to the retained element; a retained publish
// with an empty payload clears the retained element and deletes the
// datapoint. Subscribers get the message through the datapoint connection
// only, on every system.
func (s *Service) PublishTopic(topic string, payload []byte, retain bool) (Verdict, string) {
	t, err := s.opts.Names.ParseTopic(topic)
	if err != nil {
		return Invalid, err.Error()
	}
	if !s.ready.Load() || !s.oaUp.Load() {
		return Unavailable, "WinCC OA not ready"
	}
	sysDP := s.sysOf(t.System) + ":" + s.topicDP(t.Topic)
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.WriteTimeout)
	defer cancel()
	if retain && len(payload) == 0 {
		return s.deleteTopic(ctx, sysDP, t.Topic)
	}
	el := topicElValue
	if retain {
		el = topicElRetained
	}
	addr := sysDP + "." + el + ":" + WriteAttr
	val := oahost.Value{Kind: oahost.KindBytes, Bytes: payload}
	for attempt := 0; ; attempt++ {
		if err := s.prepareTopic(ctx, sysDP, t.Topic); err != nil {
			return topicVerdict(err)
		}
		err := s.api.DpSet(ctx, []string{addr}, []oahost.Value{val}, s.opts.WriteTimeout)
		if err == nil {
			return Accept, ""
		}
		if errors.Is(err, oahost.ErrNotFound) && attempt == 0 {
			// Deleted by someone else since it was prepared.
			s.mu.Lock()
			delete(s.prepared, sysDP)
			s.mu.Unlock()
			continue
		}
		return topicVerdict(err)
	}
}

func topicVerdict(err error) (Verdict, string) {
	if errors.Is(err, oahost.ErrInvalid) || errors.Is(err, oahost.ErrType) || errors.Is(err, ErrTopicCollision) {
		return Invalid, err.Error()
	}
	return Unavailable, err.Error()
}

// prepareTopic makes sure the datapoint of a topic exists with its topic
// element set and the last value storage of the value element turned off.
// It runs once per datapoint and broker lifetime; later publishes only
// write the value.
func (s *Service) prepareTopic(ctx context.Context, sysDP, topic string) error {
	s.mu.Lock()
	done := s.prepared[sysDP]
	s.mu.Unlock()
	if done {
		return nil
	}
	s.prepMu.Lock()
	defer s.prepMu.Unlock()
	s.mu.Lock()
	done = s.prepared[sysDP]
	s.mu.Unlock()
	if done {
		return nil
	}
	res, err := s.api.Resolve(ctx, sysDP+"."+topicElTopic)
	if err != nil {
		return err
	}
	created := false
	if !res.Exists {
		if err := s.api.DpCreate(ctx, sysDP, TopicType, s.opts.WriteTimeout); err != nil && !strings.Contains(err.Error(), "exists") {
			return fmt.Errorf("create %s for %s: %w", sysDP, topic, err)
		}
		created = true
	} else if res.TypeName != "" && res.TypeName != TopicType {
		return fmt.Errorf("%w: %s has type %s, not %s", oahost.ErrInvalid, sysDP, res.TypeName, TopicType)
	} else if err := s.checkTopicElement(ctx, sysDP, topic); err != nil {
		return err
	}
	err = s.api.DpSet(ctx,
		[]string{sysDP + "." + topicElTopic + ":" + WriteAttr, sysDP + "." + topicElValue + ":" + lastValueStorageOff},
		[]oahost.Value{{Kind: oahost.KindString, Str: topic}, {Kind: oahost.KindBool, Bool: true}},
		s.opts.WriteTimeout)
	if err != nil {
		return fmt.Errorf("configure %s for %s: %w", sysDP, topic, err)
	}
	s.mu.Lock()
	s.prepared[sysDP] = true
	s.mu.Unlock()
	if created {
		s.topicCreatedHere(sysDP, topic)
	}
	return nil
}

// deleteTopic clears the retained element, so connected subscribers get
// the empty retained message, then deletes the datapoint.
func (s *Service) deleteTopic(ctx context.Context, sysDP, topic string) (Verdict, string) {
	s.prepMu.Lock()
	defer s.prepMu.Unlock()
	s.mu.Lock()
	delete(s.prepared, sysDP)
	s.mu.Unlock()
	if err := s.checkTopicElement(ctx, sysDP, topic); errors.Is(err, oahost.ErrNotFound) {
		return Accept, ""
	} else if err != nil {
		return topicVerdict(err)
	}
	err := s.api.DpSet(ctx, []string{sysDP + "." + topicElRetained + ":" + WriteAttr},
		[]oahost.Value{{Kind: oahost.KindBytes}}, s.opts.WriteTimeout)
	if errors.Is(err, oahost.ErrNotFound) {
		return Accept, ""
	}
	if err != nil {
		return topicVerdict(err)
	}
	if err := s.api.DpDelete(ctx, sysDP, s.opts.WriteTimeout); err != nil && !errors.Is(err, oahost.ErrNotFound) {
		return topicVerdict(err)
	}
	return Accept, ""
}

// checkTopicElement makes sure an existing datapoint holds topic (or none
// yet), so a name collision never overwrites or deletes another topic.
func (s *Service) checkTopicElement(ctx context.Context, sysDP, topic string) error {
	vals, err := s.api.DpGet(ctx, []string{sysDP + "." + topicElTopic + ":" + DefaultAttr}, s.opts.WriteTimeout)
	if err != nil {
		return err
	}
	if have := vals[0].Str; have != "" && have != topic {
		return fmt.Errorf("%w: %s holds %q, not %q", ErrTopicCollision, sysDP, have, topic)
	}
	return nil
}
