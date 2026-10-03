package winccoanative

import (
	"context"
	"strings"
	"sync"

	"monstermq.io/edge/internal/oahost"
)

// reduState is the WinCC OA redundancy state reported in the broker status:
// which host this broker runs on and which host is active. The role follows
// _ReduManager.Status.Active (host 1) and _ReduManager_2.Status.Active
// (host 2), so a switchover updates the status at once.
type reduState struct {
	mu        sync.Mutex
	redundant bool
	host      int // own host, 1 or 2; 0 = not determined
	hostNames [3]string
	active    [3]*bool // index 1 and 2; nil = not known yet
	ref       uint64
}

// ownReduHost finds the host this manager runs on: the computer name
// matches event host 1 or 2 (case-insensitive, first label), otherwise the
// manager's replica number is used.
func ownReduHost(info oahost.SysInfo) int {
	local := shortHost(info.LocalHost)
	for i, h := range info.Hosts {
		if i < 2 && local != "" && shortHost(h) == local {
			return i + 1
		}
	}
	if info.Replica == 1 || info.Replica == 2 {
		return int(info.Replica)
	}
	return 0
}

func shortHost(h string) string {
	h, _, _ = strings.Cut(strings.TrimSpace(h), ".")
	return strings.ToLower(h)
}

func reduActiveAddr(system string, host int) string {
	dp := "_ReduManager"
	if host == 2 {
		dp = "_ReduManager_2"
	}
	return system + ":" + dp + ".Status.Active:" + DefaultAttr
}

// startRedu records the redundancy facts and connects the two Status.Active
// elements. A failed connect leaves the role UNKNOWN.
func (s *Service) startRedu(ctx context.Context, info oahost.SysInfo) {
	r := &s.redu
	r.mu.Lock()
	r.redundant = info.Redundant
	if info.Redundant {
		r.host = ownReduHost(info)
		for i, h := range info.Hosts {
			if i < 2 {
				r.hostNames[i+1] = h
			}
		}
	}
	r.mu.Unlock()
	if !info.Redundant {
		return
	}
	names := []string{reduActiveAddr(s.localSystem, 1), reduActiveAddr(s.localSystem, 2)}
	ref, err := s.api.DpConnect(ctx, names, oahost.FlagAnswer, s.onReduHotlink, s.opts.ConnectTimeout)
	if err != nil {
		s.logger.Warn("redundancy state not available; role UNKNOWN", "err", err)
		return
	}
	r.mu.Lock()
	r.ref = ref
	r.mu.Unlock()
	s.logger.Info("redundant WinCC OA system", "host", r.host, "hosts", info.Hosts, "localHost", info.LocalHost)
}

// onReduHotlink updates the active flags and republishes the status when
// the role or the active host changed. With connectToRedundantHosts the
// manager may get each change once per event manager; repeats are ignored.
func (s *Service) onReduHotlink(m oahost.Message) {
	names, values, err := oahost.HotlinkItems(m)
	if err != nil {
		return
	}
	changed := false
	r := &s.redu
	r.mu.Lock()
	for i, n := range names {
		host := 0
		switch n {
		case reduActiveAddr(s.localSystem, 1):
			host = 1
		case reduActiveAddr(s.localSystem, 2):
			host = 2
		default:
			continue
		}
		v := values[i].Bool
		if r.active[host] == nil || *r.active[host] != v {
			r.active[host] = &v
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		s.logger.Info("redundancy state changed", "role", s.reduRole(), "activeHost", s.reduActiveHost())
		s.PublishStatus()
	}
}

func (s *Service) stopRedu(ctx context.Context) {
	r := &s.redu
	r.mu.Lock()
	ref := r.ref
	r.ref = 0
	r.mu.Unlock()
	if ref != 0 {
		if err := s.api.DpDisconnect(ctx, ref); err != nil {
			s.logger.Warn("dpDisconnect of redundancy state failed", "err", err)
		}
	}
}

// reduRole is STANDALONE for a non-redundant system, otherwise ACTIVE or
// PASSIVE for the host this broker runs on, or UNKNOWN until both the host
// and its state are known.
func (s *Service) reduRole() string {
	r := &s.redu
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.redundant {
		return "STANDALONE"
	}
	if r.host == 0 || r.active[r.host] == nil {
		return "UNKNOWN"
	}
	if *r.active[r.host] {
		return "ACTIVE"
	}
	return "PASSIVE"
}

// reduActiveHost is the active host (1 or 2), or 0 when unknown or when both
// report active (split mode).
func (s *Service) reduActiveHost() int {
	r := &s.redu
	r.mu.Lock()
	defer r.mu.Unlock()
	a1 := r.active[1] != nil && *r.active[1]
	a2 := r.active[2] != nil && *r.active[2]
	switch {
	case a1 && !a2:
		return 1
	case a2 && !a1:
		return 2
	}
	return 0
}

// reduStatus adds the redundancy fields to the broker status.
func (s *Service) reduStatus(st map[string]any) {
	st["role"] = s.reduRole()
	r := &s.redu
	r.mu.Lock()
	redundant, host, name := r.redundant, r.host, r.hostNames[r.host]
	r.mu.Unlock()
	st["redundant"] = redundant
	if !redundant {
		return
	}
	st["host"] = host
	st["hostName"] = name
	st["activeHost"] = s.reduActiveHost()
}
