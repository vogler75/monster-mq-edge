package mqtt

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/mqtt/packets"
)

// Every packet sent to a client is encoded and written by one goroutine per
// connection, WriteLoop. Live publishes arrive on ClientState.outbound, which
// drops packets when full (see outboundQueue). Every other packet (CONNACK, acks, PINGRESP,
// DISCONNECT, inflight resends, deferred and replayed messages) arrives on
// ClientState.control, which never drops. Because the writer is the only
// encoder, encode order is wire order: outbound topic aliases are assigned
// here, and live publishes are held back until the CONNACK has been written.

const (
	controlQueueSize       = 256         // control packets queued per client before WritePacket blocks
	maxBatchPackets        = 256         // packets encoded per batch before the writer checks its state again
	disconnectFlushTimeout = time.Second // how long a DISCONNECT may take to reach the socket before the connection is closed
)

// writeItem is one entry of the control queue.
type writeItem struct {
	pk         *packets.Packet // nil: only flush
	flushed    chan struct{}   // closed once everything queued before it has been written; nil when nobody waits
	closeAfter error           // when set, the writer stops the client with this cause right after writing pk
	live       bool            // pk came from the outbound queue
}

var outboundPacketPool = sync.Pool{
	New: func() any {
		return new(packets.Packet)
	},
}

// connWriter is the state of a connection's writer goroutine.
type connWriter struct {
	cl     *Client
	buf    *bytes.Buffer
	maxBuf int

	connackSent bool              // live publishes are written only after the CONNACK
	aliasMax    uint16            // the client's Topic Alias Maximum
	aliases     map[string]uint16 // aliases the client has been sent on this connection

	failed         bool // a write failed; the client is being stopped
	oversizeLogged bool

	// per-batch settings
	now            int64
	hasEncodeHook  bool
	hasRawSentHook bool
	disallowProb   bool
	alwaysRespInfo bool

	// written since the last flush
	bytes     int64
	packets   int64
	publishes int64
	waiters   []chan struct{}
}

// WriteLoop encodes and writes every packet sent to the client until the
// client is stopped.
func (cl *Client) WriteLoop() {
	w := &connWriter{cl: cl, maxBuf: cl.ops.options.ClientNetWriteBufferSize, buf: new(bytes.Buffer)}
	if w.maxBuf <= 0 {
		w.maxBuf = defaultClientNetBufferSize
	}
	overloadRecoveryThreshold := cl.State.outbound.max / 4
	for {
		var outbound chan struct{} // nil, and never ready, until the CONNACK is written
		if w.connackSent {
			outbound = cl.State.outbound.notify
		}
		select {
		case it := <-cl.State.control:
			w.writeBatch(it)
		case <-outbound:
			if it, ok := w.next(); ok {
				w.writeBatch(it)
			}
		case <-cl.State.open.Done():
			w.releaseWaiters()
			return
		}
		if w.connackSent && cl.State.outbound.len() > 0 {
			cl.State.outbound.signal() // a batch ends at maxBatchPackets; wake up again for the rest
		}
		if cl.State.writeOverloaded.Load() && cl.State.outbound.len() <= overloadRecoveryThreshold {
			if cl.State.writeOverloaded.CompareAndSwap(true, false) {
				dropped := cl.State.droppedInOverload.Swap(0)
				cl.ops.log.Info("client outbound write queue overload cleared", "client", cl.ID, "dropped", dropped)
			}
		}
	}
}

// writeBatch encodes it and whatever else is queued, up to maxBatchPackets,
// flushing whenever the buffer is full and at the end. The buffer is never
// carried into the next batch: a steady stream of packets that are all dropped
// (for their size) would otherwise hold back a small ack indefinitely.
func (w *connWriter) writeBatch(it writeItem) {
	cl := w.cl
	if cl.Closed() || w.failed {
		w.discard(it)
		return
	}

	props := &cl.Properties.Props
	w.now = 0
	w.hasEncodeHook = cl.ops.hooks.Provides(OnPacketEncode)
	w.hasRawSentHook = cl.ops.hooks.HasRawPacketSent()
	w.disallowProb = props.RequestProblemInfoFlag && props.RequestProblemInfo == 0x0
	w.alwaysRespInfo = props.RequestResponseInfo == 0x1 || cl.ops.options.Capabilities.Compatibilities.AlwaysReturnResponseInfo

	for n := 0; ; n++ {
		if it.pk != nil {
			w.encode(it.pk)
			releasePacket(it)
		}
		if it.flushed != nil {
			w.waiters = append(w.waiters, it.flushed)
		}
		if it.closeAfter != nil {
			// Nothing may follow a DISCONNECT or a refused CONNACK [MQTT-3.14.4-1].
			w.flush()
			w.failed = true
			cl.Stop(it.closeAfter)
			return
		}
		if w.failed {
			w.releaseWaiters()
			return
		}
		if it.flushed != nil || w.buf.Len() >= w.maxBuf {
			w.flush()
		}
		if n+1 >= maxBatchPackets {
			break
		}
		var ok bool
		if it, ok = w.next(); !ok {
			break
		}
	}
	w.flush()
}

// next returns the next queued packet without blocking, control packets first.
// A control packet queued before a live publish was pushed is always written
// first: a SUBACK, for example, goes out before the retained messages of its
// subscription. That is why the control queue is checked again after taking a
// live publish, which goes back to the front of its queue if a control packet
// arrived in the meantime.
func (w *connWriter) next() (writeItem, bool) {
	select {
	case it := <-w.cl.State.control:
		return it, true
	default:
	}
	if !w.connackSent {
		return writeItem{}, false
	}
	pk, ok := w.cl.State.outbound.pop()
	if !ok {
		return writeItem{}, false
	}
	select {
	case it := <-w.cl.State.control:
		w.cl.State.outbound.pushFront(pk)
		return it, true
	default:
		return writeItem{pk: pk, live: true}, true
	}
}

// encode appends pk to the batch buffer. When pk is a QoS 1/2 message dropped
// for its size, the send quota it returns goes to the next deferred message,
// which is encoded in its place: nothing else would send it until the client
// sends another packet.
func (w *connWriter) encode(pk *packets.Packet) {
	for pk != nil {
		if !w.encodeOne(pk) {
			return
		}
		next, ok := w.cl.claimDeferred()
		if !ok {
			return
		}
		pk = &next
	}
}

// encodeOne appends pk to the batch buffer. It reports whether pk was a QoS 1/2
// message dropped for its size, which returned its send quota.
func (w *connWriter) encodeOne(pk *packets.Packet) (quotaReturned bool) {
	cl := w.cl
	// Only a publisher's own expiry is forwarded; pk.Expiry also holds the
	// server maximum for messages without one.
	if pk.Expiry > 0 && pk.Properties.MessageExpiryInterval > 0 {
		if w.now == 0 {
			w.now = time.Now().Unix()
		}
		pk.Properties.MessageExpiryInterval = uint32(max(pk.Expiry-w.now, 1)) // [MQTT-3.3.2-6]
	}
	pk.ProtocolVersion = cl.Properties.ProtocolVersion
	if pk.Mods.MaxSize == 0 { // NB we use this statement to embed client packet sizes in tests
		pk.Mods.MaxSize = cl.Properties.Props.MaximumPacketSize
	}
	if w.disallowProb {
		pk.Mods.DisallowProblemInfo = true // [MQTT-3.1.2-29] strict, no problem info on any packet if set
	}
	if pk.FixedHeader.Type != packets.Connack || w.alwaysRespInfo {
		pk.Mods.AllowResponseInfo = true // [MQTT-3.1.2-28] we need to know which properties we can encode
	}

	isPublish := pk.FixedHeader.Type == packets.Publish
	var aliasTopic string
	var newAlias uint16
	if isPublish {
		aliasTopic, newAlias = w.applyAlias(pk)
	}
	if w.hasEncodeHook {
		*pk = cl.ops.hooks.OnPacketEncode(cl, *pk)
	}

	start := w.buf.Len()
	var err error
	if isPublish {
		err = pk.PublishEncode(w.buf)
	} else {
		err = encodeOtherPacket(pk, w.buf)
	}
	if err != nil {
		w.buf.Truncate(start)
		cl.ops.log.Debug("failed encoding packet", "error", err, "client", cl.ID, "type", pk.FixedHeader.Type)
		return false
	}
	size := w.buf.Len() - start
	if pk.Mods.MaxSize > 0 && uint32(size) > pk.Mods.MaxSize { // [MQTT-3.1.2-24]
		w.buf.Truncate(start)
		if !isPublish {
			// Only application messages can be discarded; without its CONNACK or
			// ack the session cannot continue.
			cl.ops.log.Warn("packet exceeds the client's maximum packet size; closing the connection",
				"client", cl.ID, "type", packets.PacketNames[pk.FixedHeader.Type], "size", size, "limit", pk.Mods.MaxSize)
			w.failed = true
			cl.Stop(packets.ErrPacketTooLarge)
			return false
		}
		return w.dropOversize(pk, size)
	}

	if pk.FixedHeader.Type == packets.Connack {
		w.connackSent = true
		if cl.Properties.ProtocolVersion == 5 {
			w.aliasMax = cl.Properties.Props.TopicAliasMaximum
		}
	}
	if newAlias > 0 {
		if w.aliases == nil {
			w.aliases = make(map[string]uint16)
		}
		w.aliases[strings.Clone(aliasTopic)] = newAlias
	}

	w.bytes += int64(size)
	w.packets++
	if isPublish {
		w.publishes++
	}
	if w.hasRawSentHook {
		cl.ops.hooks.OnRawPacketSent(cl, *pk, w.buf.Bytes()[start:])
	}
	return false
}

// applyAlias replaces the topic of a PUBLISH with an alias the client already
// knows, or offers the client a new alias along with the topic. It returns the
// topic and alias to record once the packet is known to be sent, so a packet
// that is dropped registers nothing. Packets are queued and stored in Inflight
// with their full topic; aliases exist only on the wire copy, and only for the
// current connection [MQTT-3.3.2-7].
func (w *connWriter) applyAlias(pk *packets.Packet) (string, uint16) {
	pk.Properties.TopicAlias = 0
	pk.Properties.TopicAliasFlag = false
	if w.aliasMax == 0 || pk.TopicName == "" || pk.ProtocolVersion < 5 {
		return "", 0
	}
	if alias, ok := w.aliases[pk.TopicName]; ok {
		pk.Properties.TopicAlias = alias
		pk.Properties.TopicAliasFlag = true
		pk.TopicName = ""
		return "", 0
	}
	if len(w.aliases) >= int(w.aliasMax) {
		return "", 0
	}
	alias := uint16(len(w.aliases) + 1)
	pk.Properties.TopicAlias = alias
	pk.Properties.TopicAliasFlag = true
	return pk.TopicName, alias
}

// dropOversize handles a packet larger than the client's Maximum Packet Size.
// The packet is discarded, and the broker behaves as if it had been delivered
// [MQTT-3.1.2-25]: a QoS 1/2 message leaves Inflight and returns its send quota,
// instead of staying unacknowledged for the life of the session.
func (w *connWriter) dropOversize(pk *packets.Packet, size int) (quotaReturned bool) {
	cl := w.cl
	if pk.FixedHeader.Type == packets.Publish {
		atomic.AddInt64(&cl.ops.info.MessagesDropped, 1)
		if pk.FixedHeader.Qos > 0 {
			// The packet ID may have been freed and reused since pk was queued.
			if pki, ok := cl.State.Inflight.Get(pk.PacketID); ok && pki.FixedHeader.Type == packets.Publish &&
				pki.Created == pk.Created && pki.TopicName == pk.TopicName {
				if cl.State.Inflight.Delete(pk.PacketID) {
					quotaReturned = cl.State.Inflight.IncreaseSendQuota()
					atomic.AddInt64(&cl.ops.info.Inflight, -1)
					cl.ops.hooks.OnQosComplete(cl, *pk)
				}
			}
		}
	}
	if !w.oversizeLogged {
		w.oversizeLogged = true
		cl.ops.log.Warn("packet exceeds the client's maximum packet size and was dropped",
			"client", cl.ID, "type", packets.PacketNames[pk.FixedHeader.Type], "size", size, "limit", pk.Mods.MaxSize)
	}
	return quotaReturned
}

// flush writes the batch buffer to the connection and wakes anyone waiting for
// it. A failed write stops the client.
func (w *connWriter) flush() {
	cl := w.cl
	if w.buf.Len() > 0 && !w.failed {
		if _, err := w.buf.WriteTo(cl.Net.Conn); err != nil {
			w.failed = true
			cl.Stop(fmt.Errorf("write to client: %w", err))
		} else {
			atomic.AddInt64(&cl.ops.info.BytesSent, w.bytes)
			atomic.AddInt64(&cl.ops.info.PacketsSent, w.packets)
			if w.publishes > 0 {
				atomic.AddInt64(&cl.ops.info.MessagesSent, w.publishes)
				cl.ops.hooks.OnPublishBatchSent(cl, w.publishes)
			}
		}
	}
	w.buf.Reset()
	if w.buf.Cap() > 4*w.maxBuf {
		w.buf = new(bytes.Buffer) // don't keep a buffer grown by one large packet
	}
	w.bytes, w.packets, w.publishes = 0, 0, 0
	w.releaseWaiters()
}

func (w *connWriter) releaseWaiters() {
	for _, ch := range w.waiters {
		close(ch)
	}
	w.waiters = w.waiters[:0]
}

// discard drops an item the writer can no longer send.
func (w *connWriter) discard(it writeItem) {
	if it.pk != nil {
		releasePacket(it)
	}
	if it.flushed != nil {
		close(it.flushed)
	}
}

// releasePacket returns a queued packet to the pool. Live publishes are reset
// field by field by resetOutboundPacket; control packets can carry any field,
// so they are cleared completely.
func releasePacket(it writeItem) {
	if it.live {
		resetOutboundPacket(it.pk)
		return
	}
	*it.pk = packets.Packet{}
	outboundPacketPool.Put(it.pk)
}

// WritePacket queues pk for the client's writer. It returns once the packet is
// queued, not once it is written, and blocks while the control queue is full.
// The only error is ErrConnectionClosed. Packets for a client without a
// connection (an inline client) are discarded.
func (cl *Client) WritePacket(pk packets.Packet) error {
	_, err := cl.enqueueControl(&pk, nil, nil, nil)
	return err
}

// writeAndWait queues pk, or only a flush when pk is nil, and waits until
// everything queued so far has been written, the client is stopped, or the
// timeout expires. With closeCause set, the writer stops the client with that
// cause as soon as pk is written, so nothing queued after pk reaches the client.
func (cl *Client) writeAndWait(pk *packets.Packet, closeCause error, timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	flushed := make(chan struct{})
	queued, err := cl.enqueueControl(pk, flushed, closeCause, t.C)
	if err != nil || !queued {
		return err
	}
	select {
	case <-flushed:
	case <-cl.State.open.Done():
	case <-t.C:
	}
	return nil
}

func (cl *Client) enqueueControl(pk *packets.Packet, flushed chan struct{}, closeCause error, expired <-chan time.Time) (queued bool, err error) {
	if cl.Closed() {
		return false, ErrConnectionClosed
	}
	if cl.Net.Conn == nil {
		return false, nil
	}

	it := writeItem{flushed: flushed, closeAfter: closeCause}
	if pk != nil {
		it.pk = outboundPacketPool.Get().(*packets.Packet)
		*it.pk = *pk
	}
	select {
	case cl.State.control <- it:
		return true, nil
	default:
	}
	select {
	case cl.State.control <- it:
		return true, nil
	case <-cl.State.open.Done():
	case <-expired:
	}
	if it.pk != nil {
		releasePacket(it)
	}
	return false, ErrConnectionClosed
}

func encodeOtherPacket(pk *packets.Packet, buf *bytes.Buffer) error {
	switch pk.FixedHeader.Type {
	case packets.Connect:
		return pk.ConnectEncode(buf)
	case packets.Connack:
		return pk.ConnackEncode(buf)
	case packets.Publish:
		return pk.PublishEncode(buf)
	case packets.Puback:
		return pk.PubackEncode(buf)
	case packets.Pubrec:
		return pk.PubrecEncode(buf)
	case packets.Pubrel:
		return pk.PubrelEncode(buf)
	case packets.Pubcomp:
		return pk.PubcompEncode(buf)
	case packets.Subscribe:
		return pk.SubscribeEncode(buf)
	case packets.Suback:
		return pk.SubackEncode(buf)
	case packets.Unsubscribe:
		return pk.UnsubscribeEncode(buf)
	case packets.Unsuback:
		return pk.UnsubackEncode(buf)
	case packets.Pingreq:
		return pk.PingreqEncode(buf)
	case packets.Pingresp:
		return pk.PingrespEncode(buf)
	case packets.Disconnect:
		return pk.DisconnectEncode(buf)
	case packets.Auth:
		return pk.AuthEncode(buf)
	default:
		return fmt.Errorf("%w: %v", packets.ErrNoValidPacketAvailable, pk.FixedHeader.Type)
	}
}
