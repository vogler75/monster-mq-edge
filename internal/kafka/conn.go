package kafka

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"

	"monstermq.io/edge/internal/kafka/wire"
)

// maxFrame is the largest request the server accepts (10 MiB).
const maxFrame = 10 * 1024 * 1024

// conn is one client connection. Requests are processed strictly in order, so
// responses are sent in request order (as a real broker does per connection).
type conn struct {
	s      *Server
	nc     net.Conn
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once

	authenticated bool
	username      string
	rawSasl       bool // SaslHandshake v0 seen: next frames are raw PLAIN tokens

	publishers map[string]struct{} // inline MQTT client IDs used by Produce
}

func newConn(s *Server, nc net.Conn) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{s: s, nc: nc, ctx: ctx, cancel: cancel,
		authenticated: !s.deps.UserManagementEnabled, publishers: map[string]struct{}{}}
}

func (c *conn) close() {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.nc.Close()
	})
}

func (c *conn) serve() {
	defer func() {
		c.close()
		if c.s.deps.Release != nil {
			for id := range c.publishers {
				c.s.deps.Release(id)
			}
		}
	}()
	br := bufio.NewReaderSize(c.nc, 64*1024)
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		size := int32(binary.BigEndian.Uint32(hdr[:]))
		if size <= 0 || size > maxFrame {
			c.s.log.Debug("Kafka frame size rejected, closing", "size", size)
			return
		}
		pkt := make([]byte, size)
		if _, err := io.ReadFull(br, pkt); err != nil {
			return
		}
		resp, keep := c.handlePacket(pkt)
		if resp != nil {
			var out [4]byte
			binary.BigEndian.PutUint32(out[:], uint32(len(resp)))
			if _, err := c.nc.Write(append(out[:], resp...)); err != nil {
				return
			}
		}
		if !keep {
			return
		}
	}
}

// handlePacket processes one request frame. resp (without the size prefix) is
// nil when no response is due; keep=false closes the connection afterwards.
func (c *conn) handlePacket(pkt []byte) (resp []byte, keep bool) {
	if c.rawSasl && !c.authenticated {
		return c.handleRawSasl(pkt)
	}
	r := wire.NewReader(pkt)
	apiKey := r.Int16()
	version := r.Int16()
	corr := r.Int32()
	clientID := r.String()
	if r.Err() != nil {
		c.s.log.Debug("Kafka request header unreadable, closing", "err", r.Err())
		return nil, false
	}
	if c.s.deps.UserManagementEnabled && !c.authenticated {
		if apiKey != wire.APIApiVersions && apiKey != wire.APISaslHandshake && apiKey != wire.APISaslAuthenticate {
			c.s.log.Warn("Unauthenticated Kafka request rejected, closing", "apiKey", apiKey, "clientId", clientID)
			return nil, false
		}
	}
	rng, known := wire.SupportedRange(apiKey)
	if !known {
		c.s.log.Warn("Unsupported Kafka API key, closing", "apiKey", apiKey)
		return nil, false
	}
	if version < rng.Min || version > rng.Max {
		if apiKey == wire.APIApiVersions {
			// KIP-511: answer an unsupported ApiVersions version with a v0 body
			// carrying UNSUPPORTED_VERSION and the supported ranges.
			w := wire.NewWriter()
			w.Int32(corr)
			c.writeAPIVersions(w, wire.ErrUnsupportedVersion)
			return w.Bytes(), true
		}
		c.s.log.Warn("Unsupported Kafka API version, closing", "apiKey", apiKey, "version", version)
		return nil, false
	}

	w := wire.NewWriter()
	w.Int32(corr)
	h := &req{c: c, v: version, r: r, w: w, clientID: clientID}
	var closeAfter bool
	respond := true
	switch apiKey {
	case wire.APIApiVersions:
		c.writeAPIVersions(w, wire.ErrNone)
	case wire.APIMetadata:
		h.metadata()
	case wire.APIProduce:
		respond = h.produce()
	case wire.APIFetch:
		h.fetch()
	case wire.APIListOffsets:
		h.listOffsets()
	case wire.APIOffsetCommit:
		h.offsetCommit()
	case wire.APIOffsetFetch:
		h.offsetFetch()
	case wire.APIFindCoordinator:
		h.findCoordinator()
	case wire.APIJoinGroup:
		h.joinGroup()
	case wire.APISyncGroup:
		h.syncGroup()
	case wire.APIHeartbeat:
		h.heartbeat()
	case wire.APILeaveGroup:
		h.leaveGroup()
	case wire.APISaslHandshake:
		h.saslHandshake()
	case wire.APISaslAuthenticate:
		closeAfter = h.saslAuthenticate()
	case wire.APIDescribeConfigs:
		h.describeConfigs()
	}
	if r.Err() != nil {
		c.s.log.Debug("Kafka request body unreadable, closing", "apiKey", apiKey, "version", version, "err", r.Err())
		return nil, false
	}
	if c.ctx.Err() != nil {
		return nil, false
	}
	if !respond {
		return nil, !closeAfter
	}
	return w.Bytes(), !closeAfter
}

func (c *conn) writeAPIVersions(w *wire.Writer, code int16) {
	w.Int16(code)
	w.Int32(int32(len(wire.SupportedAPIs)))
	for _, a := range wire.SupportedAPIs {
		w.Int16(a.Key)
		w.Int16(a.Min)
		w.Int16(a.Max)
	}
}

// parsePlain splits a SASL PLAIN token "[authzid]\0user\0password".
func parsePlain(tok []byte) (user, pass string, ok bool) {
	parts := strings.SplitN(string(tok), "\x00", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// authenticate validates SASL PLAIN credentials. Without user management every
// credential is accepted (the user name is kept for logging only).
func (c *conn) authenticate(user, pass string) bool {
	if !c.s.deps.UserManagementEnabled {
		c.username, c.authenticated = user, true
		return true
	}
	if c.s.deps.AuthCache != nil && c.s.deps.AuthCache.Validate(user, pass) {
		c.username, c.authenticated = user, true
		return true
	}
	return false
}

// handleRawSasl handles the unframed token after SaslHandshake v0: success is
// answered with an empty (int32 0) token frame, failure closes the connection.
func (c *conn) handleRawSasl(pkt []byte) ([]byte, bool) {
	user, pass, ok := parsePlain(pkt)
	if !ok || !c.authenticate(user, pass) {
		c.s.log.Warn("Kafka raw SASL PLAIN authentication failed", "user", user)
		return nil, false
	}
	c.rawSasl = false
	return []byte{}, true
}

func (c *conn) canPublish(clientID, mqttTopic string) bool {
	if !c.s.deps.UserManagementEnabled {
		return true
	}
	if c.s.deps.AuthCache == nil {
		return false
	}
	return c.s.deps.AuthCache.AllowClient(c.username, clientID, mqttTopic, true)
}

func (c *conn) canSubscribe(clientID, filter string) bool {
	if !c.s.deps.UserManagementEnabled {
		return true
	}
	if c.s.deps.AuthCache == nil {
		return false
	}
	return c.s.deps.AuthCache.AllowClient(c.username, clientID, filter, false)
}
