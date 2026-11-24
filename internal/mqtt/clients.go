package mqtt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/xid"

	"monstermq.io/edge/internal/mqtt/packets"
)

const (
	defaultKeepalive             uint16 = 10 // the default connection keepalive value in seconds.
	defaultClientProtocolVersion byte   = 4  // the default mqtt protocol version of connecting clients (if somehow unspecified).
	minimumKeepalive             uint16 = 5  // the minimum recommended keepalive - values under with display a warning.

	readSlabSize = 16 * 1024 // PUBLISH bodies up to half of this are cut from a shared per-connection slab
)

var (
	ErrMinimumKeepalive = errors.New("client keepalive is below minimum recommended value and may exhibit connection instability")
)

// ReadFn is the function signature for the function used for reading and processing new packets.
type ReadFn func(*Client, packets.Packet) error

// Clients contains a map of the clients known by the broker.
type Clients struct {
	internal map[string]*Client // clients known by the broker, keyed on client id.
	snap     atomic.Pointer[map[string]*Client]
	sync.RWMutex
}

// NewClients returns an instance of Clients.
func NewClients() *Clients {
	c := &Clients{
		internal: make(map[string]*Client),
	}
	empty := make(map[string]*Client)
	c.snap.Store(&empty)
	return c
}

func (cl *Clients) refreshSnapLocked() {
	m := make(map[string]*Client, len(cl.internal))
	for k, v := range cl.internal {
		m[k] = v
	}
	cl.snap.Store(&m)
}

// Add adds a new client to the clients map, keyed on client id.
func (cl *Clients) Add(val *Client) {
	cl.Lock()
	cl.internal[val.ID] = val
	cl.refreshSnapLocked()
	cl.Unlock()
}

// GetAll returns all the clients.
func (cl *Clients) GetAll() map[string]*Client {
	cl.RLock()
	defer cl.RUnlock()
	m := make(map[string]*Client, len(cl.internal))
	for k, v := range cl.internal {
		m[k] = v
	}
	return m
}

// Get returns the value of a client if it exists.
func (cl *Clients) Get(id string) (*Client, bool) {
	if s := cl.snap.Load(); s != nil {
		val, ok := (*s)[id]
		return val, ok
	}
	cl.RLock()
	defer cl.RUnlock()
	val, ok := cl.internal[id]
	return val, ok
}

// Len returns the length of the clients map.
func (cl *Clients) Len() int {
	if s := cl.snap.Load(); s != nil {
		return len(*s)
	}
	cl.RLock()
	defer cl.RUnlock()
	return len(cl.internal)
}

// Delete removes a client from the internal map.
func (cl *Clients) Delete(id string) {
	cl.Lock()
	delete(cl.internal, id)
	cl.refreshSnapLocked()
	cl.Unlock()
}

// DeleteIfCurrent removes the client with the given id only if it is still
// the registered one, so that a stale connection ending late cannot remove
// the connection that took over its client ID. It reports whether the
// entry was removed.
func (cl *Clients) DeleteIfCurrent(val *Client) bool {
	cl.Lock()
	defer cl.Unlock()
	if cur, ok := cl.internal[val.ID]; !ok || cur != val {
		return false
	}
	delete(cl.internal, val.ID)
	cl.refreshSnapLocked()
	return true
}

// GetByListener returns clients matching a listener id.
func (cl *Clients) GetByListener(id string) []*Client {
	cl.RLock()
	defer cl.RUnlock()
	clients := make([]*Client, 0, len(cl.internal))
	for _, client := range cl.internal {
		if client.Net.Listener == id && !client.Closed() {
			clients = append(clients, client)
		}
	}
	return clients
}

// Client contains information about a client known by the broker.
type Client struct {
	Properties   ClientProperties // client properties
	State        ClientState      // the operational state of the client.
	Net          ClientConnection // network connection state of the client
	ID           string           // the client id.
	ops          *ops             // ops provides a reference to server ops.
	sync.RWMutex                  // mutex
}

// ClientConnection contains the connection transport and metadata for the client.
type ClientConnection struct {
	Conn     net.Conn      // the net.Conn used to establish the connection
	bconn    *bufio.Reader // a buffered net.Conn for reading packets
	readSlab []byte        // slab for PUBLISH bodies, see allocReadBuf
	Remote   string        // the remote address of the client
	Listener string        // listener id of the client
	Inline   bool          // if true, the client is the built-in 'inline' embedded client
}

// ClientProperties contains the properties which define the client behaviour.
type ClientProperties struct {
	Props           packets.Properties
	Will            Will
	Username        []byte
	ProtocolVersion byte
	Clean           bool
}

// Will contains the last will and testament details for a client connection.
type Will struct {
	Payload           []byte                 // -
	User              []packets.UserProperty // -
	ContentType       string                 // MQTT 5 will property, forwarded with the will message
	ResponseTopic     string                 // MQTT 5 will property, forwarded with the will message
	CorrelationData   []byte                 // MQTT 5 will property, forwarded with the will message
	MessageExpiry     uint32                 // MQTT 5 Message Expiry Interval of the will message
	PayloadFormat     byte                   // MQTT 5 Payload Format Indicator of the will message
	PayloadFormatFlag bool                   // PayloadFormat was set
	TopicName         string                 // -
	Flag              uint32                 // 0,1
	WillDelayInterval uint32                 // -
	Qos               byte                   // -
	Retain            bool                   // -
}

// ClientState tracks the state of the client.
type ClientState struct {
	TopicAliases      TopicAliases       // a map of topic aliases
	stopCause         atomic.Value       // reason for stopping
	Inflight          *Inflight          // a map of in-flight qos messages
	Subscriptions     *Subscriptions     // a map of the subscription filters a client maintains
	disconnected      atomic.Int64       // the time the client disconnected in unix time, for calculating expiry
	disconnectedNano  atomic.Int64       // the time the client disconnected in unix nanoseconds
	outbound          *outboundQueue     // live publishes for the writer; dropped when full
	control           chan writeItem     // every other packet for the writer; never dropped
	endOnce           sync.Once          // only end once
	isTakenOver       atomic.Bool        // used to identify orphaned clients
	established       chan struct{}      // closed once the connection reads packets after CONNECT handling
	sessionReleased   atomic.Bool        // set once the OnDisconnect hooks ran for the ended connection
	claimMu           sync.Mutex         // keeps claiming a deferred packet and queueing it for the writer in order
	establishedOnce   sync.Once          // closes established once
	packetIDMu        sync.Mutex         // guards NextPacketID without contending with network write lock
	packetID          uint32             // the current highest packetID
	open              context.Context    // indicate that the client is open for packet exchange
	cancelOpen        context.CancelFunc // cancel function for open context
	writeOverloaded   atomic.Bool        // true while outbound queue is dropping packets
	droppedInOverload atomic.Int64       // packets dropped during current overload window
	MetricClientIn    *atomic.Int64      // cached metrics collector counter for inbound messages
	MetricClientOut   *atomic.Int64      // cached metrics collector counter for outbound messages
	lastDeadline      atomic.Int64       // last unix second and keepalive applied via SetDeadline
	Keepalive         uint16             // the number of seconds the connection can wait
	ServerKeepalive   bool               // keepalive was set by the server
}

// newClient returns a new instance of Client. This is almost exclusively used by Server
// for creating new clients, but it lives here because it's not dependent.
func newClient(c net.Conn, o *ops) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	cl := &Client{
		State: ClientState{
			Inflight:      NewInflights(),
			Subscriptions: NewSubscriptions(),
			TopicAliases:  NewTopicAliases(o.options.Capabilities.TopicAliasMaximum),
			open:          ctx,
			cancelOpen:    cancel,
			Keepalive:     defaultKeepalive,
		},
		Properties: ClientProperties{
			ProtocolVersion: defaultClientProtocolVersion, // default protocol version
		},
		ops: o,
	}

	// Clients without a connection (inline clients, sessions restored from
	// storage) never run a writer, so they get no queues.
	if c != nil {
		cl.Net = ClientConnection{
			Conn:   c,
			bconn:  bufio.NewReaderSize(c, o.options.ClientNetReadBufferSize),
			Remote: c.RemoteAddr().String(),
		}
		cl.State.outbound = newOutboundQueue(int(o.options.Capabilities.MaximumClientWritesPending))
		cl.State.established = make(chan struct{})
		cl.State.control = make(chan writeItem, controlQueueSize)
	}

	return cl
}

// ParseConnect parses the connect parameters and properties for a client.
func (cl *Client) ParseConnect(lid string, pk packets.Packet) {
	cl.Net.Listener = lid

	cl.Properties.ProtocolVersion = pk.ProtocolVersion
	cl.Properties.Username = pk.Connect.Username
	cl.Properties.Clean = pk.Connect.Clean
	cl.Properties.Props = pk.Properties.Copy(false)

	if cl.Properties.Props.ReceiveMaximum > cl.ops.options.Capabilities.MaximumInflight { // 3.3.4 Non-normative
		cl.Properties.Props.ReceiveMaximum = cl.ops.options.Capabilities.MaximumInflight
	}

	if pk.Connect.Keepalive <= minimumKeepalive {
		cl.ops.log.Warn(
			ErrMinimumKeepalive.Error(),
			"client", cl.ID,
			"keepalive", pk.Connect.Keepalive,
			"recommended", minimumKeepalive,
		)
	}

	cl.State.Keepalive = pk.Connect.Keepalive                                              // [MQTT-3.2.2-22]
	cl.State.Inflight.ResetReceiveQuota(int32(cl.ops.options.Capabilities.ReceiveMaximum)) // server receive max per client
	cl.State.Inflight.ResetSendQuota(int32(cl.Properties.Props.ReceiveMaximum))            // client receive max

	cl.ID = pk.Connect.ClientIdentifier
	if cl.ID == "" {
		cl.ID = xid.New().String() // [MQTT-3.1.3-6] [MQTT-3.1.3-7]
		cl.Properties.Props.AssignedClientID = cl.ID
	}

	if pk.Connect.WillFlag {
		cl.Properties.Will = Will{
			Qos:               pk.Connect.WillQos,
			Retain:            pk.Connect.WillRetain,
			Payload:           pk.Connect.WillPayload,
			TopicName:         pk.Connect.WillTopic,
			WillDelayInterval: pk.Connect.WillProperties.WillDelayInterval,
			User:              pk.Connect.WillProperties.User,
			ContentType:       pk.Connect.WillProperties.ContentType,
			ResponseTopic:     pk.Connect.WillProperties.ResponseTopic,
			CorrelationData:   pk.Connect.WillProperties.CorrelationData,
			MessageExpiry:     pk.Connect.WillProperties.MessageExpiryInterval,
			PayloadFormat:     pk.Connect.WillProperties.PayloadFormat,
			PayloadFormatFlag: pk.Connect.WillProperties.PayloadFormatFlag,
		}
		if pk.Properties.SessionExpiryIntervalFlag &&
			pk.Properties.SessionExpiryInterval < pk.Connect.WillProperties.WillDelayInterval {
			cl.Properties.Will.WillDelayInterval = pk.Properties.SessionExpiryInterval
		}
		if pk.Connect.WillFlag {
			cl.Properties.Will.Flag = 1 // atomic for checking
		}
	}
}

// refreshDeadline refreshes the read/write deadline for the net.Conn connection.
func (cl *Client) refreshDeadline(keepalive uint16) {
	if cl.Net.Conn == nil {
		return
	}
	if keepalive == 0 {
		if cl.State.lastDeadline.Swap(-1) != -1 {
			_ = cl.Net.Conn.SetDeadline(time.Time{}) // [MQTT-3.1.2-22]
		}
		return
	}
	now := time.Now()
	stamp := (now.Unix() << 16) | int64(keepalive)
	if cl.State.lastDeadline.Load() == stamp {
		return
	}
	cl.State.lastDeadline.Store(stamp)
	expiry := now.Add(time.Duration(keepalive+(keepalive/2)) * time.Second) // [MQTT-3.1.2-22]
	_ = cl.Net.Conn.SetDeadline(expiry)                                     // [MQTT-3.1.2-22]
}

// NextPacketID returns the next available (unused) packet id for the client.
// If no unused packet ids are available, an error is returned and the client
// should be disconnected.
func (cl *Client) NextPacketID() (i uint32, err error) {
	cl.State.packetIDMu.Lock()
	defer cl.State.packetIDMu.Unlock()

	i = atomic.LoadUint32(&cl.State.packetID)
	started := i
	overflowed := false
	for {
		if overflowed && i == started {
			return 0, packets.ErrQuotaExceeded
		}

		if i >= cl.ops.options.Capabilities.maximumPacketID {
			overflowed = true
			i = 0
			continue
		}

		i++

		if _, ok := cl.State.Inflight.Get(uint16(i)); !ok {
			atomic.StoreUint32(&cl.State.packetID, i)
			return i, nil
		}
	}
}

// ResendInflightMessages attempts to resend any pending inflight messages to connected clients.
func (cl *Client) ResendInflightMessages(force bool) error {
	if cl.State.Inflight.Len() == 0 {
		return nil
	}

	for _, tk := range cl.State.Inflight.GetAll(false) {
		if tk.FixedHeader.Type == packets.Pubrec {
			// Inbound QoS 2 flow waiting for the client's PUBREL: on reconnect only
			// PUBLISH and PUBREL are re-sent [MQTT-4.4.0-1]; the stored PUBREC is
			// kept so the PUBREL from the client is answered with PUBCOMP.
			continue
		}
		if tk.FixedHeader.Type == packets.Publish {
			if !cl.ops.hooks.OnACLCheck(cl, tk.TopicName, false) {
				if ok := cl.State.Inflight.Delete(tk.PacketID); ok {
					cl.ops.hooks.OnQosDropped(cl, tk)
					atomic.AddInt64(&cl.ops.info.Inflight, -1)
				}
				continue
			}
			tk.FixedHeader.Dup = true // [MQTT-3.3.1-1] [MQTT-3.3.1-3]
		}

		cl.ops.hooks.OnQosPublish(cl, tk, tk.Created, 0)
		err := cl.WritePacket(tk)
		if err != nil {
			return err
		}

		if tk.FixedHeader.Type == packets.Puback || tk.FixedHeader.Type == packets.Pubcomp {
			if ok := cl.State.Inflight.Delete(tk.PacketID); ok {
				cl.ops.hooks.OnQosComplete(cl, tk)
				atomic.AddInt64(&cl.ops.info.Inflight, -1)
			}
		}
	}

	return nil
}

// claimDeferred takes the oldest QoS 1/2 message that was deferred because the
// client's send quota was exhausted, if quota is available again.
// writeNextDeferred claims the oldest packet waiting for send quota, if quota
// is available, and queues it for the writer. Claim and queueing happen under
// one lock so that concurrent callers cannot reorder deferred packets.
func (cl *Client) writeNextDeferred() {
	if !cl.State.Inflight.HasDeferred() {
		return
	}
	cl.State.claimMu.Lock()
	defer cl.State.claimMu.Unlock()
	if next, ok := cl.claimDeferred(); ok {
		_ = cl.WritePacket(next)
	}
}

func (cl *Client) claimDeferred() (packets.Packet, bool) {
	if !cl.State.Inflight.HasDeferred() {
		return packets.Packet{}, false
	}
	return cl.State.Inflight.ClaimNextImmediate(func(p packets.Packet) int64 {
		if expiry := minimum(cl.ops.options.Capabilities.MaximumMessageExpiryInterval,
			int64(p.Properties.MessageExpiryInterval)); expiry > 0 {
			base := p.Created
			if base <= 0 {
				base = time.Now().Unix()
			}
			return base + expiry
		}
		return 0
	})
}

// ClearInflights deletes all inflight messages for the client, e.g. for a disconnected user with a clean session.
func (cl *Client) ClearInflights() {
	for _, tk := range cl.State.Inflight.GetAll(false) {
		if ok := cl.State.Inflight.Delete(tk.PacketID); ok {
			cl.ops.hooks.OnQosDropped(cl, tk)
			atomic.AddInt64(&cl.ops.info.Inflight, -1)
		}
	}
}

// ClearExpiredInflights deletes any inflight messages which have expired.
func (cl *Client) ClearExpiredInflights(now, maximumExpiry int64) []uint16 {
	deleted := []uint16{}
	for _, tk := range cl.State.Inflight.GetAll(false) {
		expired := tk.ProtocolVersion == 5 && tk.Expiry > 0 && tk.Expiry < now // [MQTT-3.3.2-5]

		// If the maximum message expiry interval is set (greater than 0), and the message
		// retention period exceeds the maximum expiry, the message will be forcibly removed.
		enforced := maximumExpiry > 0 && now-tk.Created > maximumExpiry

		if expired || enforced {
			if ok := cl.State.Inflight.Delete(tk.PacketID); ok {
				cl.ops.hooks.OnQosDropped(cl, tk)
				atomic.AddInt64(&cl.ops.info.Inflight, -1)
				deleted = append(deleted, tk.PacketID)
			}
		}
	}

	return deleted
}

// Read reads incoming packets from the connected client and transforms them into
// packets to be handled by the packetHandler.
func (cl *Client) Read(packetHandler ReadFn) error {
	var err error
	var fh packets.FixedHeader

	for {
		if cl.Closed() {
			return nil
		}

		cl.refreshDeadline(cl.State.Keepalive)
		fh = packets.FixedHeader{}
		err = cl.ReadFixedHeader(&fh)
		if err != nil {
			return keepaliveErr(err)
		}

		pk, err := cl.ReadPacket(&fh)
		if err != nil {
			return keepaliveErr(err)
		}

		err = packetHandler(cl, pk) // Process inbound packet.
		if err != nil {
			return err
		}
	}
}

// keepaliveErr reports an expired read deadline as a keepalive timeout [MQTT-3.1.2-22].
func keepaliveErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return packets.ErrKeepAliveTimeout
	}
	return err
}

// Stop instructs the client to shut down all processing goroutines and disconnect.
func (cl *Client) Stop(err error) {
	cl.State.endOnce.Do(func() {

		if cl.Net.Conn != nil {
			_ = cl.Net.Conn.Close() // omit close error
		}

		if err != nil {
			cl.State.stopCause.Store(err)
		}

		if cl.State.cancelOpen != nil {
			cl.State.cancelOpen()
		}

		now := time.Now()
		cl.State.disconnectedNano.Store(now.UnixNano())
		cl.State.disconnected.Store(now.Unix())
	})
}

// sessionExpired reports whether the session of a disconnected client has
// outlived its Session Expiry Interval (or maxExpiry, the server maximum,
// for clients without one) at now [MQTT-3.1.2-23].
func (cl *Client) sessionExpired(now time.Time, maxExpiry uint32) bool {
	stopped := cl.State.disconnectedNano.Load()
	if stopped == 0 {
		if sec := cl.State.disconnected.Load(); sec > 0 {
			stopped = sec * int64(time.Second) // restored from storage: seconds only
		} else {
			return false
		}
	}
	expire := maxExpiry
	if cl.Properties.ProtocolVersion == 5 && cl.Properties.Props.SessionExpiryIntervalFlag {
		expire = cl.Properties.Props.SessionExpiryInterval
	}
	return now.UnixNano()-stopped >= int64(expire)*int64(time.Second)
}

// StopCause returns the reason the client connection was stopped, if any.
func (cl *Client) StopCause() error {
	if cl.State.stopCause.Load() == nil {
		return nil
	}
	return cl.State.stopCause.Load().(error)
}

// StopTime returns the the time the client disconnected in unix time, else zero.
func (cl *Client) StopTime() int64 {
	return cl.State.disconnected.Load()
}

// Closed returns true if client connection is closed.
func (cl *Client) Closed() bool {
	return cl.State.open == nil || cl.State.open.Err() != nil
}

// markEstablished records that the connection finished its CONNECT handling
// (CONNACK, session hooks) and now reads packets, or that it ended before.
func (cl *Client) markEstablished() {
	if cl.State.established == nil {
		return
	}
	cl.State.establishedOnce.Do(func() { close(cl.State.established) })
}

// settleBeforeTakeover is called when a new connection takes over cl. If cl
// has not finished its CONNECT handling yet, its client may already have sent
// a DISCONNECT (possibly changing the Session Expiry Interval) that nobody has
// read: wait until cl reads packets again and give it a short moment to
// process such a pending DISCONNECT, so the takeover sees the final session
// state [MQTT-3.14.4-3].
func (cl *Client) settleBeforeTakeover(establishTimeout, grace time.Duration) {
	if cl.State.established == nil || cl.State.open == nil {
		return
	}
	select {
	case <-cl.State.established:
		return // already reading: anything it sent before was handled in order
	default:
	}
	timer := time.NewTimer(establishTimeout)
	defer timer.Stop()
	select {
	case <-cl.State.established:
	case <-cl.State.open.Done():
		return
	case <-timer.C:
		return
	}
	graceTimer := time.NewTimer(grace)
	defer graceTimer.Stop()
	select {
	case <-cl.State.open.Done():
	case <-graceTimer.C:
	}
}

func (cl *Client) IsTakenOver() bool {
	return cl.State.isTakenOver.Load()
}

// ReadFixedHeader reads in the values of the next packet's fixed header.
func (cl *Client) ReadFixedHeader(fh *packets.FixedHeader) error {
	if cl.Net.bconn == nil {
		return ErrConnectionClosed
	}

	b, err := cl.Net.bconn.ReadByte()
	if err != nil {
		return err
	}

	err = fh.Decode(b)
	if err != nil {
		return err
	}

	var bu int
	fh.Remaining, bu, err = packets.DecodeLength(cl.Net.bconn)
	if err != nil {
		return err
	}

	if cl.ops.options.Capabilities.MaximumPacketSize > 0 && uint32(1+bu+fh.Remaining) > cl.ops.options.Capabilities.MaximumPacketSize {
		return packets.ErrPacketTooLarge // [MQTT-3.2.2-15]
	}

	atomic.AddInt64(&cl.ops.info.BytesReceived, int64(bu+1))
	return nil
}

// allocReadBuf returns a buffer for the body of the next packet. PUBLISH bodies
// are cut from a per-connection slab; decoded topics and payloads point into it,
// so anything that keeps them long-term must copy them. A region of the slab is
// never rewritten (a new slab is allocated instead), which is what makes the
// zero-copy decode safe. Every other packet type
// gets its own buffer: subscription filters, client IDs, usernames and wills
// live as long as the session and would otherwise keep a whole slab alive.
func (cl *Client) allocReadBuf(packetType byte, n int) []byte {
	if n == 0 {
		return nil
	}
	if packetType != packets.Publish || n > readSlabSize/2 {
		return make([]byte, n)
	}
	if len(cl.Net.readSlab) < n {
		cl.Net.readSlab = make([]byte, readSlabSize)
	}
	b := cl.Net.readSlab[:n:n]
	cl.Net.readSlab = cl.Net.readSlab[n:]
	return b
}

// releaseReadBuffers drops the read buffer and slab once the connection has
// ended. A client with a persistent session stays in Server.Clients until the
// session expires, and must not keep them that long.
func (cl *Client) releaseReadBuffers() {
	cl.Net.bconn = nil
	cl.Net.readSlab = nil
}

// ReadPacket reads the remaining buffer into an MQTT packet.
func (cl *Client) ReadPacket(fh *packets.FixedHeader) (pk packets.Packet, err error) {
	atomic.AddInt64(&cl.ops.info.PacketsReceived, 1)

	pk.ProtocolVersion = cl.Properties.ProtocolVersion // inherit client protocol version for decoding
	pk.FixedHeader = *fh
	p := cl.allocReadBuf(pk.FixedHeader.Type, pk.FixedHeader.Remaining)
	if len(p) > 0 {
		n, err := io.ReadFull(cl.Net.bconn, p)
		if err != nil {
			return pk, err
		}
		atomic.AddInt64(&cl.ops.info.BytesReceived, int64(n))
	}

	// p is freshly allocated for this packet, so decoded fields may safely reference it;
	// no further copy is needed to protect it from the next read.
	px := p
	switch pk.FixedHeader.Type {
	case packets.Connect:
		err = pk.ConnectDecode(px)
	case packets.Disconnect:
		err = pk.DisconnectDecode(px)
	case packets.Connack:
		err = pk.ConnackDecode(px)
	case packets.Publish:
		err = pk.PublishDecode(px)
		if err == nil {
			atomic.AddInt64(&cl.ops.info.MessagesReceived, 1)
		}
	case packets.Puback:
		err = pk.PubackDecode(px)
	case packets.Pubrec:
		err = pk.PubrecDecode(px)
	case packets.Pubrel:
		err = pk.PubrelDecode(px)
	case packets.Pubcomp:
		err = pk.PubcompDecode(px)
	case packets.Subscribe:
		err = pk.SubscribeDecode(px)
	case packets.Suback:
		err = pk.SubackDecode(px)
	case packets.Unsubscribe:
		err = pk.UnsubscribeDecode(px)
	case packets.Unsuback:
		err = pk.UnsubackDecode(px)
	case packets.Pingreq:
	case packets.Pingresp:
	case packets.Auth:
		err = pk.AuthDecode(px)
	default:
		err = fmt.Errorf("invalid packet type; %v", pk.FixedHeader.Type)
	}

	if err != nil {
		return pk, err
	}

	pk, err = cl.ops.hooks.OnPacketRead(cl, pk)
	return
}
