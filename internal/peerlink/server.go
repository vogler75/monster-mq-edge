package peerlink

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/peerlink/wire"
	"monstermq.io/edge/internal/tlsutil"
)

const (
	sniffTimeout     = 3 * time.Second
	handshakeTimeout = 10 * time.Second
	ioBufferSize     = 64 << 10
	takeoverWindow   = 60 * time.Second
	duplicateRefusal = 5 * time.Minute
	maxFetchRecords  = 1 << 16
	maxFetchWait     = 60 * time.Second
)

// consumerSlot is the source-side state of one configured consumer (a Serve peer). The log keeps
// its committed and served offsets; the slot keeps the session and the per-consumer counters.
type consumerSlot struct {
	idx     int
	nodeID  string
	peer    config.PeerConfig
	tlsPeer tlsutil.Peer
	secrets [][]byte

	mu           sync.Mutex
	active       *session
	takeovers    []takeover
	refused      map[uint64]time.Time
	remote       string
	authFailures map[string]uint64
	warnedRoot   bool
	warnedClass  bool
	// logConnected: the log state of this consumer is CONNECTED. Changed only under mu together
	// with active, so a superseded or failed session never leaves a stale state behind.
	logConnected bool

	lastFetch             atomic.Int64 // unix ms
	sessions              atomic.Uint64
	servedRecords         atomic.Uint64
	servedBytes           atomic.Uint64
	servedSkipped         atomic.Uint64
	snapshotServed        atomic.Uint64
	duplicateConsumer     atomic.Uint64
	shutdownUnserved      atomic.Uint64
	oaRetained            atomic.Bool
	topicRootMismatch     atomic.Bool
	retainedClassMismatch atomic.Bool
}

type takeover struct {
	at       time.Time
	from, to uint64
}

func (c *consumerSlot) status(ls LogConsumerStats) ConsumerStatus {
	cs := ConsumerStatus{
		NodeID:                c.nodeID,
		State:                 ls.State.String(),
		Committed:             ls.Committed,
		Served:                ls.Served,
		Lag:                   ls.Lag,
		LostTotal:             ls.LostTotal,
		ServedRecords:         c.servedRecords.Load(),
		ServedBytes:           c.servedBytes.Load(),
		ServedSkipped:         map[string]uint64{"size": c.servedSkipped.Load()},
		SnapshotServed:        c.snapshotServed.Load(),
		Sessions:              c.sessions.Load(),
		DuplicateConsumer:     c.duplicateConsumer.Load(),
		ShutdownUnserved:      c.shutdownUnserved.Load(),
		OARetained:            c.oaRetained.Load(),
		TopicRootMismatch:     c.topicRootMismatch.Load(),
		RetainedClassMismatch: c.retainedClassMismatch.Load(),
		AuthFailures:          map[string]uint64{},
	}
	if ms := c.lastFetch.Load(); ms > 0 {
		cs.LastFetch = time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
	}
	c.mu.Lock()
	cs.Remote = c.remote
	for k, v := range c.authFailures {
		cs.AuthFailures[k] = v
	}
	c.mu.Unlock()
	return cs
}

// admit installs sess as the active session (9.5). It returns the superseded session, or dup when
// the takeover pattern shows two live processes using this NodeId; the active session then stays.
func (c *consumerSlot) admit(sess *session, now time.Time) (old *session, dup bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, until := range c.refused {
		if now.After(until) {
			delete(c.refused, id)
		}
	}
	if _, ok := c.refused[sess.instance]; ok {
		return nil, true
	}
	if c.active != nil {
		c.takeovers = append(c.takeovers, takeover{at: now, from: c.active.instance, to: sess.instance})
		keep := c.takeovers[:0]
		for _, t := range c.takeovers {
			if now.Sub(t.at) <= takeoverWindow {
				keep = append(keep, t)
			}
		}
		c.takeovers = keep
		if alternating(c.takeovers) {
			c.refused[sess.instance] = now.Add(duplicateRefusal)
			c.duplicateConsumer.Add(1)
			c.takeovers = c.takeovers[:0]
			return nil, true
		}
		old = c.active
	}
	c.active = sess
	c.remote = sess.remote
	return old, false
}

// alternating reports at least three takeovers between exactly two distinct instance ids.
func alternating(ts []takeover) bool {
	n := 0
	var a, b uint64
	for _, t := range ts {
		if t.from == t.to {
			continue
		}
		for _, id := range [2]uint64{t.from, t.to} {
			switch {
			case a == 0 || a == id:
				a = id
			case b == 0 || b == id:
				b = id
			default:
				return false
			}
		}
		n++
	}
	return n >= 3 && a != 0 && b != 0
}

// release clears sess as the active session and sets the log state DISCONNECTED when it was
// CONNECTED. It reports whether sess was active; a superseded session changes nothing.
func (c *consumerSlot) release(lg *Log, sess *session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != sess {
		return false
	}
	c.active = nil
	if c.logConnected {
		c.logConnected = false
		lg.SetConsumerState(c.idx, LogConsumerDisconnected)
	}
	return true
}

// connected sets the log state CONNECTED while sess is still the active session.
func (c *consumerSlot) connected(lg *Log, sess *session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != sess {
		return false
	}
	c.logConnected = true
	lg.SetConsumerState(c.idx, LogConsumerConnected)
	return true
}

func (c *consumerSlot) countAuthFailure(code wire.GoAwayCode) {
	c.mu.Lock()
	c.authFailures[code.String()]++
	c.mu.Unlock()
}

// session is one authenticated consumer connection on the source.
type session struct {
	m        *Manager
	slot     *consumerSlot
	conn     net.Conn
	tcp      *net.TCPConn // plaintext: writev target
	br       *bufio.Reader
	bw       *bufio.Writer // TLS writes
	remote   string
	instance uint64
	caps     uint64
	maxRec   uint32 // consumer's HELLO.maxRecordBytes, 0 = unlimited
	keep     time.Duration
	snapOK   bool

	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
	fetches   chan wire.Fetch

	waiter *LogWaiter
	frames [][]byte
	bufs   net.Buffers
	prefix [wire.BatchPrefixLen]byte
	snap   *snapshotState
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (m *Manager) acceptLoop() {
	for {
		c, err := m.listener.Accept()
		if err != nil {
			if m.closed.Load() || m.drained.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			m.logger.Warn("peerlink: accept failed", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Add under sessMu, so closeSessions (which takes sessMu before Wait) never races an Add.
		if !m.trackConn(c) {
			_ = c.Close()
			continue
		}
		go func() {
			defer m.connWG.Done()
			defer m.untrackConn(c)
			m.handleConn(c)
		}()
	}
}

func remoteIP(c net.Conn) net.IP {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func (m *Manager) allowedIP(ip net.IP) bool {
	if len(m.allowedNets) == 0 {
		return true
	}
	if ip == nil {
		return false
	}
	for _, n := range m.allowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// admissionKey is the per-IP pre-auth key: the address for IPv4, its /64 for IPv6, so one host
// with a prefix cannot take every slot.
func admissionKey(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if ip.To4() == nil && len(ip) == net.IPv6len {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

// preAuthAcquire takes a pre-auth slot. IPs of configured peer Addresses and IPs that completed a
// handshake recently bypass the global cap, so idle connections from other hosts cannot lock a
// peer out (9.2).
func (m *Manager) preAuthAcquire(ip net.IP, key string) bool {
	m.admMu.Lock()
	defer m.admMu.Unlock()
	if m.preAuthIP[key] >= m.maxPerIP {
		return false
	}
	ipStr := ip.String()
	if m.preAuthAll >= maxPreAuthTotal && !m.peerIPs[ipStr] && !m.authedRecent(ipStr) {
		return false
	}
	m.preAuthIP[key]++
	m.preAuthAll++
	return true
}

// authedIPTTL is how long an IP that completed a handshake bypasses the global pre-auth cap.
const authedIPTTL = 24 * time.Hour

func (m *Manager) authedRecent(ip string) bool {
	at, ok := m.authedIPs[ip]
	return ok && time.Since(at) < authedIPTTL
}

// rememberAuthed records the IP of an authenticated session. The set is bounded: entries past
// their TTL go first, then the oldest.
func (m *Manager) rememberAuthed(ip string) {
	m.admMu.Lock()
	defer m.admMu.Unlock()
	if _, ok := m.authedIPs[ip]; !ok && len(m.authedIPs) >= maxAuthedIPs {
		var oldest string
		var oldestAt time.Time
		for k, at := range m.authedIPs {
			if time.Since(at) >= authedIPTTL {
				delete(m.authedIPs, k)
				continue
			}
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		if len(m.authedIPs) >= maxAuthedIPs {
			delete(m.authedIPs, oldest)
		}
	}
	m.authedIPs[ip] = time.Now()
}

func (m *Manager) preAuthRelease(ip string) {
	m.admMu.Lock()
	if m.preAuthIP[ip] <= 1 {
		delete(m.preAuthIP, ip)
	} else {
		m.preAuthIP[ip]--
	}
	m.preAuthAll--
	m.admMu.Unlock()
}

// handleConn runs admission, sniffing and the optional TLS handshake on an accepted connection
// (plan 9.2), then hands it to the peer protocol or the status endpoint.
func (m *Manager) handleConn(c net.Conn) {
	ip := remoteIP(c)
	if !m.allowedIP(ip) {
		m.adm.refusedNetwork.Add(1)
		_ = c.Close()
		return
	}
	ipKey := admissionKey(ip)
	if !m.preAuthAcquire(ip, ipKey) {
		m.adm.refusedBusy.Add(1)
		_ = c.Close()
		return
	}
	var once sync.Once
	release := func() { once.Do(func() { m.preAuthRelease(ipKey) }) }
	defer release()
	m.adm.accepted.Add(1)

	tcp, _ := c.(*net.TCPConn)
	if tcp != nil {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(15 * time.Second)
	}
	br := bufio.NewReaderSize(c, ioBufferSize)
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	first, err := br.Peek(1)
	if err != nil {
		_ = c.Close()
		return
	}
	switch first[0] {
	case 0x16:
		if m.tls.server == nil {
			m.adm.refusedSniff.Add(1)
			_ = c.Close()
			return
		}
		tc := tls.Server(&bufConn{Conn: c, r: br}, m.tls.server)
		ctx, cancel := context.WithTimeout(m.ctx, handshakeTimeout)
		err := tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			m.adm.tlsFailures.Add(1)
			_ = c.Close()
			return
		}
		cs := tc.ConnectionState()
		if cs.NegotiatedProtocol == tlsutil.ALPNHTTP {
			if _, err := m.tls.trust.FindPeer(cs.PeerCertificates, m.servePeers(), x509.ExtKeyUsageClientAuth); err != nil {
				m.adm.refusedHTTP.Add(1)
				_ = tc.Close()
				return
			}
			// The pre-auth slot stays taken while the request is served: it bounds the
			// concurrent status connections like any unauthenticated connection.
			m.serveHTTP(tc, bufio.NewReader(tc), false)
			return
		}
		m.servePeer(tc, nil, bufio.NewReaderSize(tc, ioBufferSize), &cs, release)
	case 'M':
		if m.tls.server != nil && !m.cfg.Listener.AllowPlaintext {
			m.adm.refusedPlaintext.Add(1)
			_ = c.Close()
			return
		}
		m.servePeer(c, tcp, br, nil, release)
	case 'G', 'P':
		if ip == nil || !ip.IsLoopback() {
			m.adm.refusedHTTP.Add(1)
			_ = c.Close()
			return
		}
		m.serveHTTP(c, br, true)
	default:
		m.adm.refusedSniff.Add(1)
		_ = c.Close()
	}
}

func (m *Manager) trackConn(c net.Conn) bool {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	if m.closed.Load() || m.drained.Load() {
		return false
	}
	if m.conns == nil {
		m.conns = make(map[net.Conn]struct{})
	}
	m.conns[c] = struct{}{}
	m.connWG.Add(1)
	return true
}

func (m *Manager) untrackConn(c net.Conn) {
	m.sessMu.Lock()
	delete(m.conns, c)
	m.sessMu.Unlock()
}

func (m *Manager) servePeers() []tlsutil.Peer {
	out := make([]tlsutil.Peer, 0, len(m.consumers))
	for _, c := range m.consumers {
		out = append(out, c.tlsPeer)
	}
	return out
}

// helloResult is the outcome of the HELLO checks (9.5).
type helloResult struct {
	slot      *consumerSlot
	secretIdx int
	code      wire.GoAwayCode
	reason    string
}

// checkHello runs the source-side HELLO checks of 9.5 in order; the first failure wins.
func (m *Manager) checkHello(h *wire.Hello, cs *tls.ConnectionState, nonceS *[wire.NonceLen]byte, exporter []byte) helloResult {
	r := helloResult{secretIdx: -1}
	cid, err := config.CanonicalNodeID(h.ConsumerNodeID)
	if err != nil {
		r.code, r.reason = wire.GoAwayUnknownPeer, "invalid consumer NodeId"
		return r
	}
	if cid == m.nodeID {
		r.code, r.reason = wire.GoAwaySelfConnection, "consumer NodeId equals the source NodeId"
		return r
	}
	slot := m.consumerByID[cid]
	if slot == nil {
		if _, ok := m.peerByID[cid]; ok {
			r.code, r.reason = wire.GoAwayNotAllowed, "peer has Serve: false"
		} else {
			r.code, r.reason = wire.GoAwayUnknownPeer, "consumer NodeId is not a configured peer"
		}
		return r
	}
	r.slot = slot
	if exp, err := config.CanonicalNodeID(h.ExpectedSourceNodeID); err != nil || exp != m.nodeID {
		r.code, r.reason = wire.GoAwayWrongNode, fmt.Sprintf("consumer expected source %q", h.ExpectedSourceNodeID)
		return r
	}
	certAuth := false
	if cs != nil && len(cs.PeerCertificates) > 0 {
		if err := m.tls.trust.Verify(cs.PeerCertificates, slot.tlsPeer, x509.ExtKeyUsageClientAuth); err != nil {
			r.code, r.reason = wire.GoAwayIdentityMismatch, "client certificate does not match the consumer NodeId"
			return r
		}
		certAuth = true
	} else if slot.peer.Tls.RequireClientCert {
		r.code, r.reason = wire.GoAwayIdentityMismatch, "client certificate required"
		return r
	}
	if len(slot.secrets) > 0 && cs == nil {
		// A secret cannot be bound to a plaintext connection, and the waiver must not turn a peer
		// that has a secret into one that needs none.
		r.code, r.reason = wire.GoAwayAuthFailed, "shared secret configured; plaintext not allowed"
		return r
	}
	macAuth := false
	if len(slot.secrets) > 0 && cs != nil {
		if h.Flags&wire.HelloFlagMAC == 0 {
			r.code, r.reason = wire.GoAwayAuthFailed, "shared secret configured but no MAC"
			return r
		}
		in := wire.ConsumerMACInput(nonceS, &h.NonceC, cid, m.nodeID, exporter)
		r.secretIdx = wire.MatchMAC(slot.secrets, in, &h.MAC)
		if r.secretIdx < 0 {
			r.code, r.reason = wire.GoAwayAuthFailed, "MAC mismatch"
			return r
		}
		macAuth = true
	}
	if !certAuth && !macAuth && !m.cfg.AllowUnauthenticatedPeers {
		r.code, r.reason = wire.GoAwayAuthFailed, "peer not authenticated"
		return r
	}
	return r
}

// writeRaw writes a frame during the handshake, before the session's writer exists.
func writeRaw(c net.Conn, f wire.Frame) error {
	_ = c.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	return wire.WriteFrame(c, f)
}

// servePeer runs the source handshake and then the session. cs is nil on plaintext.
func (m *Manager) servePeer(conn net.Conn, tcp *net.TCPConn, br *bufio.Reader, cs *tls.ConnectionState, releasePreAuth func()) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	major, _, err := wire.ReadPreamble(br)
	if err != nil {
		return
	}
	reasonFor := func(reason string) string {
		if m.authConfigured {
			return ""
		}
		return reason
	}
	if major != wire.VersionMajor {
		_ = writeRaw(conn, &wire.GoAway{Code: wire.GoAwayVersion, Reason: reasonFor(fmt.Sprintf("major version %d not supported", major))})
		return
	}
	tlsLink := cs != nil
	ownCaps := m.ownCaps(tlsLink)
	sh := wire.ServerHello{VersionMajor: wire.VersionMajor, VersionMinor: wire.VersionMinor, Capabilities: ownCaps, NonceS: wire.NewNonce()}
	if tlsLink && m.tls.clientAuth != tlsutil.ClientAuthNone {
		sh.AuthModes |= wire.AuthClientCertRequested
	}
	for _, c := range m.consumers {
		if len(c.secrets) > 0 {
			sh.AuthModes |= wire.AuthSharedSecret
			break
		}
	}
	if err := writeRaw(conn, &sh); err != nil {
		return
	}
	fr := wire.NewFrameReader(br, wire.MaxPreAuthFrame)
	t, body, err := fr.ReadFrame()
	if err != nil {
		if errors.Is(err, wire.ErrFrameTooLarge) {
			_ = writeRaw(conn, &wire.GoAway{Code: wire.GoAwayProtocol})
		}
		return
	}
	if t == wire.FrameGoAway {
		return
	}
	var hello wire.Hello
	if t != wire.FrameHello || hello.Decode(body) != nil {
		_ = writeRaw(conn, &wire.GoAway{Code: wire.GoAwayProtocol, Reason: reasonFor("expected HELLO")})
		return
	}
	var exporter []byte
	if tlsLink {
		exporter, err = cs.ExportKeyingMaterial(wire.ExporterLabel, nil, wire.ExporterLen)
		if err != nil {
			exporter = nil
		}
	}
	remote := conn.RemoteAddr().String()
	hr := m.checkHello(&hello, cs, &sh.NonceS, exporter)
	if hr.code != 0 {
		m.refuse(conn, hr, remote, hello.ConsumerNodeID)
		return
	}
	slot := hr.slot

	sess := &session{
		m: m, slot: slot, conn: conn, tcp: tcp, br: br, remote: remote, instance: hello.InstanceID,
		caps: ownCaps & hello.Capabilities, maxRec: hello.MaxRecordBytes,
		keep:    time.Duration(m.cfg.GetKeepAliveSeconds()) * time.Second,
		fetches: make(chan wire.Fetch, 2),
		waiter:  NewLogWaiter(),
	}
	if tcp == nil {
		sess.bw = bufio.NewWriterSize(conn, ioBufferSize)
	}
	sess.ctx, sess.cancel = context.WithCancel(m.ctx)
	defer sess.cancel()

	old, dup := slot.admit(sess, time.Now())
	if dup {
		m.logger.Error("peerlink: duplicate consumer NodeId: two processes alternate on one NodeId; refusing the newer instance for 5 min",
			"peer", slot.nodeID, "remote", remote, "activeRemote", slot.remoteOfActive())
		slot.countAuthFailure(wire.GoAwayDuplicateNode)
		_ = writeRaw(conn, &wire.GoAway{Code: wire.GoAwayDuplicateNode, Reason: "duplicate NodeId"})
		return
	}
	if old != nil {
		old.goAway(wire.GoAwaySuperseded, "a newer session took over")
	}
	if !m.registerSession(sess) {
		slot.release(m.log, sess)
		_ = writeRaw(conn, &wire.GoAway{Code: wire.GoAwayShutdown})
		return
	}
	defer m.unregisterSession(sess)

	resume, err := m.log.Resume(slot.idx, hello.LastEpoch, hello.ResumeOffset)
	if err != nil {
		slot.release(m.log, sess)
		_ = writeRaw(conn, &wire.GoAway{Code: wire.GoAwayOffsetOutOfRange, Reason: "resume offset beyond log end"})
		return
	}

	oaRet := oaRetainedFor(m.deps.RetainedClass, m.oaSystem, hello.RetainedClass, hello.OASystem)
	slot.oaRetained.Store(oaRet)
	root := m.namespaceRoot()
	slot.topicRootMismatch.Store(root != "" && hello.TopicRoot != "" && root != hello.TopicRoot)
	slot.retainedClassMismatch.Store(hello.RetainedClass != m.deps.RetainedClass)
	m.handshakeWarnings(slot, &hello, root, oaRet)

	sess.snapOK = sess.caps&(wire.CapSnapshotFill|wire.CapResyncNewer) != 0 && m.retained != nil
	now := time.Now()
	ok := wire.HelloOK{
		Capabilities:   sess.caps,
		Epoch:          m.log.Epoch(),
		ResumeAt:       resume.ResumeAt,
		LogStart:       resume.LSO,
		Leo:            resume.LEO,
		Committed:      resume.Committed,
		LostOnResume:   resume.LostOnResume,
		WallNowMs:      now.UnixMilli(),
		MonoNowMs:      m.log.MonoMs(now),
		MaxRecordBytes: uint32(m.log.MaxRecordBytes()),
		RetainedClass:  m.deps.RetainedClass,
		SourceNodeID:   m.nodeID,
		TopicRoot:      root,
		OASystem:       m.oaSystem,
	}
	if resume.SourceReset {
		ok.Flags |= wire.HelloOKSourceReset
	}
	if resume.ConsumerStateUsed {
		ok.Flags |= wire.HelloOKConsumerStateUsed
	}
	if (hello.LastEpoch == 0 || resume.SourceReset) && sess.caps&wire.CapSnapshotFill != 0 && !oaRet && sess.snapOK {
		ok.Flags |= wire.HelloOKSnapshotAvailable
	}
	if hr.secretIdx >= 0 {
		ok.MACS = wire.MAC(slot.secrets[hr.secretIdx], wire.SourceMACInput(&hello.NonceC, &sh.NonceS, m.nodeID, slot.nodeID, exporter))
	}
	if err := writeRaw(conn, &ok); err != nil {
		slot.release(m.log, sess)
		return
	}
	releasePreAuth()
	_ = conn.SetDeadline(time.Time{})
	if ip := remoteIP(conn); ip != nil {
		m.rememberAuthed(ip.String())
	}

	if !slot.connected(m.log, sess) {
		return // superseded meanwhile
	}
	slot.sessions.Add(1)
	m.logger.Info("peerlink: consumer connected", "peer", slot.nodeID, "remote", remote, "tls", tlsLink,
		"resumeAt", resume.ResumeAt, "lostOnResume", resume.LostOnResume, "sourceReset", resume.SourceReset,
		"oaRetained", oaRet)
	if resume.LostOnResume > 0 {
		m.logger.Warn("peerlink: consumer resumes after a gap", "peer", slot.nodeID, "lostOnResume", resume.LostOnResume)
	}
	m.stateChanged()

	sess.run()

	if slot.release(m.log, sess) {
		m.logger.Info("peerlink: consumer disconnected", "peer", slot.nodeID, "remote", remote)
		m.stateChanged()
	}
}

func (c *consumerSlot) remoteOfActive() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		return c.active.remote
	}
	return ""
}

func (m *Manager) refuse(conn net.Conn, hr helloResult, remote, claimed string) {
	if hr.slot != nil {
		hr.slot.countAuthFailure(hr.code)
	}
	m.authFailMu.Lock()
	m.authFailAll[hr.code.String()]++
	m.authFailMu.Unlock()
	// The claimed id is unauthenticated: it never becomes a rate-limiter key, only a truncated field.
	peer := "unknown"
	if hr.slot != nil {
		peer = hr.slot.nodeID
	}
	if len(claimed) > 64 {
		claimed = claimed[:64] + "..."
	}
	if ok, n := m.rate.allow("refuse:"+peer+":"+hr.code.String(), 10*time.Second); ok {
		m.logger.Error("peerlink: handshake refused", "claimedNodeId", claimed, "remote", remote,
			"code", hr.code.String(), "reason", hr.reason, "suppressed", n)
	}
	ga := wire.GoAway{Code: hr.code, Reason: hr.reason}
	if m.authConfigured {
		ga = wire.GoAway{Code: wire.GoAwayAuthFailed}
	}
	_ = writeRaw(conn, &ga)
}

func (m *Manager) handshakeWarnings(slot *consumerSlot, h *wire.Hello, root string, oaRet bool) {
	slot.mu.Lock()
	warnRoot := slot.topicRootMismatch.Load() && !slot.warnedRoot
	if warnRoot {
		slot.warnedRoot = true
	}
	warnClass := slot.retainedClassMismatch.Load() && !slot.warnedClass
	if warnClass {
		slot.warnedClass = true
	}
	slot.mu.Unlock()
	if warnRoot {
		m.logger.Warn("peerlink: TopicRoot differs from the consumer's", "peer", slot.nodeID, "own", root, "consumer", h.TopicRoot)
	}
	if warnClass {
		m.logger.Warn("peerlink: retained store class differs from the consumer's", "peer", slot.nodeID,
			"own", m.deps.RetainedClass.String(), "consumer", h.RetainedClass.String())
	}
	if oaRet {
		m.logger.Info("peerlink: retained store replicated by WinCC OA on this link (oaRetained)", "peer", slot.nodeID, "oaSystem", m.oaSystem)
	}
}

func (m *Manager) registerSession(s *session) bool {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	if m.closed.Load() || m.drained.Load() {
		return false
	}
	m.sessions[s] = struct{}{}
	return true
}

func (m *Manager) unregisterSession(s *session) {
	m.sessMu.Lock()
	delete(m.sessions, s)
	m.sessMu.Unlock()
}

// goAway sends GOAWAY (best effort, short deadline) and closes the session.
func (s *session) goAway(code wire.GoAwayCode, reason string) {
	s.closeOnce.Do(func() {
		s.cancel()
		// A batch write in progress holds the lock until its deadline; then just close.
		if s.writeMu.TryLock() {
			_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			b := (&wire.GoAway{Code: code, Reason: reason}).AppendFrame(nil)
			if s.bw != nil {
				_, _ = s.bw.Write(b)
				_ = s.bw.Flush()
			} else {
				_, _ = s.conn.Write(b)
			}
			s.writeMu.Unlock()
		}
		_ = s.conn.Close()
	})
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.conn.Close()
	})
}

func (s *session) run() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.readLoop()
	}()
	s.serveLoop()
	s.close()
	<-done
	clear(s.frames[:cap(s.frames)])
}

// readLoop reads consumer frames: FETCH (queued for the serve loop, its commit applied at once),
// COMMIT, PING and GOAWAY. The read deadline is 3 x KeepAliveSeconds per frame.
func (s *session) readLoop() {
	defer s.cancel()
	fr := wire.NewFrameReader(s.br, wire.MaxConsumerFrame)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(3 * s.keep))
		t, n, err := fr.ReadHeader()
		if err != nil {
			if errors.Is(err, wire.ErrFrameTooLarge) || errors.Is(err, wire.ErrFrameEmpty) {
				s.goAway(wire.GoAwayProtocol, "frame exceeds the size cap")
			}
			return
		}
		body, err := fr.Body(n)
		if err != nil {
			return
		}
		switch t {
		case wire.FrameFetch:
			var f wire.Fetch
			if f.Decode(body) != nil {
				s.goAway(wire.GoAwayProtocol, "short FETCH")
				return
			}
			s.slot.lastFetch.Store(time.Now().UnixMilli())
			s.m.log.ObserveConsumer(s.slot.idx)
			if f.Commit != 0 && !s.commit(f.Commit) {
				return
			}
			select {
			case s.fetches <- f:
			case <-s.ctx.Done():
				return
			}
		case wire.FrameCommit:
			var c wire.Commit
			if c.Decode(body) != nil {
				s.goAway(wire.GoAwayProtocol, "short COMMIT")
				return
			}
			if !s.commit(c.Commit) {
				return
			}
		case wire.FramePing:
			var p wire.Ping
			if p.Decode(body) != nil {
				s.goAway(wire.GoAwayProtocol, "short PING")
				return
			}
			if s.writeFrame(&wire.Pong{Token: p.Token}) != nil {
				return
			}
		case wire.FrameGoAway:
			var g wire.GoAway
			_ = g.Decode(body)
			s.m.logger.Info("peerlink: consumer sent GOAWAY", "peer", s.slot.nodeID, "code", g.Code.String(), "reason", g.Reason)
			return
		case wire.FrameServerHello, wire.FrameHello, wire.FrameHelloOK, wire.FrameBatch, wire.FramePong:
			s.goAway(wire.GoAwayProtocol, "unexpected "+t.String())
			return
		default:
			// Unknown frame types of the same major version are ignored (9.3).
		}
	}
}

func (s *session) commit(off uint64) bool {
	if err := s.m.log.Commit(s.slot.idx, off); err != nil {
		s.goAway(wire.GoAwayProtocol, "commit beyond log end")
		return false
	}
	return true
}

func (s *session) writeFrame(f wire.Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.keep))
	b := f.AppendFrame(nil)
	if s.bw != nil {
		if _, err := s.bw.Write(b); err != nil {
			return err
		}
		return s.bw.Flush()
	}
	_, err := s.conn.Write(b)
	return err
}

func (s *session) serveLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case f := <-s.fetches:
			if err := s.serveFetch(&f); err != nil {
				return
			}
		}
	}
}

func fetchLimits(f *wire.Fetch) (maxRecords, maxBytes, minRecords int, maxWait time.Duration) {
	maxRecords = int(f.MaxRecords)
	if maxRecords <= 0 {
		maxRecords = 4096
	}
	maxRecords = min(maxRecords, maxFetchRecords)
	maxBytes = int(f.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	minRecords = max(int(f.MinRecords), 1)
	maxWait = min(time.Duration(f.MaxWaitMs)*time.Millisecond, maxFetchWait)
	return
}

// serveFetch answers one FETCH with one BATCH (8.6, 9.7).
func (s *session) serveFetch(f *wire.Fetch) error {
	if f.Flags&wire.FetchFlagSnapshot != 0 {
		return s.serveSnapshot(f)
	}
	lg := s.m.log
	lso, leo := lg.Bounds()
	if f.Offset == 0 || f.Offset > leo {
		s.goAway(wire.GoAwayOffsetOutOfRange, "fetch offset beyond log end")
		return errors.New("offset out of range")
	}
	maxRecords, maxBytes, minRecords, maxWait := fetchLimits(f)
	if f.Offset >= lso && f.Offset+uint64(minRecords) > leo && maxWait > 0 {
		lg.WaitFor(s.ctx, s.waiter, f.Offset+uint64(minRecords), maxWait)
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
	}
	res, err := lg.ReadFor(s.slot.idx, f.Offset, maxRecords, maxBytes, &s.frames)
	if err != nil {
		s.goAway(wire.GoAwayOffsetOutOfRange, "fetch offset beyond log end")
		return err
	}
	if f.LingerMs > 0 && res.Count > 0 && res.Count < maxRecords && res.Lost == 0 && res.Bytes < maxBytes {
		clear(s.frames)
		lg.WaitFor(s.ctx, s.waiter, f.Offset+uint64(maxRecords), time.Duration(f.LingerMs)*time.Millisecond)
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if res, err = lg.ReadFor(s.slot.idx, f.Offset, maxRecords, maxBytes, &s.frames); err != nil {
			s.goAway(wire.GoAwayOffsetOutOfRange, "fetch offset beyond log end")
			return err
		}
	}
	h := wire.BatchHeader{
		FetchID:    f.FetchID,
		BaseOffset: res.Base,
		Count:      uint32(res.Count),
		LogStart:   res.LSO,
		Leo:        res.LEO,
		Lost:       res.Lost,
	}
	if res.Lost > 0 {
		h.Flags |= wire.BatchFlagGap
	}
	if res.Truncated {
		h.Flags |= wire.BatchFlagTruncated
	}
	if res.Count == 0 {
		h.Flags |= wire.BatchFlagEmpty
	}
	skipped := s.substituteTombstones(s.frames)
	err = s.writeBatch(&h, s.frames)
	clear(s.frames)
	if err != nil {
		return err
	}
	s.slot.servedRecords.Add(uint64(res.Count))
	s.slot.servedBytes.Add(uint64(h.RecordsBytes))
	s.slot.servedSkipped.Add(uint64(skipped))
	if res.Count > 0 && s.m.deps.Metrics != nil {
		s.m.deps.Metrics.IncBusOut(res.Count)
	}
	return nil
}

// substituteTombstones replaces records larger than the consumer's maxRecordBytes with tombstones
// (9.7) so asymmetric MaxMessageSize settings cannot stall the link.
func (s *session) substituteTombstones(frames [][]byte) int {
	if s.maxRec == 0 {
		return 0
	}
	n := 0
	for i, f := range frames {
		if len(f) > int(s.maxRec) {
			frames[i] = wire.AppendTombstone(nil, f)
			n++
		}
	}
	return n
}

// writeBatch writes the BATCH prefix and the record frames: one writev on a plaintext TCP
// connection, a buffered write under TLS. The write deadline grows with the batch size (9.9).
func (s *session) writeBatch(h *wire.BatchHeader, frames [][]byte) error {
	total := 0
	for _, f := range frames {
		total += len(f)
	}
	h.RecordsBytes = uint32(total)
	if s.caps&wire.CapBatchCRC != 0 {
		h.Flags |= wire.BatchFlagCRC
	}
	now := time.Now()
	h.SourceMonoMs = s.m.log.MonoMs(now)
	h.SourceWallMs = now.UnixMilli()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wire.EncodeBatchPrefix(&s.prefix, h)
	if h.Flags&wire.BatchFlagCRC != 0 {
		wire.SetBatchCRC(&s.prefix, frames)
	}
	_ = s.conn.SetWriteDeadline(now.Add(s.keep * time.Duration(1+total/ioBufferSize)))
	if s.tcp != nil {
		s.bufs = append(s.bufs[:0], s.prefix[:])
		s.bufs = append(s.bufs, frames...)
		b := s.bufs
		_, err := b.WriteTo(s.tcp)
		clear(s.bufs[:cap(s.bufs)])
		return err
	}
	if _, err := s.bw.Write(s.prefix[:]); err != nil {
		return err
	}
	for _, f := range frames {
		if _, err := s.bw.Write(f); err != nil {
			return err
		}
	}
	return s.bw.Flush()
}
