package winccoanative

import (
	"encoding/json"
	"strings"
	"time"
)

// Every WinCC OA system the host reports (distribution manager) gets a
// retained status on <root>/<systems>/<system>: {"nodeId", "system",
// "local": false, "oa": "connected|disconnected", "ready", "timestamp"}.
// The local system's status is the broker status (PublishStatus).

// seedRemote takes the systems the host reported before the watchers were
// registered and reports whether the complete list is already known.
func (s *Service) seedRemote() bool {
	systems, known := s.api.C.Systems()
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	for sys, up := range systems {
		if sys == s.localSystem {
			continue
		}
		if _, seen := s.remote[sys]; !seen {
			s.remote[sys] = up
		}
	}
	return known
}

// onSystemStatus publishes the status of a remote system that connected
// or disconnected.
func (s *Service) onSystemStatus(system string, available bool) {
	if system == "" || system == s.localSystem {
		return
	}
	s.statusMu.Lock()
	s.remote[system] = available
	s.statusMu.Unlock()
	s.publishRemoteStatus(system, available)
}

func (s *Service) publishRemoteStatus(system string, up bool) {
	if s.localSystem == "" {
		return
	}
	st := map[string]any{
		"nodeId":    s.opts.NodeID,
		"system":    system,
		"local":     false,
		"oa":        map[bool]string{true: "connected", false: "disconnected"}[up],
		"ready":     up && s.ready.Load() && s.oaUp.Load(),
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	b, _ := json.Marshal(st)
	_ = s.broker.Publish(s.opts.Names.StatusTopic(system), b, true, 1)
}

// sweepStatuses runs once the complete list of connected systems is known:
// a retained remote-system status (left from an earlier run) of a system
// that is not connected now is republished as disconnected.
func (s *Service) sweepStatuses() {
	if s.opts.RetainedStatuses == nil || s.localSystem == "" {
		return
	}
	n := s.opts.Names
	for topic, payload := range s.opts.RetainedStatuses(n.Root + "/" + n.Systems + "/+") {
		sys, ok := s.statusSystem(topic)
		if !ok || len(payload) == 0 {
			continue
		}
		var st struct {
			Local *bool  `json:"local"`
			OA    string `json:"oa"`
		}
		if json.Unmarshal(payload, &st) != nil || st.Local == nil || *st.Local {
			continue // not a remote-system status
		}
		s.statusMu.Lock()
		up, seen := s.remote[sys]
		if !seen {
			s.remote[sys] = false
		}
		s.statusMu.Unlock()
		if up || (seen && st.OA == "disconnected") {
			continue
		}
		s.publishRemoteStatus(sys, false)
	}
}

// statusSystem returns the remote system of a status topic
// <root>/<systems>/<system>; false for the local system and other topics.
func (s *Service) statusSystem(topic string) (string, bool) {
	n := s.opts.Names
	seg, ok := strings.CutPrefix(topic, n.Root+"/"+n.Systems+"/")
	if !ok || seg == "" || strings.Contains(seg, "/") {
		return "", false
	}
	sys, err := decodeSegment(seg, true)
	if err != nil || sys == "" || sys == s.localSystem {
		return "", false
	}
	return sys, true
}
