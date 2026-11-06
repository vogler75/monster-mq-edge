package kafka

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"

	"monstermq.io/edge/internal/kafka/wire"
	"monstermq.io/edge/internal/stores"
)

const (
	fetchRowLimit    = 100
	defaultProducer  = "kafka-producer"
	defaultGroup     = "default-group"
	clusterID        = "monstermq-cluster"
	storeCallTimeout = 30 * time.Second
)

// req is the decoding/encoding context of one request.
type req struct {
	c        *conn
	v        int16
	r        *wire.Reader
	w        *wire.Writer
	clientID string
}

func (h *req) throttle(minVersion int16) {
	if h.v >= minVersion {
		h.w.Int32(0)
	}
}

func (h *req) storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(h.c.ctx, storeCallTimeout)
}

// ---- Metadata (3) ----

func (h *req) metadata() {
	n := h.r.ArrayLen(2)
	var requested []string
	for i := 0; i < n; i++ {
		requested = append(requested, h.r.String())
	}
	if h.v >= 4 {
		h.r.Int8() // allow_auto_topic_creation
	}
	s := h.c.s
	h.throttle(3)
	h.w.Int32(1) // one broker
	h.w.Int32(0)
	h.w.String(s.advertisedHost)
	h.w.Int32(int32(s.advertisedPort))
	if h.v >= 1 {
		h.w.NullString(nil) // rack
	}
	if h.v >= 2 {
		h.w.String(clusterID)
	}
	if h.v >= 1 {
		h.w.Int32(0) // controller
	}
	topics := requested
	if len(topics) == 0 {
		for _, st := range s.streams {
			topics = append(topics, st.cfg.StreamName)
		}
	}
	h.w.Int32(int32(len(topics)))
	for _, t := range topics {
		h.w.Int16(wire.ErrNone)
		h.w.String(t)
		if h.v >= 1 {
			h.w.Int8(0) // is_internal
		}
		h.w.Int32(1) // one partition
		h.w.Int16(wire.ErrNone)
		h.w.Int32(0) // partition
		h.w.Int32(0) // leader
		h.w.Int32(1) // replicas
		h.w.Int32(0)
		h.w.Int32(1) // isr
		h.w.Int32(0)
	}
}

// ---- DescribeConfigs (32) ----

type configEntry struct {
	name, value string
	readOnly    bool
}

func (h *req) describeConfigs() {
	type resource struct {
		typ   int8
		name  string
		names map[string]bool
	}
	n := h.r.ArrayLen(7)
	res := make([]resource, 0, n)
	for i := 0; i < n; i++ {
		rs := resource{typ: h.r.Int8(), name: h.r.String()}
		cn := h.r.ArrayLen(2)
		if cn > 0 {
			rs.names = map[string]bool{}
			for j := 0; j < cn; j++ {
				rs.names[h.r.String()] = true
			}
		}
		res = append(res, rs)
	}
	if h.v >= 1 {
		h.r.Int8() // include_synonyms
	}
	s := h.c.s
	h.w.Int32(0) // throttle
	h.w.Int32(int32(len(res)))
	for _, rs := range res {
		var entries []configEntry
		switch rs.typ {
		case 2: // topic
			entries = []configEntry{
				{"cleanup.policy", "delete", false},
				{"retention.ms", "604800000", false},
				{"segment.bytes", "1073741824", false},
			}
		case 4: // broker
			entries = []configEntry{
				{"zookeeper.connect", "", true},
				{"advertised.listeners", "PLAINTEXT://" + s.advertisedHost + ":" + strconv.Itoa(s.advertisedPort), true},
			}
		}
		h.w.Int16(wire.ErrNone)
		h.w.NullString(nil)
		h.w.Int8(rs.typ)
		h.w.String(rs.name)
		var out []configEntry
		for _, e := range entries {
			if rs.names == nil || rs.names[e.name] {
				out = append(out, e)
			}
		}
		h.w.Int32(int32(len(out)))
		for _, e := range out {
			h.w.String(e.name)
			h.w.String(e.value)
			h.w.Int8(b2i(e.readOnly))
			if h.v == 0 {
				h.w.Int8(1) // is_default
			} else {
				h.w.Int8(5) // config_source: DEFAULT_CONFIG
			}
			h.w.Int8(0) // is_sensitive
			if h.v >= 1 {
				h.w.Int32(0) // synonyms
			}
		}
	}
}

func b2i(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

// ---- Group coordination stubs (10-14): stateless, every member leads its own group ----

func (h *req) findCoordinator() {
	h.r.SkipString() // group / key
	if h.v >= 1 {
		h.r.Int8() // key type
		h.w.Int32(0)
	}
	h.w.Int16(wire.ErrNone)
	if h.v >= 1 {
		h.w.NullString(nil)
	}
	h.w.Int32(0)
	h.w.String(h.c.s.advertisedHost)
	h.w.Int32(int32(h.c.s.advertisedPort))
}

func (h *req) joinGroup() {
	h.r.SkipString() // group
	h.r.Int32()      // session timeout
	if h.v >= 1 {
		h.r.Int32() // rebalance timeout
	}
	member := h.r.String()
	h.r.SkipString() // protocol type
	n := h.r.ArrayLen(6)
	protocol := "range"
	var meta []byte
	for i := 0; i < n; i++ {
		name := h.r.String()
		md := h.r.Bytes()
		if i == 0 {
			protocol, meta = name, md
		}
	}
	if member == "" {
		member = "member_" + uuid.NewString()
	}
	h.throttle(2)
	h.w.Int16(wire.ErrNone)
	h.w.Int32(1) // generation
	h.w.String(protocol)
	h.w.String(member) // leader
	h.w.String(member)
	h.w.Int32(1)
	h.w.String(member)
	h.w.Bytes32(meta)
}

func (h *req) syncGroup() {
	h.r.SkipString() // group
	h.r.Int32()      // generation
	member := h.r.String()
	n := h.r.ArrayLen(6)
	mine := []byte{}
	for i := 0; i < n; i++ {
		id := h.r.String()
		a := h.r.Bytes()
		if id == member && a != nil {
			mine = a
		}
	}
	h.throttle(1)
	h.w.Int16(wire.ErrNone)
	h.w.Bytes32(mine)
}

func (h *req) heartbeat() {
	h.r.SkipString()
	h.r.Int32()
	h.r.SkipString()
	h.throttle(1)
	h.w.Int16(wire.ErrNone)
}

func (h *req) leaveGroup() {
	h.r.SkipString()
	h.r.SkipString()
	h.throttle(1)
	h.w.Int16(wire.ErrNone)
}

// ---- SASL (17, 36) ----

func (h *req) saslHandshake() {
	mech := h.r.String()
	c := h.c
	c.authenticated = false
	if h.v == 0 {
		c.rawSasl = true
	}
	code := wire.ErrNone
	if mech != "PLAIN" {
		code = wire.ErrUnsupportedSASLMech
	}
	h.w.Int16(code)
	h.w.Int32(1)
	h.w.String("PLAIN")
}

// saslAuthenticate returns true when the connection must close afterwards.
func (h *req) saslAuthenticate() bool {
	tok := h.r.Bytes()
	fail := func(msg string) bool {
		h.w.Int16(wire.ErrSASLAuthFailed)
		h.w.String(msg)
		h.w.Bytes32([]byte{})
		if h.v >= 1 {
			h.w.Int64(0)
		}
		return true
	}
	user, pass, ok := parsePlain(tok)
	if !ok {
		h.c.s.log.Warn("Kafka SASL: unparsable PLAIN token")
		return fail("Failed to parse plain token")
	}
	if !h.c.authenticate(user, pass) {
		h.c.s.log.Warn("Kafka SASL PLAIN authentication failed", "user", user)
		return fail("Authentication failed")
	}
	h.w.Int16(wire.ErrNone)
	h.w.NullString(nil)
	h.w.Bytes32([]byte{})
	if h.v >= 1 {
		h.w.Int64(0) // session lifetime
	}
	return false
}

// ---- Produce (0) ----

type produceResult struct {
	topic string
	parts []partResult
}

type partResult struct {
	partition int32
	code      int16
}

// produce handles a Produce request. It returns false when acks=0, for which a
// Kafka broker sends no response.
func (h *req) produce() bool {
	if h.v >= 3 {
		h.r.NullableString() // transactional id
	}
	acks := h.r.Int16()
	h.r.Int32() // timeout
	nt := h.r.ArrayLen(6)
	results := make([]produceResult, 0, nt)
	for i := 0; i < nt; i++ {
		topic := h.r.String()
		np := h.r.ArrayLen(8)
		res := produceResult{topic: topic}
		for j := 0; j < np; j++ {
			partition := h.r.Int32()
			size := int(h.r.Int32())
			var set []byte
			if size > 0 {
				set = h.r.Raw(size)
			}
			if h.r.Err() != nil {
				return true
			}
			res.parts = append(res.parts, partResult{partition, h.produceSet(topic, partition, set)})
		}
		results = append(results, res)
	}
	if acks == 0 {
		return false
	}
	w := h.w
	w.Int32(int32(len(results)))
	for _, res := range results {
		w.String(res.topic)
		w.Int32(int32(len(res.parts)))
		for _, p := range res.parts {
			w.Int32(p.partition)
			w.Int16(p.code)
			w.Int64(0) // base offset (not tracked)
			if h.v >= 2 {
				w.Int64(-1) // log append time
			}
			if h.v >= 5 {
				w.Int64(0) // log start offset
			}
		}
	}
	h.throttle(1)
	return true
}

// produceSet validates, stores and publishes the records of one partition and
// returns the Kafka error code. Nothing of a partition is stored or published
// when any of its records is rejected.
//
// Storage happens here, synchronously, so a failure is reported to the client;
// the MQTT publish follows with a UUID the orchestrator skips (no double store).
func (h *req) produceSet(kafkaTopic string, partition int32, set []byte) int16 {
	if len(set) == 0 {
		return wire.ErrNone
	}
	s := h.c.s
	if partition != 0 {
		return wire.ErrUnknownTopicOrPart
	}
	recs, err := wire.DecodeRecords(set)
	if err != nil {
		var ce *wire.UnsupportedCodecError
		if errors.As(err, &ce) {
			s.log.Warn("Kafka produce: unsupported compression", "topic", kafkaTopic, "codec", ce.Codec)
			return wire.ErrUnsupportedCompression
		}
		s.log.Warn("Kafka produce: corrupt record set", "topic", kafkaTopic, "err", err)
		return wire.ErrCorruptMessage
	}
	producer := h.clientID
	if producer == "" {
		producer = defaultProducer
	}
	now := time.Now()
	msgs := make([]stores.BrokerMessage, 0, len(recs))
	for _, rec := range recs {
		mqttTopic := kafkaTopic
		if len(rec.Key) > 0 {
			mqttTopic = string(rec.Key)
		}
		if !s.isWriteAllowed(kafkaTopic, mqttTopic) || !h.c.canPublish(producer, mqttTopic) {
			s.log.Warn("Kafka producer write rejected (stream configuration or ACL)", "topic", kafkaTopic, "key", mqttTopic)
			return wire.ErrTopicAuthFailed
		}
		msgs = append(msgs, stores.BrokerMessage{
			MessageUUID: uuid.NewString(),
			TopicName:   mqttTopic,
			Payload:     append([]byte(nil), rec.Value...),
			QoS:         1,
			ClientID:    producer,
			Time:        now,
		})
	}
	if len(msgs) == 0 {
		return wire.ErrNone
	}

	// Store: the stream named like the Kafka topic plus the stream the
	// orchestrator would pick for the MQTT topic (usually the same one).
	ctx, cancel := h.storeCtx()
	defer cancel()
	perStream := map[*stream][]stores.BrokerMessage{}
	var order []*stream
	add := func(st *stream, m stores.BrokerMessage) {
		if st == nil {
			return
		}
		if _, ok := perStream[st]; !ok {
			order = append(order, st)
		}
		perStream[st] = append(perStream[st], m)
	}
	for _, m := range msgs {
		named := s.byName(kafkaTopic)
		routed := s.routeEnqueue(m.TopicName)
		add(named, m)
		if routed != named {
			add(routed, m)
		}
	}
	for _, st := range order {
		if err := st.store.Enqueue(ctx, perStream[st]); err != nil {
			s.log.Error("Kafka produce: queue write failed", "stream", st.cfg.StreamName, "err", err)
			return wire.ErrKafkaStorageError
		}
	}
	for _, st := range order {
		st.signal()
	}

	// Publish to MQTT; the orchestrator must not store these again.
	code := wire.ErrNone
	h.c.publishers[producer] = struct{}{}
	for _, m := range msgs {
		if s.deps.Publish == nil {
			break
		}
		skip := s.firstMatching(m.TopicName) != nil
		if skip {
			s.selfPub.Store(m.MessageUUID, now)
		}
		if err := s.deps.Publish(producer, m); err != nil {
			if skip {
				s.selfPub.Delete(m.MessageUUID)
			}
			s.log.Warn("Kafka produce: MQTT publish failed (record is stored)", "topic", m.TopicName, "err", err)
			code = wire.ErrUnknownServerError
		}
	}
	return code
}

// ---- Fetch (1) ----

type fetchPart struct {
	partition int32
	offset    int64
}

type fetchTopic struct {
	name  string
	parts []fetchPart
}

type fetchPartResult struct {
	partition int32
	code      int16
	hwm       int64
	logStart  int64
	entries   []stores.KafkaQueueEntry
}

func (h *req) fetch() {
	h.r.Int32() // replica
	maxWait := h.r.Int32()
	h.r.Int32() // min bytes
	if h.v >= 3 {
		h.r.Int32() // max bytes
	}
	if h.v >= 4 {
		h.r.Int8() // isolation
	}
	if h.v >= 7 {
		h.r.Int32() // session id
		h.r.Int32() // epoch
	}
	nt := h.r.ArrayLen(6)
	topics := make([]fetchTopic, 0, nt)
	for i := 0; i < nt; i++ {
		ft := fetchTopic{name: h.r.String()}
		np := h.r.ArrayLen(12)
		for j := 0; j < np; j++ {
			p := fetchPart{partition: h.r.Int32()}
			if h.v >= 9 {
				h.r.Int32() // current leader epoch
			}
			p.offset = h.r.Int64()
			if h.v >= 5 {
				h.r.Int64() // log start offset
			}
			h.r.Int32() // partition max bytes
			ft.parts = append(ft.parts, p)
		}
		topics = append(topics, ft)
	}
	if h.v >= 7 { // forgotten topics
		nf := h.r.ArrayLen(6)
		for i := 0; i < nf; i++ {
			h.r.SkipString()
			np := h.r.ArrayLen(4)
			for j := 0; j < np; j++ {
				h.r.Int32()
			}
		}
	}
	if h.r.Err() != nil {
		return
	}

	s := h.c.s
	deadline := time.Now().Add(time.Duration(maxWait) * time.Millisecond)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	var results [][]fetchPartResult
	for {
		// Take the wake-up channels before reading, so an append in between is not missed.
		var waits []<-chan struct{}
		seen := map[*stream]bool{}
		for _, ft := range topics {
			if st := s.routeFetch(ft.name); st != nil && !seen[st] {
				seen[st] = true
				waits = append(waits, st.wait())
			}
		}
		var hasData bool
		results, hasData = h.fetchOnce(topics)
		if hasData || maxWait <= 0 {
			break
		}
		if !waitAny(h.c.ctx, timer.C, waits) {
			break
		}
	}

	w := h.w
	h.throttle(1)
	if h.v >= 7 {
		w.Int16(wire.ErrNone)
		w.Int32(0) // session id
	}
	w.Int32(int32(len(topics)))
	for i, ft := range topics {
		w.String(ft.name)
		w.Int32(int32(len(results[i])))
		for _, pr := range results[i] {
			w.Int32(pr.partition)
			w.Int16(pr.code)
			w.Int64(pr.hwm)
			if h.v >= 4 {
				w.Int64(pr.hwm) // last stable offset
			}
			if h.v >= 5 {
				w.Int64(pr.logStart)
			}
			if h.v >= 4 {
				w.Int32(0) // aborted transactions
			}
			set := make([]wire.MessageV0, len(pr.entries))
			for k, e := range pr.entries {
				set[k] = wire.MessageV0{Offset: e.Offset, Key: []byte(e.Message.TopicName), Value: e.Message.Payload}
			}
			w.Bytes32(wire.EncodeMessageSetV0(set))
		}
	}
}

// waitAny blocks until one channel fires (true), the timer expires or the
// connection closes (false).
func waitAny(ctx context.Context, timeout <-chan time.Time, chans []<-chan struct{}) bool {
	woke := make(chan struct{}, len(chans))
	done := make(chan struct{})
	defer close(done)
	for _, ch := range chans {
		go func(ch <-chan struct{}) {
			select {
			case <-ch:
				woke <- struct{}{}
			case <-done:
			}
		}(ch)
	}
	select {
	case <-woke:
		return true
	case <-timeout:
		return false
	case <-ctx.Done():
		return false
	}
}

func (h *req) fetchOnce(topics []fetchTopic) ([][]fetchPartResult, bool) {
	s := h.c.s
	ctx, cancel := h.storeCtx()
	defer cancel()
	out := make([][]fetchPartResult, len(topics))
	any := false
	for i, ft := range topics {
		st := s.routeFetch(ft.name)
		for _, p := range ft.parts {
			pr := fetchPartResult{partition: p.partition}
			switch {
			case st == nil || p.partition != 0:
				// unknown topic: empty result, no error (main broker behaviour)
				if st != nil {
					pr.code = wire.ErrUnknownTopicOrPart
				}
			case !h.c.canSubscribe(h.clientID, st.cfg.TopicFilter):
				s.log.Warn("Kafka consumer read rejected by ACL", "topic", ft.name)
			default:
				latest, err := st.store.GetLatestOffset(ctx, ft.name)
				if err == nil {
					pr.hwm = latest + 1 // next offset to be written
					pr.logStart, err = st.store.GetEarliestOffset(ctx, ft.name)
				}
				if err == nil {
					pr.entries, err = st.store.Fetch(ctx, ft.name, p.offset, fetchRowLimit)
				}
				if err != nil {
					s.log.Warn("Kafka fetch failed", "topic", ft.name, "err", err)
					pr.code = wire.ErrUnknownServerError
					pr.entries, pr.hwm = nil, 0
				}
				if len(pr.entries) > 0 {
					any = true
				}
			}
			out[i] = append(out[i], pr)
		}
	}
	return out, any
}

// ---- ListOffsets (2) ----

func (h *req) listOffsets() {
	h.r.Int32() // replica
	if h.v >= 2 {
		h.r.Int8() // isolation
	}
	nt := h.r.ArrayLen(6)
	type item struct {
		partition int32
		code      int16
		offset    int64
	}
	type topicRes struct {
		name  string
		parts []item
	}
	s := h.c.s
	ctx, cancel := h.storeCtx()
	defer cancel()
	var res []topicRes
	for i := 0; i < nt; i++ {
		tr := topicRes{name: h.r.String()}
		np := h.r.ArrayLen(12)
		for j := 0; j < np; j++ {
			p := h.r.Int32()
			ts := h.r.Int64()
			if h.v == 0 {
				h.r.Int32() // max num offsets
			}
			it := item{partition: p}
			st := s.routeFetch(tr.name)
			switch {
			case st == nil:
			case p != 0:
				it.code = wire.ErrUnknownTopicOrPart
			case ts == -1: // latest: the next offset
				latest, err := st.store.GetLatestOffset(ctx, tr.name)
				if err != nil {
					it.code = wire.ErrUnknownServerError
				}
				it.offset = latest + 1
			default: // earliest (any timestamp other than -1)
				earliest, err := st.store.GetEarliestOffset(ctx, tr.name)
				if err != nil {
					it.code = wire.ErrUnknownServerError
				}
				it.offset = earliest
			}
			tr.parts = append(tr.parts, it)
		}
		res = append(res, tr)
	}
	h.throttle(2)
	h.w.Int32(int32(len(res)))
	for _, tr := range res {
		h.w.String(tr.name)
		h.w.Int32(int32(len(tr.parts)))
		for _, it := range tr.parts {
			h.w.Int32(it.partition)
			h.w.Int16(it.code)
			if h.v == 0 {
				h.w.Int32(1)
				h.w.Int64(it.offset)
			} else {
				h.w.Int64(-1) // timestamp
				h.w.Int64(it.offset)
			}
		}
	}
}

// ---- OffsetCommit (8) / OffsetFetch (9) ----

func (h *req) offsetCommit() {
	group := h.r.String()
	if group == "" {
		group = defaultGroup
	}
	if h.v >= 1 {
		h.r.Int32()      // generation
		h.r.SkipString() // member
	}
	if h.v >= 7 {
		h.r.NullableString() // group instance id
	}
	if h.v >= 2 && h.v <= 4 {
		h.r.Int64() // retention time
	}
	nt := h.r.ArrayLen(6)
	type partRes struct {
		partition int32
		code      int16
	}
	type topicRes struct {
		name  string
		parts []partRes
	}
	s := h.c.s
	ctx, cancel := h.storeCtx()
	defer cancel()
	var res []topicRes
	for i := 0; i < nt; i++ {
		tr := topicRes{name: h.r.String()}
		np := h.r.ArrayLen(12)
		for j := 0; j < np; j++ {
			p := h.r.Int32()
			off := h.r.Int64()
			if h.v == 1 {
				h.r.Int64() // commit timestamp
			}
			if h.v >= 6 {
				h.r.Int32() // leader epoch
			}
			h.r.NullableString() // metadata
			pr := partRes{partition: p}
			if st := s.routeFetch(tr.name); st == nil {
				pr.code = wire.ErrUnknownTopicOrPart
			} else if err := st.store.CommitOffset(ctx, group, tr.name, int(p), off); err != nil {
				s.log.Warn("Kafka offset commit failed", "group", group, "topic", tr.name, "err", err)
				pr.code = wire.ErrUnknownServerError
			}
			tr.parts = append(tr.parts, pr)
		}
		res = append(res, tr)
	}
	h.throttle(3)
	h.w.Int32(int32(len(res)))
	for _, tr := range res {
		h.w.String(tr.name)
		h.w.Int32(int32(len(tr.parts)))
		for _, p := range tr.parts {
			h.w.Int32(p.partition)
			h.w.Int16(p.code)
		}
	}
}

func (h *req) offsetFetch() {
	group := h.r.String()
	if group == "" {
		group = defaultGroup
	}
	nt := h.r.ArrayLen(6)
	s := h.c.s
	ctx, cancel := h.storeCtx()
	defer cancel()
	type partRes struct {
		partition int32
		offset    int64
		code      int16
	}
	type topicRes struct {
		name  string
		parts []partRes
	}
	var res []topicRes
	for i := 0; i < nt; i++ {
		tr := topicRes{name: h.r.String()}
		np := h.r.ArrayLen(4)
		for j := 0; j < np; j++ {
			pr := partRes{partition: h.r.Int32(), offset: -1}
			if st := s.routeFetch(tr.name); st != nil {
				off, ok, err := st.store.GetOffset(ctx, group, tr.name, int(pr.partition))
				switch {
				case err != nil:
					s.log.Warn("Kafka offset fetch failed", "group", group, "topic", tr.name, "err", err)
					pr.code = wire.ErrUnknownServerError
				case ok:
					pr.offset = off
				}
			}
			tr.parts = append(tr.parts, pr)
		}
		res = append(res, tr)
	}
	h.w.Int32(int32(len(res)))
	for _, tr := range res {
		h.w.String(tr.name)
		h.w.Int32(int32(len(tr.parts)))
		for _, p := range tr.parts {
			h.w.Int32(p.partition)
			h.w.Int64(p.offset)
			h.w.String("") // metadata
			h.w.Int16(p.code)
		}
	}
}
