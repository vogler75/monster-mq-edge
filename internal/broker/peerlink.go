package broker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"monstermq.io/edge/internal/bridge/mqttclient"
	"monstermq.io/edge/internal/bridge/rtspcamera"
	"monstermq.io/edge/internal/bridge/winccoa"
	"monstermq.io/edge/internal/bridge/winccua"
	"monstermq.io/edge/internal/config"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink"
	"monstermq.io/edge/internal/peerlink/wire"
	"monstermq.io/edge/internal/scripting"
	"monstermq.io/edge/internal/stores"
)

// peerLinkNamespaceRoot is the single predicate of the PeerLink namespace
// exclusion: the WinCC OA TopicRoot while native mode is active (embedded
// host, WinCCOaNative.Enabled and Namespace), "" otherwise. It returns a
// function so the decision can later depend on the WinCC OA host role.
func peerLinkNamespaceRoot(nativeActive bool, root string) func() string {
	if !nativeActive {
		return nil
	}
	return func() string { return root }
}

// peerLinkRetainedClass is the retained store class announced in the
// PeerLink handshake.
func peerLinkRetainedClass(t config.StoreType) wire.RetainedClass {
	switch t {
	case config.StoreMemory:
		return wire.RetainedMemory
	case config.StoreWinCCOA:
		return wire.RetainedWinCCOA
	}
	return wire.RetainedDB
}

// peerRetained implements peerlink.RetainedAccess. In MEMORY mode the
// engine's retained map is the source of truth; in DB modes the retained
// store is.
type peerRetained struct {
	engine *mqtt.Server
	store  stores.MessageStore
	hook   *StorageHook
	memory bool
}

func (r *peerRetained) Snapshot(ctx context.Context, fn func(pk packets.Packet) bool) error {
	if r.memory {
		for _, pk := range r.engine.Topics.Retained.GetAll() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !fn(pk) {
				return nil
			}
		}
		return nil
	}
	// ctx is the session (source) or the snapshot (consumer preload): a slow scan ends with them.
	return r.store.FindMatchingMessages(ctx, "#", func(msg stores.BrokerMessage) bool {
		return fn(retainedPacket(msg))
	})
}

func (r *peerRetained) Has(topic string) bool {
	if r.memory {
		_, ok := r.engine.Topics.Retained.Get(topic)
		return ok
	}
	msg := r.get(topic)
	return msg != nil && len(msg.Payload) > 0
}

func (r *peerRetained) Created(topic string) (int64, bool) {
	if r.memory {
		pk, ok := r.engine.Topics.Retained.Get(topic)
		return pk.Created, ok
	}
	msg := r.get(topic)
	if msg == nil || len(msg.Payload) == 0 {
		return 0, false
	}
	return msg.Time.Unix(), true
}

func (r *peerRetained) Get(topic string) (packets.Packet, bool) {
	if r.memory {
		return r.engine.Topics.Retained.Get(topic)
	}
	msg := r.get(topic)
	if msg == nil || len(msg.Payload) == 0 {
		return packets.Packet{}, false
	}
	return retainedPacket(*msg), true
}

func (r *peerRetained) get(topic string) *stores.BrokerMessage {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg, err := r.store.Get(ctx, topic)
	if err != nil {
		return nil
	}
	return msg
}

func (r *peerRetained) FlushReplicas(source string) error {
	return r.hook.FlushReplicas(source)
}

// retainedPacket converts a stored retained row into the packet shape the
// PeerLink snapshot encodes.
func retainedPacket(msg stores.BrokerMessage) packets.Packet {
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: msg.QoS, Retain: true},
		TopicName:   msg.TopicName,
		Payload:     msg.Payload,
		Origin:      msg.ClientID,
		Created:     msg.Time.Unix(),
	}
	if msg.MessageExpiryInterval != nil && *msg.MessageExpiryInterval > 0 {
		pk.Properties.MessageExpiryInterval = *msg.MessageExpiryInterval
		pk.Expiry = pk.Created + int64(*msg.MessageExpiryInterval)
	}
	if msg.PayloadFormatIndicator != nil {
		pf := *msg.PayloadFormatIndicator
		pk.Properties.PayloadFormat = pf
		pk.Properties.PayloadFormatFlag = true
	}
	pk.Properties.ContentType = msg.ContentType
	pk.Properties.ResponseTopic = msg.ResponseTopic
	pk.Properties.CorrelationData = msg.CorrelationData
	for k, v := range msg.UserProperties {
		pk.Properties.User = append(pk.Properties.User, packets.UserProperty{Key: k, Val: v})
	}
	return pk
}

// warnPeerLinkDevices logs the device WARNs of plan 6.2 build() step 5:
// devices whose output would arrive twice or loop between the peers.
// Config.Validate cannot see these configs, which live in the
// DeviceConfigStore.
func warnPeerLinkDevices(ctx context.Context, cfg *config.Config, storage *stores.Storage, setup *config.PeerLinkSetup, logger *slog.Logger) {
	if cfg.HostMonitoring.Enabled && !strings.Contains(cfg.HostMonitoring.BaseTopic, "{NodeId}") {
		logger.Warn("PeerLink: HostMonitoring.BaseTopic has no {NodeId}; the host metrics of every node share one topic tree",
			"baseTopic", cfg.HostMonitoring.BaseTopic)
	}
	if storage.DeviceConfig == nil {
		return
	}
	devs, err := storage.DeviceConfig.GetAll(ctx)
	if err != nil {
		logger.Warn("PeerLink: device configs not readable for the startup checks", "err", err)
		return
	}
	shared := false
	switch cfg.ConfigStore() {
	case config.StoreWinCCOA, config.StorePostgres, config.StoreMongoDB:
		shared = true
	}
	peerHosts := map[string]string{}
	for _, p := range setup.Peers {
		if host, _, err := net.SplitHostPort(p.Address); err == nil && host != "" {
			peerHosts[strings.ToLower(host)] = p.NodeID
		}
	}
	for _, d := range devs {
		if !d.Enabled {
			continue
		}
		everyNode := d.NodeID == "local" || d.NodeID == "*"
		switch d.Type {
		case "Redfish":
			logger.Warn("PeerLink: Redfish gateways ignore NodeId and run on every node; their publishes arrive on the peers twice",
				"device", d.Name)
			continue
		case "", "MQTT_CLIENT":
			var mc mqttclient.Config
			if err := json.Unmarshal([]byte(d.Config), &mc); err != nil {
				continue
			}
			inbound := false
			for _, a := range mc.Addresses {
				if strings.EqualFold(a.Mode, "SUBSCRIBE") {
					inbound = true
					break
				}
			}
			if host := bridgeHost(mc.BrokerURL); host != "" {
				if peer, ok := peerHosts[host]; ok {
					logger.Warn("PeerLink: MQTT bridge connects to a PeerLink peer; messages can loop between the nodes",
						"device", d.Name, "peer", peer, "brokerUrl", mc.BrokerURL, "inbound", inbound)
				}
			}
			if !inbound {
				continue
			}
		case winccua.DeviceTypeWinCCUaClient, winccoa.DeviceTypeWinCCOaClient, rtspcamera.DeviceTypeRtspCamera, scripting.DeviceTypeScript:
		default:
			continue
		}
		if shared && everyNode {
			logger.Warn("PeerLink: device publishes into the broker on every node of a shared config store; its output arrives twice",
				"device", d.Name, "type", d.Type, "nodeId", d.NodeID, "configStore", cfg.ConfigStore())
		}
	}
}

func bridgeHost(brokerURL string) string {
	if brokerURL == "" {
		return ""
	}
	if u, err := url.Parse(brokerURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	if host, _, err := net.SplitHostPort(brokerURL); err == nil {
		return strings.ToLower(host)
	}
	return ""
}

// peerLinkNativeStatus returns the native status hook for the compact
// PeerLink object (plan 20.2), or nil when PeerLink is off. pl is filled
// later in build(), before the native service starts.
func peerLinkNativeStatus(enabled bool, pl **peerlink.Manager) func() any {
	if !enabled {
		return nil
	}
	return func() any {
		if *pl == nil {
			return nil
		}
		return (*pl).NativeStatus()
	}
}

// startPeerStatus republishes the native status every 5 s and after every
// PeerLink state change, so the peerLink object stays current (plan 20.2).
func (s *Server) startPeerStatus() {
	if s.peerStatus == nil || s.native == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.stopMu.Lock()
	s.peerStatusStop, s.peerStatusDone = cancel, done
	s.stopMu.Unlock()
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			case <-s.peerStatus:
			}
			s.native.PublishStatus()
		}
	}()
}

func (s *Server) stopPeerStatus() {
	s.stopMu.Lock()
	stop, done := s.peerStatusStop, s.peerStatusDone
	s.peerStatusStop = nil
	s.stopMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}
