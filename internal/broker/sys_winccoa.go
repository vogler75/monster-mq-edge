package broker

import (
	"strconv"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/winccoanative"
)

// winccoaSysTopics returns the $SYS/winccoa/... counters of the embedding
// host: host (Go side of the C ABI), native (namespace service, nil when
// the namespace is off) and manager (pushed by the host via mmq_stats).
func winccoaSysTopics(oa *oahost.Client, native *winccoanative.Service) func() map[string]string {
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	i := func(v int) string { return strconv.Itoa(v) }
	return func() map[string]string {
		h := oa.Stats()
		t := map[string]string{
			"winccoa/host/submitted":        u(h.Submitted),
			"winccoa/host/completed":        u(h.Completed),
			"winccoa/host/timedOut":         u(h.TimedOut),
			"winccoa/host/overloaded":       u(h.Overloaded),
			"winccoa/host/late":             u(h.LateCompletions),
			"winccoa/host/eventsDelivered":  u(h.EventsDelivered),
			"winccoa/host/eventsDropped":    u(h.EventsDropped),
			"winccoa/host/eventsUnrouted":   u(h.EventsUnrouted),
			"winccoa/host/pendingHighWater": u(h.PendingHighWater),
			"winccoa/host/pending":          i(oa.Pending()),
		}
		if native != nil {
			n := native.Stats()
			t["winccoa/native/interests"] = i(n.Interests)
			t["winccoa/native/dpes"] = i(n.DPEs)
			t["winccoa/native/batches"] = i(n.Batches)
			t["winccoa/native/connects"] = u(n.Connects)
			t["winccoa/native/disconnects"] = u(n.Disconnects)
			t["winccoa/native/connectErrors"] = u(n.ConnectErrors)
			t["winccoa/native/published"] = u(n.Published)
			t["winccoa/native/commands"] = u(n.Commands)
			t["winccoa/native/commandErrors"] = u(n.CommandErrors)
			t["winccoa/native/duplicates"] = u(n.Duplicates)
			t["winccoa/native/wildQueries"] = i(n.WildQueries)
			t["winccoa/native/wildSubs"] = i(n.WildSubs)
			t["winccoa/native/topicDps"] = i(n.TopicDPs)
			t["winccoa/native/topicSubs"] = i(n.TopicSubs)
			t["winccoa/native/topicWilds"] = i(n.TopicWilds)
			t["winccoa/native/topicDirs"] = i(n.TopicDirs)
		}
		if m := oa.HostStats(); m != nil {
			for k, v := range m.Values {
				t["winccoa/manager/"+k] = u(v)
			}
			t["winccoa/manager/updated"] = strconv.FormatInt(m.Updated.UnixMilli(), 10)
		}
		return t
	}
}
