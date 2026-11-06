package kafka

import (
	"context"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

const (
	flushInterval   = 100 * time.Millisecond
	firstPruneAfter = 5 * time.Second
	pruneInterval   = time.Hour
	// maxBuffered bounds the messages held while a store is failing; the
	// oldest are dropped (and logged) beyond it.
	maxBuffered = 200000
	// selfPubTTL is how long a Produce-published UUID is remembered when the
	// message never comes back through the bus.
	selfPubTTL = 30 * time.Second
)

// orchestrator copies MQTT messages matching a stream filter into the stream's
// queue store: one inline subscription over all filters, a buffer flushed every
// 100 ms, hourly per-stream retention pruning.
//
// Records produced over Kafka are stored by Produce itself (so a storage failure
// can be answered with an error code) and then published to MQTT with a UUID
// registered in Server.selfPub; the orchestrator skips exactly those UUIDs, so
// nothing is stored twice and nothing is lost.
type orchestrator struct {
	s     *Server
	subID int

	mu  sync.Mutex
	buf []stores.BrokerMessage

	cancel context.CancelFunc
	done   chan struct{}
}

func (o *orchestrator) start() error {
	s := o.s
	filters := make([]string, 0, len(s.streams))
	seen := map[string]bool{}
	for _, st := range s.streams {
		if !seen[st.cfg.TopicFilter] {
			seen[st.cfg.TopicFilter] = true
			filters = append(filters, st.cfg.TopicFilter)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	o.done = make(chan struct{})
	if len(filters) == 0 || s.deps.Bus == nil {
		close(o.done)
		return nil
	}
	if err := s.deps.Bus.Validate(filters); err != nil {
		cancel()
		close(o.done)
		return err
	}
	o.subID = s.deps.Bus.SubscribeFunc(filters, o.onMessage)
	s.log.Info("Kafka stream orchestrator subscribed", "server", s.cfg.Name, "filters", filters)
	go o.run(ctx)
	return nil
}

func (o *orchestrator) onMessage(msg stores.BrokerMessage) {
	if msg.MessageUUID != "" {
		if _, ok := o.s.selfPub.LoadAndDelete(msg.MessageUUID); ok {
			return // produced over Kafka: already stored by Produce
		}
	}
	o.mu.Lock()
	if len(o.buf) >= maxBuffered {
		drop := len(o.buf) - maxBuffered + 1
		o.buf = o.buf[drop:]
		o.s.log.Warn("Kafka orchestrator buffer full, dropping oldest messages", "server", o.s.cfg.Name, "dropped", drop)
	}
	o.buf = append(o.buf, msg)
	o.mu.Unlock()
}

func (o *orchestrator) run(ctx context.Context) {
	defer close(o.done)
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	firstPrune := time.NewTimer(firstPruneAfter)
	defer firstPrune.Stop()
	hourly := time.NewTicker(pruneInterval)
	defer hourly.Stop()
	for {
		select {
		case <-ctx.Done():
			o.flush(context.Background()) // final flush
			return
		case <-flush.C:
			o.flush(ctx)
			o.sweepSelfPub()
		case <-firstPrune.C:
			o.prune(ctx)
		case <-hourly.C:
			o.prune(ctx)
		}
	}
}

func (o *orchestrator) take() []stores.BrokerMessage {
	o.mu.Lock()
	defer o.mu.Unlock()
	b := o.buf
	o.buf = nil
	return b
}

// flush stores the buffered messages per stream. A failing stream's messages
// go back to the front of the buffer and are retried on the next tick.
func (o *orchestrator) flush(ctx context.Context) {
	batch := o.take()
	if len(batch) == 0 {
		return
	}
	var failed []stores.BrokerMessage
	for st, msgs := range o.s.groupByStream(batch) {
		if err := st.store.Enqueue(ctx, msgs); err != nil {
			o.s.log.Error("Kafka queue write failed", "server", o.s.cfg.Name, "stream", st.cfg.StreamName, "messages", len(msgs), "err", err)
			failed = append(failed, msgs...)
			continue
		}
		st.signal()
	}
	if len(failed) > 0 && ctx.Err() == nil {
		o.mu.Lock()
		o.buf = append(failed, o.buf...)
		if len(o.buf) > maxBuffered {
			o.buf = o.buf[len(o.buf)-maxBuffered:]
		}
		o.mu.Unlock()
	}
}

func (o *orchestrator) prune(ctx context.Context) {
	now := time.Now()
	for _, st := range o.s.streams {
		n, err := st.store.PruneExpired(ctx, now.Add(-st.retention))
		if err != nil {
			o.s.log.Warn("Kafka retention prune failed", "server", o.s.cfg.Name, "stream", st.cfg.StreamName, "err", err)
		} else if n > 0 {
			o.s.log.Info("Kafka retention prune", "server", o.s.cfg.Name, "stream", st.cfg.StreamName, "deleted", n)
		}
	}
}

func (o *orchestrator) sweepSelfPub() {
	cutoff := time.Now().Add(-selfPubTTL)
	o.s.selfPub.Range(func(k, v any) bool {
		if v.(time.Time).Before(cutoff) {
			o.s.selfPub.Delete(k)
		}
		return true
	})
}

func (o *orchestrator) stop() {
	if o.cancel == nil {
		return
	}
	if o.subID != 0 {
		o.s.deps.Bus.Unsubscribe(o.subID)
		o.subID = 0
	}
	o.cancel()
	<-o.done
}
