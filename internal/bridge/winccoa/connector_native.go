package winccoa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/oahost"
)

// reservedNativeBranches are owned by the native namespace; configured query
// output must never publish into them.
var reservedNativeBranches = []string{"winccoa/this", "winccoa/remote", "winccoa/node", "winccoa/cns"}

// CheckReservedOutput reports an error when an address's output topic prefix
// falls into a reserved native branch.
func CheckReservedOutput(namespace string, addr Address) error {
	base := joinTopic(namespace, addr.Topic)
	for _, r := range reservedNativeBranches {
		if base == r || strings.HasPrefix(base, r+"/") {
			return fmt.Errorf("address topic %q resolves into reserved branch %q", base, r)
		}
	}
	return nil
}

type nativeAddress struct {
	addr Address
	ref  uint64
	ok   bool
}

// nativeConnector runs the configured dpQueryConnectSingle addresses through
// the embedding host instead of GraphQL. Registration state is tracked per
// address; the connector is connected only when every address is registered.
type nativeConnector struct {
	name      string
	namespace string
	cfg       *ConnectionConfig
	pub       *publisher
	publish   LocalPublisher
	api       oahost.API
	logger    *slog.Logger
	metrics   metrics

	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{}
	kick    chan struct{}
	unwatch func()

	mu    sync.Mutex
	addrs []*nativeAddress
}

func newNativeConnector(name, namespace string, cfg *ConnectionConfig, pub *publisher, publish LocalPublisher, api oahost.API, logger *slog.Logger) *nativeConnector {
	n := &nativeConnector{
		name:      name,
		namespace: namespace,
		cfg:       cfg,
		pub:       pub,
		publish:   publish,
		api:       api,
		logger:    logger.With("device", name, "transport", "native"),
		stopped:   make(chan struct{}),
		kick:      make(chan struct{}, 1),
	}
	for _, a := range cfg.Addresses {
		n.addrs = append(n.addrs, &nativeAddress{addr: a})
	}
	return n
}

func (n *nativeConnector) MessagesIn() float64 { return n.metrics.sampleRate() }
func (n *nativeConnector) IsConnected() bool   { return n.metrics.connFlag.Load() }

func (n *nativeConnector) SampleMetrics(now time.Time) MetricsSnapshot {
	return MetricsSnapshot{MessagesIn: n.MessagesIn(), Connected: n.IsConnected(), Timestamp: now.UTC()}
}

func (n *nativeConnector) Start(ctx context.Context) error {
	for _, a := range n.addrs {
		if err := CheckReservedOutput(n.namespace, a.addr); err != nil {
			return err
		}
	}
	n.ctx, n.cancel = context.WithCancel(ctx)
	n.unwatch = n.api.C.WatchSystems(func(system string, available bool) {
		n.onSystem(system, available)
	})
	go n.runLoop()
	return nil
}

func (n *nativeConnector) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
	<-n.stopped
	if n.unwatch != nil {
		n.unwatch()
	}
}

// onSystem invalidates registrations when OA reports a system change. The
// host has already released its side of registrations on the lost system;
// a reappearing system triggers re-registration without duplicates because
// only addresses marked not-ok are registered again.
func (n *nativeConnector) onSystem(system string, available bool) {
	n.mu.Lock()
	for _, a := range n.addrs {
		if !available && a.ok && querySystem(a.addr.Query, system) {
			n.api.C.DropRef(a.ref)
			a.ok = false
			a.ref = 0
		}
	}
	n.mu.Unlock()
	n.refreshConnected()
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// querySystem reports whether a dpQuery FROM pattern targets system. An
// unqualified pattern targets the local system, reported by the host under
// its real name, so any change of an unqualified query's system counts.
func querySystem(query, system string) bool {
	q := strings.ToUpper(query)
	i := strings.Index(q, " FROM ")
	if i < 0 {
		return true
	}
	from := strings.TrimSpace(query[i+6:])
	from = strings.Trim(strings.SplitN(from, " ", 2)[0], "'\"")
	if sys, _, ok := strings.Cut(from, ":"); ok {
		return sys == system
	}
	return true
}

func (n *nativeConnector) refreshConnected() {
	n.mu.Lock()
	all := len(n.addrs) > 0
	for _, a := range n.addrs {
		if !a.ok {
			all = false
		}
	}
	n.mu.Unlock()
	n.metrics.connFlag.Store(all)
}

func (n *nativeConnector) runLoop() {
	defer close(n.stopped)
	delay := time.Duration(n.cfg.ReconnectDelay) * time.Millisecond
	timeout := time.Duration(n.cfg.ConnectionTimeout) * time.Millisecond
	for {
		n.registerPending(timeout)
		select {
		case <-n.ctx.Done():
			n.disconnectAll()
			return
		case <-n.kick:
		case <-time.After(delay):
		}
	}
}

func (n *nativeConnector) registerPending(timeout time.Duration) {
	n.mu.Lock()
	var todo []*nativeAddress
	for _, a := range n.addrs {
		if !a.ok {
			todo = append(todo, a)
		}
	}
	n.mu.Unlock()
	for _, a := range todo {
		if n.ctx.Err() != nil {
			return
		}
		addr := a.addr
		ref, err := n.api.QueryConnect(n.ctx, addr.Query, addr.Answer, func(m oahost.Message) {
			n.onRows(addr, m)
		}, timeout)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				n.logger.Warn("winccoa native query registration failed", "topic", addr.Topic, "query", addr.Query, "err", err)
			}
			continue
		}
		n.mu.Lock()
		a.ref = ref
		a.ok = true
		n.mu.Unlock()
		n.logger.Info("winccoa native query registered", "topic", addr.Topic, "ref", ref)
	}
	n.refreshConnected()
}

func (n *nativeConnector) disconnectAll() {
	n.mu.Lock()
	var refs []uint64
	for _, a := range n.addrs {
		if a.ok {
			refs = append(refs, a.ref)
			a.ok = false
			a.ref = 0
		}
	}
	n.mu.Unlock()
	n.metrics.connFlag.Store(false)
	for _, ref := range refs {
		ctx, cancel := context.WithTimeout(context.Background(), n.api.C.Limits().DefaultTimeout)
		if err := n.api.QueryDisconnect(ctx, ref); err != nil {
			n.logger.Warn("winccoa native query disconnect failed", "ref", ref, "err", err)
		}
		cancel()
	}
}

func (n *nativeConnector) onRows(addr Address, m oahost.Message) {
	if errText := m.String(oahost.TagError); errText != "" {
		n.logger.Warn("winccoa native query error", "topic", addr.Topic, "err", errText)
		return
	}
	rows, err := oahost.QueryRows(m)
	if err != nil {
		n.logger.Warn("winccoa native query decode failed", "topic", addr.Topic, "err", err)
		return
	}
	table := make([]any, len(rows))
	for i, r := range rows {
		cells := make([]any, len(r))
		for j, v := range r {
			cells[j] = v.JSON()
		}
		table[i] = cells
	}
	publishQueryRows(n.pub, n.publish, &n.metrics, n.logger, addr, table)
}
