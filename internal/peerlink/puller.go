package peerlink

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
	"monstermq.io/edge/internal/tlsutil"
)

// Puller states (11.1).
const (
	stateStopped int32 = iota
	stateBackoff
	stateDialing
	stateHandshake
	stateSnapshot
	stateStreaming
)

var stateNames = [...]string{"STOPPED", "BACKOFF", "DIALING", "HANDSHAKE", "SNAPSHOT", "STREAMING"}

const (
	dialTimeout      = 5 * time.Second
	backoffInitial   = 200 * time.Millisecond
	backoffResetLive = 30 * time.Second
	configErrorEvery = 5 * time.Minute
	commitEvery      = 100 * time.Millisecond
)

// puller is the consumer side of one link: it pulls from one source peer and injects locally.
type puller struct {
	m        *Manager
	nodeID   string
	address  string
	peer     config.PeerConfig
	tlsPeer  tlsutil.Peer
	secrets  [][]byte
	tlsCfg   *tls.Config
	filter   includeExclude
	inj      *mqtt.Client
	intern   *internCache
	pace     pacer
	maxAgeMs int64

	markReplicas    bool
	fetchMaxRecords int

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	graceful  atomic.Bool
	done      chan struct{}
	wake      chan struct{}
	resyncReq atomic.Bool
	// snapPending is set when a source offers its retained snapshot (FILL) and cleared only after
	// SNAPSHOT_END was applied and flushed, so an interrupted snapshot is pulled again on the next
	// session even though that HELLO resumes the same epoch.
	snapPending atomic.Bool
	kick        chan struct{} // ends the current session gracefully (resync)

	sessMu     sync.Mutex
	sessCancel context.CancelFunc

	// Link state kept across reconnects, in memory only (9.8, 11.1).
	epoch       atomic.Uint64
	appliedNext atomic.Uint64
	lastSeenLeo atomic.Uint64
	state       atomic.Int32
	oaRetained  atomic.Bool
	crcBase     uint64
	crcFails    int

	batches, injected, retainOnly, appliedBytes, dupSkipped atomic.Uint64
	dropped                                                 [numDropReasons]atomic.Uint64
	diverged                                                [numDivReasons]atomic.Uint64
	rejected, unknownProps, gapLost, sourceResets           atomic.Uint64
	resetLost, reconnects, sessions, crcErrors              atomic.Uint64
	snapFilled, snapSkipped, snapNewer, snapTrunc, snaps    atomic.Uint64
	paced, snapInterrupted, flushErrors, willResent         atomic.Uint64
	clockSkewMs                                             atomic.Int64
	rttUs                                                   atomic.Int64
	topicRootMismatch, retainedClassMismatch, partnerWarned atomic.Bool
	hist                                                    latencyHist
	lastErrMu                                               sync.Mutex
	lastErr                                                 string
	rate                                                    rateLimiter
	warnedFrame                                             atomic.Bool
}

func newPuller(m *Manager, peer config.PeerConfig, tp tlsutil.Peer, secrets [][]byte) (*puller, error) {
	f, err := newIncludeExclude(peer.Receive.GetInclude(), peer.Receive.Exclude)
	if err != nil {
		return nil, fmt.Errorf("peerlink: peer %s Receive filters: %w", peer.NodeID, err)
	}
	cfg := m.cfg
	p := &puller{
		m:               m,
		nodeID:          peer.NodeID,
		address:         peer.Address,
		peer:            peer,
		tlsPeer:         tp,
		secrets:         secrets,
		filter:          f,
		intern:          newInternCache(),
		maxAgeMs:        int64(cfg.Receive.MaxRecordAgeMs),
		markReplicas:    cfg.Receive.MarkReplicas,
		fetchMaxRecords: cfg.Fetch.GetMaxRecords(),
		stopCh:          make(chan struct{}),
		done:            make(chan struct{}),
		wake:            make(chan struct{}, 1),
		kick:            make(chan struct{}, 1),
	}
	p.pace.factor = cfg.Receive.GetCatchUpRateFactor()
	p.pace.maxRate = float64(cfg.Receive.MaxApplyRate)
	p.inj = m.srv.NewClient(nil, ListenerID, InjectorPrefix+peer.NodeID, true)
	p.inj.Properties.ProtocolVersion = 5
	return p, nil
}

func (p *puller) start() {
	p.startOnce.Do(func() {
		go p.run()
	})
}

// stop ends the puller. graceful finishes the current batch, flushes, commits and sends
// GOAWAY(shutdown); otherwise the connection is closed at once.
func (p *puller) stop(graceful bool) {
	p.stopOnce.Do(func() {
		p.graceful.Store(graceful)
		close(p.stopCh)
		// Only a streaming session can finish its batch; dialing or a handshake just ends.
		if !graceful || p.state.Load() != stateStreaming {
			p.sessMu.Lock()
			if p.sessCancel != nil {
				p.sessCancel()
			}
			p.sessMu.Unlock()
		}
	})
	p.startOnce.Do(func() { close(p.done) })
}

func (p *puller) wait(ctx context.Context) {
	select {
	case <-p.done:
	case <-ctx.Done():
		p.sessMu.Lock()
		if p.sessCancel != nil {
			p.sessCancel()
		}
		p.sessMu.Unlock()
		<-p.done
	}
}

func (p *puller) stopping() bool {
	select {
	case <-p.stopCh:
		return true
	default:
		return false
	}
}

func (p *puller) requestResync() {
	p.resyncReq.Store(true)
	select {
	case p.kick <- struct{}{}:
	default:
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *puller) setState(s int32) {
	if p.state.Swap(s) != s && (s == stateStreaming || s == stateBackoff || s == stateStopped) {
		p.m.stateChanged()
	}
}

func (p *puller) setLastError(s string) {
	p.lastErrMu.Lock()
	p.lastErr = s
	p.lastErrMu.Unlock()
}

// sessionResult tells the run loop how to back off after a session.
type sessionResult struct {
	applied   bool
	lived     time.Duration
	configErr bool
	resync    bool
	code      wire.GoAwayCode
	err       error
}

func (p *puller) run() {
	defer close(p.done)
	defer p.setState(stateStopped)
	reconnectMax := time.Duration(p.m.cfg.Fetch.GetReconnectMaxMs()) * time.Millisecond
	backoff := backoffInitial
	first := true
	for !p.stopping() {
		if !first {
			p.reconnects.Add(1)
		}
		first = false
		res := p.session()
		if p.stopping() {
			return
		}
		var wait time.Duration
		switch {
		case res.resync:
			backoff, wait = backoffInitial, 0
		case res.configErr:
			wait = reconnectMax
		case res.applied || res.lived >= backoffResetLive:
			backoff = backoffInitial
			wait = backoff
		default:
			wait = backoff
			backoff = min(backoff*2, reconnectMax)
		}
		p.logResult(res)
		if wait > 0 {
			wait = time.Duration(float64(wait) * (0.8 + 0.4*rand.Float64()))
			p.setState(stateBackoff)
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-p.stopCh:
				t.Stop()
				return
			case <-p.wake:
				t.Stop()
			}
		}
	}
}

func (p *puller) logResult(res sessionResult) {
	if res.err == nil && res.code == 0 {
		return
	}
	msg := ""
	if res.err != nil {
		msg = res.err.Error()
	} else {
		msg = "GOAWAY " + res.code.String()
	}
	p.setLastError(msg)
	switch {
	case res.configErr:
		if ok, n := p.rate.allow("config:"+res.code.String(), configErrorEvery); ok {
			p.m.logger.Error("peerlink: link refused; check the configuration on both nodes", "peer", p.nodeID,
				"address", p.address, "code", res.code.String(), "error", msg, "suppressed", n)
		}
	case res.code == wire.GoAwayShutdown || res.code == wire.GoAwaySuperseded:
		p.m.logger.Info("peerlink: source closed the link", "peer", p.nodeID, "code", res.code.String())
	default:
		if ok, n := p.rate.allow("link", 10*time.Second); ok {
			p.m.logger.Warn("peerlink: link down", "peer", p.nodeID, "address", p.address, "error", msg, "suppressed", n)
		}
	}
}

// session runs one connection: dial, handshake, optional snapshot, streaming.
func (p *puller) session() (res sessionResult) {
	ctx, cancel := context.WithCancel(p.m.ctx)
	p.sessMu.Lock()
	p.sessCancel = cancel
	p.sessMu.Unlock()
	defer func() {
		p.sessMu.Lock()
		p.sessCancel = nil
		p.sessMu.Unlock()
		cancel()
	}()
	select {
	case <-p.kick:
	default:
	}

	p.setState(stateDialing)
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", p.address)
	if err != nil {
		res.err = err
		return
	}
	var conn net.Conn = raw
	defer func() { _ = conn.Close() }()
	stopWatch := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stopWatch()

	p.setState(stateHandshake)
	var cs *tls.ConnectionState
	if p.tlsCfg != nil {
		tc := tls.Client(raw, p.tlsCfg)
		hctx, hcancel := context.WithTimeout(ctx, handshakeTimeout)
		err := tc.HandshakeContext(hctx)
		hcancel()
		if err != nil {
			res.err = fmt.Errorf("tls handshake: %w", err)
			return
		}
		st := tc.ConnectionState()
		cs = &st
		conn = tc
	}
	started := time.Now()
	hs, err := p.handshake(conn, cs)
	if err != nil {
		res.err = err
		var ge *goAwayError
		if errors.As(err, &ge) {
			res.code = ge.code
			res.configErr = ge.code.ConfigError()
			if ge.code == wire.GoAwayOffsetOutOfRange {
				p.epoch.Store(0)
				p.appliedNext.Store(0)
			}
		}
		return
	}
	p.sessions.Add(1)

	ac := &applyCtx{ctx: ctx, srcRoot: hs.srcRoot, ownRoot: p.m.namespaceRoot(), epoch: hs.epoch,
		rttHalf: hs.rttHalfMs}
	mode := snapNone
	fillOK := hs.caps&wire.CapSnapshotFill != 0 && p.m.cfg.Snapshot.GetMode() == config.PeerLinkSnapshotFill
	switch {
	case !fillOK || p.oaRetained.Load():
		p.snapPending.Store(false)
	case hs.flags&wire.HelloOKSnapshotAvailable != 0:
		p.snapPending.Store(true)
	}
	if p.resyncReq.Load() {
		if hs.caps&wire.CapResyncNewer != 0 {
			mode = snapNewer
		} else {
			p.resyncReq.Store(false)
			p.m.logger.Warn("peerlink: resync requested but the source does not support it", "peer", p.nodeID)
		}
	} else if p.snapPending.Load() {
		mode = snapFill
	}
	if mode != snapNone {
		p.setState(stateSnapshot)
		ac.mode = mode
		if err := p.snapshotPhase(ctx, conn, hs, ac); err != nil {
			if !errors.Is(err, errStopped) {
				p.snapInterrupted.Add(1)
				if ok, n := p.rate.allow("snapshot-interrupted", 10*time.Second); ok {
					p.m.logger.Warn("peerlink: retained snapshot interrupted; it is pulled again on the next session",
						"peer", p.nodeID, "error", err, "suppressed", n)
				}
			}
			res.err = err
			var ge *goAwayError
			if errors.As(err, &ge) {
				res.code = ge.code
				res.configErr = ge.code.ConfigError()
			}
			res.lived = time.Since(started)
			return
		}
		if mode == snapFill {
			p.snapPending.Store(false)
		}
		if mode == snapNewer {
			p.resyncReq.Store(false)
			// A NEWER snapshot also covers a pending FILL: it applies every absent topic as well.
			p.snapPending.Store(false)
			select {
			case <-p.kick:
			default:
			}
		}
		ac.mode = snapNone
	}

	p.setState(stateStreaming)
	p.m.logger.Info("peerlink: streaming from source", "peer", p.nodeID, "address", p.address, "tls", cs != nil,
		"epoch", hs.epoch, "resumeAt", p.appliedNext.Load(), "oaRetained", p.oaRetained.Load())
	sr := p.stream(ctx, cancel, conn, hs, ac)
	sr.lived = time.Since(started)
	return sr
}

type goAwayError struct {
	code   wire.GoAwayCode
	reason string
	local  bool // sent by this node
}

func (e *goAwayError) Error() string {
	dir := "source sent"
	if e.local {
		dir = "refused source:"
	}
	if e.reason != "" {
		return fmt.Sprintf("%s GOAWAY %s: %s", dir, e.code, e.reason)
	}
	return fmt.Sprintf("%s GOAWAY %s", dir, e.code)
}

// handshakeState is what the streaming phase needs from the handshake.
type handshakeState struct {
	br        *bufio.Reader
	fr        *wire.FrameReader
	caps      uint64
	flags     uint16
	epoch     uint64
	srcRoot   string
	rttHalfMs int64
}

func readFrameExpect(fr *wire.FrameReader, want wire.FrameType, dst wire.Frame) error {
	for {
		t, body, err := fr.ReadFrame()
		if err != nil {
			return err
		}
		switch t {
		case want:
			return dst.Decode(body)
		case wire.FrameGoAway:
			var g wire.GoAway
			_ = g.Decode(body)
			return &goAwayError{code: g.Code, reason: g.Reason}
		default:
			if t.Known() {
				return &goAwayError{code: wire.GoAwayProtocol, reason: "unexpected " + t.String(), local: true}
			}
		}
	}
}

// handshake runs the consumer side of 9.5 and applies HELLO_OK to the link state (9.6).
func (p *puller) handshake(conn net.Conn, cs *tls.ConnectionState) (*handshakeState, error) {
	m := p.m
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := wire.WritePreamble(conn); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(conn, ioBufferSize)
	fr := wire.NewFrameReader(br, wire.MaxPreAuthFrame)
	var sh wire.ServerHello
	if err := readFrameExpect(fr, wire.FrameServerHello, &sh); err != nil {
		return nil, err
	}
	if sh.VersionMajor != wire.VersionMajor {
		return nil, p.refuse(conn, wire.GoAwayVersion, fmt.Sprintf("source speaks major version %d", sh.VersionMajor))
	}
	var exporter []byte
	if cs != nil {
		exporter, _ = cs.ExportKeyingMaterial(wire.ExporterLabel, nil, wire.ExporterLen)
	}
	h := wire.Hello{
		Capabilities:         m.ownCaps(cs != nil),
		InstanceID:           m.instanceID,
		MaxRecordBytes:       p.maxRecordAccept(),
		RetainedClass:        m.deps.RetainedClass,
		NonceC:               wire.NewNonce(),
		ConsumerNodeID:       m.nodeID,
		ExpectedSourceNodeID: p.nodeID,
		TopicRoot:            m.namespaceRoot(),
		OASystem:             m.oaSystem,
	}
	if e := p.epoch.Load(); e != 0 {
		h.LastEpoch = e
		h.ResumeOffset = p.appliedNext.Load()
		h.LastSeenLeo = p.lastSeenLeo.Load()
	}
	if cs != nil && len(p.secrets) > 0 && len(exporter) > 0 {
		h.Flags |= wire.HelloFlagMAC
		h.MAC = wire.MAC(p.secrets[0], wire.ConsumerMACInput(&sh.NonceS, &h.NonceC, m.nodeID, p.nodeID, exporter))
	}
	sent := time.Now()
	if err := wire.WriteFrame(conn, &h); err != nil {
		return nil, err
	}
	var ok wire.HelloOK
	if err := readFrameExpect(fr, wire.FrameHelloOK, &ok); err != nil {
		return nil, err
	}
	rtt := time.Since(sent)

	sid, err := config.CanonicalNodeID(ok.SourceNodeID)
	switch {
	case err == nil && sid == m.nodeID:
		return nil, p.refuse(conn, wire.GoAwaySelfConnection, "source NodeId equals own NodeId")
	case err != nil || sid != p.nodeID:
		return nil, p.refuse(conn, wire.GoAwayWrongNode, fmt.Sprintf("source announced NodeId %q", ok.SourceNodeID))
	}
	certVerified := false
	if cs != nil && len(cs.PeerCertificates) > 0 && !p.peer.Tls.InsecureSkipVerify {
		if err := m.tls.trust.Verify(cs.PeerCertificates, p.tlsPeer, x509.ExtKeyUsageServerAuth); err == nil {
			certVerified = true
		} else if len(p.secrets) == 0 {
			return nil, p.refuse(conn, wire.GoAwayIdentityMismatch, "server certificate: "+err.Error())
		}
	}
	macOK := false
	if cs != nil && len(p.secrets) > 0 {
		in := wire.SourceMACInput(&h.NonceC, &sh.NonceS, p.nodeID, m.nodeID, exporter)
		if len(exporter) == 0 || wire.MatchMAC(p.secrets, in, &ok.MACS) < 0 {
			return nil, p.refuse(conn, wire.GoAwayAuthFailed, "source MAC mismatch")
		}
		macOK = true
	}
	if !certVerified && !macOK && !m.cfg.AllowUnauthenticatedPeers {
		return nil, p.refuse(conn, wire.GoAwayAuthFailed, "source not authenticated")
	}

	hs := &handshakeState{br: br, fr: fr, caps: ok.Capabilities, flags: ok.Flags, epoch: ok.Epoch, srcRoot: ok.TopicRoot,
		rttHalfMs: rtt.Milliseconds() / 2}
	p.rttUs.Store(rtt.Microseconds())
	frameMax := max(m.cfg.Fetch.GetMaxBytes(), int(ok.MaxRecordBytes)) + wire.FrameSlack
	if limit := m.cfg.Receive.GetMaxFrameBytes(); frameMax > limit {
		frameMax = limit
		if int(ok.MaxRecordBytes)+wire.FrameSlack > limit && p.warnedFrame.CompareAndSwap(false, true) {
			m.logger.Error("peerlink: source captures records larger than Receive.MaxFrameBytes allows; they arrive as tombstones",
				"peer", p.nodeID, "sourceMaxRecordBytes", ok.MaxRecordBytes, "maxFrameBytes", limit)
		}
	}
	fr.Max = uint32(frameMax)

	if ok.Epoch != p.epoch.Load() {
		if old := p.epoch.Load(); old != 0 {
			p.sourceResets.Add(1)
			var lost uint64
			if seen, applied := p.lastSeenLeo.Load(), p.appliedNext.Load(); seen > applied {
				lost = seen - applied
			}
			p.resetLost.Add(lost)
			m.logger.Warn("peerlink: source restarted (new epoch)", "peer", p.nodeID, "resetLostLowerBound", lost)
		}
		p.epoch.Store(ok.Epoch)
		p.crcFails = 0
	}
	p.appliedNext.Store(ok.ResumeAt)
	p.lastSeenLeo.Store(ok.Leo)
	if ok.LostOnResume > 0 {
		p.gapLost.Add(ok.LostOnResume)
		m.logger.Warn("peerlink: records lost before resume (source log overflow)", "peer", p.nodeID, "lostOnResume", ok.LostOnResume)
	}
	oaRet, why := oaRetainedFor(p.peer.RedundancyPartner, m.deps.RetainedClass, m.oaSystem, ok.RetainedClass, ok.OASystem)
	if p.oaRetained.Swap(oaRet) != oaRet || oaRet {
		m.logger.Info("peerlink: oaRetained decision", "peer", p.nodeID, "oaRetained", oaRet)
	}
	if !p.partnerWarned.Swap(why != "") && why != "" {
		m.logger.Warn("peerlink: RedundancyPartner set but the link is not oaRetained; replicas are written to WinCC OA",
			"peer", p.nodeID, "reason", why, "own", m.oaSystem, "source", ok.OASystem)
	}
	own := m.namespaceRoot()
	mism := own != "" && ok.TopicRoot != "" && own != ok.TopicRoot
	if !p.topicRootMismatch.Swap(mism) && mism {
		m.logger.Warn("peerlink: TopicRoot differs from the source's; both roots are filtered", "peer", p.nodeID,
			"own", own, "source", ok.TopicRoot)
	}
	p.retainedClassMismatch.Store(ok.RetainedClass != m.deps.RetainedClass)
	_ = conn.SetDeadline(time.Time{})
	return hs, nil
}

// refuse sends GOAWAY(code) to the source and returns the matching error.
func (p *puller) refuse(conn net.Conn, code wire.GoAwayCode, reason string) error {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = wire.WriteFrame(conn, &wire.GoAway{Code: code})
	return &goAwayError{code: code, reason: reason, local: true}
}

// maxRecordAccept is HELLO.maxRecordBytes: the largest record this node accepts.
func (p *puller) maxRecordAccept() uint32 {
	limit := p.m.cfg.Receive.GetMaxFrameBytes() - wire.FrameSlack
	if c := p.m.maxRecordCap; c > 0 && c < limit {
		limit = c
	}
	return clampU32(limit)
}

func (p *puller) fetchFrame(id uint32, offset, commit uint64, snapshot bool) *wire.Fetch {
	cfg := p.m.cfg.Fetch
	f := &wire.Fetch{
		FetchID:    id,
		LingerMs:   clampU16(cfg.LingerMs),
		Offset:     offset,
		Commit:     commit,
		MaxRecords: clampU32(cfg.GetMaxRecords()),
		MaxBytes:   clampU32(cfg.GetMaxBytes()),
		MinRecords: 1,
		MaxWaitMs:  clampU32(max(cfg.GetMaxWaitMs(), config.PeerLinkMinFetchWaitMs)),
	}
	if snapshot {
		f.Flags = wire.FetchFlagSnapshot
		f.LingerMs = 0
		f.MaxWaitMs = 0
	}
	return f
}

func (p *puller) keepAlive() time.Duration {
	return time.Duration(p.m.cfg.GetKeepAliveSeconds()) * time.Second
}

// readBatch reads one BATCH body with the progress deadline of 9.9. Other frames are handled:
// PONG updates the RTT, GOAWAY ends the session, unknown types are skipped.
func (p *puller) readBatch(conn net.Conn, fr *wire.FrameReader, onHeader func(h *wire.BatchHeader)) (*batchIn, error) {
	maxWait := time.Duration(max(p.m.cfg.Fetch.GetMaxWaitMs(), config.PeerLinkMinFetchWaitMs)) * time.Millisecond
	keep := p.keepAlive()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(maxWait + keep))
		t, n, err := fr.ReadHeader()
		if err != nil {
			if errors.Is(err, wire.ErrFrameTooLarge) || errors.Is(err, wire.ErrFrameEmpty) {
				return nil, &goAwayError{code: wire.GoAwayProtocol, reason: err.Error(), local: true}
			}
			return nil, err
		}
		fr.Progress = func() { _ = conn.SetReadDeadline(time.Now().Add(keep)) }
		switch t {
		case wire.FrameBatch:
			recv := time.Now()
			if n < wire.BatchHeaderLen {
				return nil, &goAwayError{code: wire.GoAwayProtocol, reason: "short BATCH", local: true}
			}
			body := make([]byte, n)
			if err := fr.ReadBody(body[:wire.BatchHeaderLen]); err != nil {
				return nil, err
			}
			in := &batchIn{recvAt: recv}
			_ = in.b.Decode(body[:wire.BatchHeaderLen])
			if onHeader != nil {
				onHeader(&in.b.Header)
			}
			if err := fr.ReadBody(body[wire.BatchHeaderLen:]); err != nil {
				return nil, err
			}
			fr.Progress = nil
			if err := in.b.Decode(body); err != nil {
				return nil, &goAwayError{code: wire.GoAwayProtocol, reason: err.Error(), local: true}
			}
			return in, nil
		case wire.FramePong:
			body, err := fr.Body(n)
			if err != nil {
				return nil, err
			}
			var pg wire.Pong
			if pg.Decode(body) == nil {
				if sent := int64(pg.Token); sent > 0 {
					rtt := time.Now().UnixMicro() - sent
					if rtt >= 0 {
						old := p.rttUs.Load()
						p.rttUs.Store((old*7 + rtt) / 8)
					}
				}
			}
		case wire.FrameGoAway:
			body, err := fr.Body(n)
			if err != nil {
				return nil, err
			}
			var g wire.GoAway
			_ = g.Decode(body)
			return nil, &goAwayError{code: g.Code, reason: g.Reason}
		default:
			if t.Known() {
				return nil, &goAwayError{code: wire.GoAwayProtocol, reason: "unexpected " + t.String(), local: true}
			}
			if err := fr.Discard(n); err != nil {
				return nil, err
			}
		}
		fr.Progress = nil
	}
}

// checkCRC classifies a CRC failure (9.7): the batch is poison on the third consecutive failure at
// the same base offset, otherwise the link is reset.
func (p *puller) checkCRC(in *batchIn) error {
	if in.b.CRCValid() {
		if in.b.Header.BaseOffset == p.crcBase {
			p.crcFails = 0
		}
		return nil
	}
	p.crcErrors.Add(1)
	if in.b.Header.BaseOffset == p.crcBase && p.crcFails > 0 {
		p.crcFails++
	} else {
		p.crcBase, p.crcFails = in.b.Header.BaseOffset, 1
	}
	if p.crcFails >= 3 && in.b.Header.Flags&wire.BatchFlagSnapshot == 0 {
		in.poison = true
		p.crcFails = 0
		return nil
	}
	return &goAwayError{code: wire.GoAwayProtocol, reason: "batch CRC mismatch", local: true}
}

// snapshotPhase pulls the retained snapshot (16.5) synchronously before streaming. A PING goes out
// every KeepAlive/2 meanwhile, so neither a slow snapshot build on the source nor a slow apply here
// trips the other side's read deadline; readBatch consumes the PONGs.
func (p *puller) snapshotPhase(ctx context.Context, conn net.Conn, hs *handshakeState, ac *applyCtx) error {
	p.snaps.Add(1)
	keep := p.keepAlive()
	var wmu sync.Mutex
	write := func(f wire.Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(keep))
		return wire.WriteFrame(conn, f)
	}
	goAway := func(code wire.GoAwayCode) {
		wmu.Lock()
		defer wmu.Unlock()
		p.sendGoAway(conn, code)
	}
	stopPing := make(chan struct{})
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	go func() {
		defer pingWG.Done()
		t := time.NewTicker(max(keep/2, 100*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if write(&wire.Ping{Token: uint64(time.Now().UnixMicro())}) != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(stopPing)
		pingWG.Wait()
	}()

	ac.present = nil
	if ac.mode == snapFill || ac.mode == snapNewer {
		ac.present = p.preloadPresent(ctx)
	}
	defer func() { ac.present = nil }()

	var id uint32
	for {
		if ctx.Err() != nil || p.stopping() {
			return errStopped
		}
		id++
		if err := write(p.fetchFrame(id, 0, 0, true)); err != nil {
			return err
		}
		in, err := p.readBatch(conn, hs.fr, nil)
		if err != nil {
			var ge *goAwayError
			if errors.As(err, &ge) && ge.local {
				goAway(ge.code)
			}
			return err
		}
		if err := p.checkCRC(in); err != nil {
			goAway(wire.GoAwayProtocol)
			return err
		}
		h := in.b.Header
		if h.Flags&wire.BatchFlagSnapshot == 0 {
			goAway(wire.GoAwayProtocol)
			return &goAwayError{code: wire.GoAwayProtocol, reason: "expected a SNAPSHOT batch", local: true}
		}
		ac.skewMs = p.clockSkewMs.Load()
		p.applyBatch(ac, in)
		if h.Flags&wire.BatchFlagSnapshotEnd != 0 {
			p.snapTrunc.Add(h.Lost)
			p.flush()
			p.m.logger.Info("peerlink: retained snapshot applied", "peer", p.nodeID,
				"filled", p.snapFilled.Load(), "skippedPresent", p.snapSkipped.Load(), "newer", p.snapNewer.Load(),
				"truncated", h.Lost)
			return nil
		}
	}
}

// preloadPresent reads the local retained topic set once per snapshot (16.5 consumer step 1), so
// the presence and age checks cost no store round trip per record. MEMORY mode checks the engine
// map directly (nil result).
func (p *puller) preloadPresent(ctx context.Context) map[string]int64 {
	if p.m.deps.Retained == nil || p.m.deps.RetainedClass == wire.RetainedMemory {
		return nil
	}
	present := make(map[string]int64)
	err := p.m.retained.Snapshot(ctx, func(pk packets.Packet) bool {
		if len(pk.Payload) > 0 {
			present[pk.TopicName] = pk.Created
		}
		return true
	})
	if err != nil {
		p.m.logger.Warn("peerlink: reading the local retained topics for the snapshot failed; checking per topic",
			"peer", p.nodeID, "error", err)
		return nil
	}
	return present
}

// flush writes the batched retained replicas of this source (13.4). A failure is counted; the
// values stay pending in StorageHook and are written by the next flush.
func (p *puller) flush() {
	if err := p.m.retained.FlushReplicas(p.nodeID); err != nil {
		p.flushErrors.Add(1)
		if ok, n := p.rate.allow("flush", 10*time.Second); ok {
			p.m.logger.Warn("peerlink: writing retained replicas failed; they are retried with the next batch",
				"peer", p.nodeID, "error", err, "suppressed", n)
		}
	}
}

// skewWarnMs is the clock difference to a source above which a WARN recommends NTP (K13): the
// wall clock dates retained values from snapshots and the archive rows of replicas.
const skewWarnMs = 1000

func (p *puller) warnSkew(skewMs int64) {
	if skewMs > -skewWarnMs && skewMs < skewWarnMs {
		return
	}
	if ok, n := p.rate.allow("skew", 10*time.Minute); ok {
		p.m.logger.Warn("peerlink: clock of the source differs from this node; synchronise both with NTP",
			"peer", p.nodeID, "clockSkewMs", skewMs, "suppressed", n)
	}
}

func (p *puller) sendGoAway(conn net.Conn, code wire.GoAwayCode) {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = wire.WriteFrame(conn, &wire.GoAway{Code: code})
}

// writer commands.
type writeCmd struct {
	kind   int
	offset uint64
	goAway bool // cmdFinal: also send GOAWAY(shutdown)
}

const (
	cmdFetch = iota
	cmdCommit
	cmdFinal
)

// stream runs the three goroutines of 11.1: reader, injector and writer.
func (p *puller) stream(ctx context.Context, cancel context.CancelFunc, conn net.Conn, hs *handshakeState, ac *applyCtx) sessionResult {
	pipeline := min(max(p.m.cfg.Fetch.GetPipeline(), 1), 2)
	handoff := make(chan *batchIn, pipeline)
	cmds := make(chan writeCmd, 16)
	var outstanding atomic.Int32
	var lastFetchSent atomic.Int64
	var res sessionResult
	var resMu sync.Mutex
	setErr := func(err error) {
		resMu.Lock()
		if res.err == nil && res.code == 0 {
			var ge *goAwayError
			if errors.As(err, &ge) {
				res.code = ge.code
				res.configErr = ge.code.ConfigError()
				if ge.code == wire.GoAwayOffsetOutOfRange {
					p.epoch.Store(0)
					p.appliedNext.Store(0)
				}
			}
			res.err = err
		}
		resMu.Unlock()
	}
	send := func(c writeCmd) bool {
		select {
		case cmds <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var wg sync.WaitGroup
	writerDone := make(chan struct{})
	wg.Add(3)
	// writer: the only goroutine writing to the connection.
	go func() {
		defer wg.Done()
		defer close(writerDone)
		keep := p.keepAlive()
		ping := time.NewTicker(max(keep/2, 100*time.Millisecond))
		defer ping.Stop()
		var fetchID uint32
		var lastCommit uint64
		write := func(f wire.Frame) bool {
			_ = conn.SetWriteDeadline(time.Now().Add(keep))
			if err := wire.WriteFrame(conn, f); err != nil {
				setErr(err)
				cancel()
				return false
			}
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				if outstanding.Load() == 0 {
					if !write(&wire.Ping{Token: uint64(time.Now().UnixMicro())}) {
						return
					}
				}
			case c := <-cmds:
				applied := p.appliedNext.Load()
				switch c.kind {
				case cmdFetch:
					fetchID++
					commit := uint64(0)
					if applied > lastCommit {
						commit, lastCommit = applied, applied
					}
					outstanding.Add(1)
					lastFetchSent.Store(time.Now().UnixMilli())
					if !write(p.fetchFrame(fetchID, c.offset, commit, false)) {
						return
					}
				case cmdCommit:
					off := c.offset
					if off > lastCommit {
						lastCommit = off
						if !write(&wire.Commit{Commit: off}) {
							return
						}
					}
				case cmdFinal:
					if applied > lastCommit {
						if !write(&wire.Commit{Commit: applied}) {
							return
						}
					}
					if c.goAway {
						write(&wire.GoAway{Code: wire.GoAwayShutdown})
					}
					return
				}
			}
		}
	}()

	// reader: read, CRC, decode, hand off.
	go func() {
		defer wg.Done()
		defer cancel()
		next := p.appliedNext.Load()
		if !send(writeCmd{kind: cmdFetch, offset: next}) {
			return
		}
		for {
			var headerNext uint64
			early := false
			in, err := p.readBatch(conn, hs.fr, func(h *wire.BatchHeader) {
				outstanding.Add(-1)
				headerNext = next
				if h.Flags&wire.BatchFlagSnapshot == 0 && h.BaseOffset != 0 {
					headerNext = h.BaseOffset + uint64(h.Count)
				}
				if pipeline > 1 && h.Flags&wire.BatchFlagSnapshot == 0 {
					early = send(writeCmd{kind: cmdFetch, offset: headerNext})
				}
			})
			if err != nil {
				if ctx.Err() == nil {
					var ge *goAwayError
					if errors.As(err, &ge) && ge.local {
						p.sendGoAway(conn, ge.code)
					}
					setErr(err)
				}
				return
			}
			h := &in.b.Header
			if h.Flags&wire.BatchFlagSnapshot != 0 {
				p.sendGoAway(conn, wire.GoAwayProtocol)
				setErr(&goAwayError{code: wire.GoAwayProtocol, reason: "unexpected SNAPSHOT batch", local: true})
				return
			}
			if err := p.checkCRC(in); err != nil {
				p.sendGoAway(conn, wire.GoAwayProtocol)
				setErr(err)
				return
			}
			if tsend := lastFetchSent.Load(); tsend > 0 {
				trecv := in.recvAt.UnixMilli()
				rttMs := p.rttUs.Load() / 1000
				if trecv-tsend <= 2*rttMs+20 {
					skew := h.SourceWallMs - (tsend+trecv)/2
					old := p.clockSkewMs.Load()
					est := (old*7 + skew) / 8
					p.clockSkewMs.Store(est)
					p.warnSkew(est)
				}
			}
			p.lastSeenLeo.Store(h.Leo)
			p.batches.Add(1)
			if h.Lost > 0 {
				p.gapLost.Add(h.Lost)
				if ok, n := p.rate.allow("gap", 10*time.Second); ok {
					p.m.logger.Warn("peerlink: records lost (source log overflow)", "peer", p.nodeID, "lost", h.Lost,
						"baseOffset", h.BaseOffset, "suppressed", n)
				}
			}
			next = headerNext
			select {
			case handoff <- in:
			case <-ctx.Done():
				return
			}
			if !early && !send(writeCmd{kind: cmdFetch, offset: next}) {
				return
			}
		}
	}()

	// injector: apply in offset order, flush retained writes, commit.
	go func() {
		defer wg.Done()
		ac.commitFn = func(off uint64) {
			p.flush()
			if off > p.appliedNext.Load() {
				p.appliedNext.Store(off)
			}
			select {
			case cmds <- writeCmd{kind: cmdCommit, offset: off}:
			default:
			}
		}
		finish := func(final bool) {
			p.flush()
			if send(writeCmd{kind: cmdFinal, goAway: final}) {
				select {
				case <-writerDone:
				case <-time.After(2 * time.Second):
				}
			}
			cancel()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stopCh:
				if p.graceful.Load() {
					finish(true)
				} else {
					cancel()
				}
				return
			case <-p.kick:
				resMu.Lock()
				res.resync = true
				resMu.Unlock()
				finish(false)
				return
			case in := <-handoff:
				ac.rttHalf = p.rttUs.Load() / 2000
				ac.skewMs = p.clockSkewMs.Load()
				p.pace.observe(in.b.Header.Leo, in.b.Header.SourceMonoMs)
				next := p.applyBatch(ac, in)
				p.flush()
				if next > p.appliedNext.Load() {
					p.appliedNext.Store(next)
					resMu.Lock()
					res.applied = true
					resMu.Unlock()
					if !send(writeCmd{kind: cmdCommit, offset: next}) {
						return
					}
				}
			}
		}
	}()

	wg.Wait()
	_ = conn.Close()
	resMu.Lock()
	defer resMu.Unlock()
	return res
}

func (p *puller) status() SourceStatus {
	st := SourceStatus{
		NodeID:                 p.nodeID,
		Address:                p.address,
		State:                  stateNames[p.state.Load()],
		Epoch:                  p.epoch.Load(),
		AppliedNext:            p.appliedNext.Load(),
		SourceLeo:              p.lastSeenLeo.Load(),
		Batches:                p.batches.Load(),
		Injected:               p.injected.Load(),
		RetainOnly:             p.retainOnly.Load(),
		AppliedBytes:           p.appliedBytes.Load(),
		DupSkipped:             p.dupSkipped.Load(),
		Dropped:                make(map[string]uint64, numDropReasons),
		RetainedDiverged:       make(map[string]uint64, numDivReasons),
		Rejected:               p.rejected.Load(),
		UnknownProps:           p.unknownProps.Load(),
		GapLostTotal:           p.gapLost.Load(),
		SourceResets:           p.sourceResets.Load(),
		ResetLostLowerBound:    p.resetLost.Load(),
		Reconnects:             p.reconnects.Load(),
		Sessions:               p.sessions.Load(),
		CRCErrors:              p.crcErrors.Load(),
		SnapshotFilled:         p.snapFilled.Load(),
		SnapshotSkippedPresent: p.snapSkipped.Load(),
		SnapshotNewer:          p.snapNewer.Load(),
		SnapshotTruncated:      p.snapTrunc.Load(),
		Snapshots:              p.snaps.Load(),
		SnapshotsInterrupted:   p.snapInterrupted.Load(),
		RetainedFlushErrors:    p.flushErrors.Load(),
		SupersededWillResent:   p.willResent.Load(),
		Paced:                  p.paced.Load(),
		ClockSkewMs:            p.clockSkewMs.Load(),
		RTTMs:                  float64(p.rttUs.Load()) / 1000,
		TopicRootMismatch:      p.topicRootMismatch.Load(),
		RetainedClassMismatch:  p.retainedClassMismatch.Load(),
		OARetained:             p.oaRetained.Load(),
		ApplyDelayMs:           ApplyDelay{P50: p.hist.quantile(0.5), P99: p.hist.quantile(0.99), P999: p.hist.quantile(0.999)},
	}
	if st.SourceLeo > st.AppliedNext {
		st.LagRecords = st.SourceLeo - st.AppliedNext
	}
	for i := range p.dropped {
		st.Dropped[dropNames[i]] = p.dropped[i].Load()
	}
	for i := range p.diverged {
		st.RetainedDiverged[divNames[i]] = p.diverged[i].Load()
	}
	p.lastErrMu.Lock()
	st.LastError = p.lastErr
	p.lastErrMu.Unlock()
	return st
}
