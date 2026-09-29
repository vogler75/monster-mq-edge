package broker

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"

	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/winccoanative"
)

// WinCCOaNativeHook connects the MQTT engine to the native WinCC OA
// namespace: per-filter SUBSCRIBE validation, interest tracking, typed
// writes and protection of the reserved winccoa/ branches.
type WinCCOaNativeHook struct {
	mqtt.HookBase
	svc    *winccoanative.Service
	server *mqtt.Server
	logger *slog.Logger

	mu      sync.Mutex
	existed map[string]bool
}

// ACLs, including the canonical-topic rule for native aliases, are applied
// by the auth hook before this hook runs.
func NewWinCCOaNativeHook(svc *winccoanative.Service, server *mqtt.Server, logger *slog.Logger) *WinCCOaNativeHook {
	return &WinCCOaNativeHook{svc: svc, server: server, logger: logger, existed: map[string]bool{}}
}

func (h *WinCCOaNativeHook) ID() string { return "winccoa-native" }

func (h *WinCCOaNativeHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnSubscribeValidate,
		mqtt.OnSubscribed,
		mqtt.OnUnsubscribed,
		mqtt.OnPublish,
	}, []byte{b})
}

func existedKey(clientID, filter string) string { return clientID + "\x00" + filter }

func (h *WinCCOaNativeHook) OnSubscribeValidate(cl *mqtt.Client, sub packets.Subscription) packets.Code {
	if cl.Net.Inline {
		return packets.CodeSuccess
	}
	v, why := h.svc.Validate(sub.Filter)
	switch v {
	case winccoanative.Accept:
		_, existed := cl.State.Subscriptions.Get(sub.Filter)
		h.mu.Lock()
		h.existed[existedKey(cl.ID, sub.Filter)] = existed
		h.mu.Unlock()
		return packets.CodeSuccess
	case winccoanative.NotNative:
		return packets.CodeSuccess
	}
	h.logger.Debug("native subscription rejected", "client", cl.ID, "filter", sub.Filter, "verdict", v.String(), "reason", why)
	return verdictCode(v, why)
}

func verdictCode(v winccoanative.Verdict, why string) packets.Code {
	var c packets.Code
	switch v {
	case winccoanative.Denied:
		c = packets.ErrNotAuthorized
	case winccoanative.Invalid:
		c = packets.ErrTopicFilterInvalid
	case winccoanative.WildcardDeny:
		c = packets.ErrWildcardSubscriptionsNotSupported
	case winccoanative.SharedDeny:
		c = packets.ErrSharedSubscriptionsNotSupported
	case winccoanative.BadPayload:
		c = packets.ErrPayloadFormatInvalid
	default:
		c = packets.ErrImplementationSpecificError
	}
	if why != "" {
		c.Reason = why
	}
	return c
}

func (h *WinCCOaNativeHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	for i, f := range pk.Filters {
		if i < len(reasonCodes) && reasonCodes[i] > packets.CodeGrantedQos2.Code {
			continue
		}
		k := existedKey(cl.ID, f.Filter)
		h.mu.Lock()
		existed, ok := h.existed[k]
		delete(h.existed, k)
		h.mu.Unlock()
		if !ok {
			continue
		}
		h.svc.Subscribed(cl.ID, f.Filter, existed)
	}
}

func (h *WinCCOaNativeHook) OnUnsubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	filters := make([]string, 0, len(pk.Filters))
	for _, f := range pk.Filters {
		filters = append(filters, f.Filter)
	}
	h.svc.Unsubscribed(cl.ID, filters)
}

func (h *WinCCOaNativeHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if cl.Net.Inline {
		return pk, nil
	}
	switch winccoanative.Classify(pk.TopicName) {
	case winccoanative.KindOther:
		return pk, nil
	case winccoanative.KindNative:
	default:
		return pk, packets.ErrNotAuthorized
	}
	reply := winccoanative.Reply{Topic: pk.Properties.ResponseTopic, Correlation: pk.Properties.CorrelationData}
	v, why := h.svc.Command(cl.ID, pk.TopicName, pk.Payload, pk.FixedHeader.Retain, reply, h.sendReply)
	if v == winccoanative.Accept {
		return pk, packets.CodeSuccessIgnore
	}
	h.logger.Info("native command rejected", "client", cl.ID, "topic", pk.TopicName, "verdict", v.String(), "reason", why)
	h.rejectReply(pk, reply, v, why)
	code := verdictCode(v, why)
	if v == winccoanative.Invalid {
		code = packets.ErrTopicNameInvalid
		code.Reason = why
	}
	return pk, code
}

// rejectReply reports a synchronous rejection on the reply topic too, so
// MQTT 3.1.1 and QoS 0 publishers can observe it.
func (h *WinCCOaNativeHook) rejectReply(pk packets.Packet, reply winccoanative.Reply, v winccoanative.Verdict, why string) {
	if reply.Topic == "" {
		if c, err := winccoanative.ParseCommand(pk.Payload); err == nil {
			reply.Topic = c.ReplyTo
		}
	}
	if reply.Topic == "" {
		return
	}
	id := ""
	if c, err := winccoanative.ParseCommand(pk.Payload); err == nil {
		id = c.ID
	}
	res := winccoanative.CommandResult{ID: id, Topic: pk.TopicName, Status: "rejected", Error: v.String() + ": " + why}
	b, _ := json.Marshal(res)
	h.sendReply(reply, b)
}

func (h *WinCCOaNativeHook) sendReply(r winccoanative.Reply, payload []byte) {
	if winccoanative.Classify(r.Topic) != winccoanative.KindOther {
		return
	}
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1},
		TopicName:   r.Topic,
		Payload:     payload,
		PacketID:    1,
	}
	if len(r.Correlation) > 0 {
		pk.Properties.CorrelationData = r.Correlation
	}
	if err := h.server.PublishPacket(pk); err != nil {
		h.logger.Warn("native command reply failed", "topic", r.Topic, "err", err)
	}
}
