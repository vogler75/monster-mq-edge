package broker

import (
	"bytes"

	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/winccoanative"
)

// rootWildcardHook rejects subscriptions to '#' (also as a shared
// subscription) when AllowRootWildcardSubscription is false, with the same
// reason code as the Java broker (0x8F).
type rootWildcardHook struct {
	mqtt.HookBase
}

func (h *rootWildcardHook) ID() string { return "root-wildcard" }

func (h *rootWildcardHook) Provides(b byte) bool {
	return bytes.Contains([]byte{mqtt.OnSubscribeValidate}, []byte{b})
}

func (h *rootWildcardHook) OnSubscribeValidate(cl *mqtt.Client, sub packets.Subscription) packets.Code {
	if f, _ := winccoanative.SplitShared(sub.Filter); f == "#" {
		code := packets.ErrTopicFilterInvalid
		code.Reason = "root wildcard subscriptions are disabled"
		return code
	}
	return packets.CodeSuccess
}
