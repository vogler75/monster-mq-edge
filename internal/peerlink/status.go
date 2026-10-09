package peerlink

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stripedCounter is a counter incremented on publisher goroutines. Each increment picks one of 16
// cache-line-padded stripes with the runtime's per-thread generator, so concurrent publishers do not
// contend on one cache line (plan 7.2).
type stripedCounter struct {
	stripes [16]struct {
		n atomic.Uint64
		_ [56]byte
	}
}

func (c *stripedCounter) Inc() { c.stripes[rand.Uint32()&15].n.Add(1) }

func (c *stripedCounter) Load() uint64 {
	var s uint64
	for i := range c.stripes {
		s += c.stripes[i].n.Load()
	}
	return s
}

// latencyHist is an allocation-free histogram of apply delays in milliseconds (plan 20.1).
type latencyHist struct {
	buckets [len(latencyBounds) + 1]atomic.Uint64
}

var latencyBounds = [...]uint64{1, 2, 3, 5, 7, 10, 15, 20, 30, 50, 70, 100, 150, 200, 300, 500, 700,
	1000, 1500, 2000, 3000, 5000, 7000, 10000, 20000, 30000, 60000, 120000, 300000}

func (h *latencyHist) observe(ms int64) {
	if ms < 0 {
		ms = 0
	}
	i := 0
	for i < len(latencyBounds) && uint64(ms) > latencyBounds[i] {
		i++
	}
	h.buckets[i].Add(1)
}

// quantile returns the upper bound of the bucket holding quantile q, or -1 without samples.
func (h *latencyHist) quantile(q float64) int64 {
	var counts [len(latencyBounds) + 1]uint64
	var total uint64
	for i := range h.buckets {
		counts[i] = h.buckets[i].Load()
		total += counts[i]
	}
	if total == 0 {
		return -1
	}
	rank := uint64(q*float64(total-1)) + 1
	var acc uint64
	for i, c := range counts {
		acc += c
		if acc >= rank {
			if i < len(latencyBounds) {
				return int64(latencyBounds[i])
			}
			return int64(latencyBounds[len(latencyBounds)-1]) + 1
		}
	}
	return -1
}

// rateLimiter rate-limits log lines per key and reports how many were suppressed meanwhile.
type rateLimiter struct {
	mu sync.Mutex
	m  map[string]*rateState
}

type rateState struct {
	last       time.Time
	every      time.Duration
	suppressed uint64
}

// rateLimiterMax bounds the key set. Keys are built from codes and configured NodeIds only, so the
// bound is a safety net: entries past their interval are pruned first, then the map is reset.
const rateLimiterMax = 1024

func (r *rateLimiter) allow(key string, every time.Duration) (bool, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]*rateState)
	}
	st := r.m[key]
	now := time.Now()
	if st == nil {
		if len(r.m) >= rateLimiterMax {
			for k, v := range r.m {
				if now.Sub(v.last) >= v.every {
					delete(r.m, k)
				}
			}
			if len(r.m) >= rateLimiterMax {
				clear(r.m)
			}
		}
		r.m[key] = &rateState{last: now, every: every}
		return true, 0
	}
	if now.Sub(st.last) < every {
		st.suppressed++
		return false, 0
	}
	n := st.suppressed
	st.last, st.suppressed = now, 0
	return true, n
}

// Status is the JSON document of GET /peerlink/v1/status and the return value of Manager.Status
// (plan 20.1). Every counter the integration tests assert is here.
type Status struct {
	Enabled   bool             `json:"enabled"`
	NodeID    string           `json:"nodeId"`
	Epoch     uint64           `json:"epoch"`
	Listen    string           `json:"listen"`
	TLS       bool             `json:"tls"`
	Log       LogStatus        `json:"log"`
	Admission AdmissionStatus  `json:"admission"`
	Consumers []ConsumerStatus `json:"consumers"`
	Sources   []SourceStatus   `json:"sources"`
	Interest  *InterestCounts  `json:"interest,omitempty"`
}

// InterestCounts are the node-wide interest routing counters (plan-peerlink-interest-routing 13).
// The source counters are present when a consumer uses interest routing, Local when a source does.
type InterestCounts struct {
	InterestSkipped          uint64         `json:"interestSkipped"`
	InterestMatched          uint64         `json:"interestMatched"`
	SparseBatches            uint64         `json:"sparseBatches"`
	VolatileDropped          uint64         `json:"volatileDropped"`
	PersistentExpired        uint64         `json:"persistentExpired"`
	InterestBacklogDiscarded uint64         `json:"interestBacklogDiscarded"`
	InterestRejected         uint64         `json:"interestRejected"`
	InterestOverLimit        uint64         `json:"interestOverLimit"`
	DeltasReceived           uint64         `json:"deltasReceived"`
	Local                    *TrackerStatus `json:"local,omitempty"`
}

// SourceInterest is the consumer side of interest routing on one link.
type SourceInterest struct {
	Active        bool   `json:"active"`
	DeltasSent    uint64 `json:"deltasSent"`
	SnapshotsSent uint64 `json:"snapshotsSent"`
}

// KindCounts splits appended records by kind.
type KindCounts struct {
	Client uint64 `json:"client"`
	Inline uint64 `json:"inline"`
	Will   uint64 `json:"will"`
}

// LogStatus describes the source log and the capture hook.
type LogStatus struct {
	Epoch                uint64            `json:"epoch"`
	LSO                  uint64            `json:"lso"`
	LEO                  uint64            `json:"leo"`
	LWM                  uint64            `json:"lwm"`
	Records              uint64            `json:"records"`
	Bytes                uint64            `json:"bytes"`
	MaxBytes             uint64            `json:"maxBytes"`
	MaxMessages          uint64            `json:"maxMessages"`
	CapacitySeconds      *float64          `json:"capacitySeconds"` // null while nothing is appended
	Appended             KindCounts        `json:"appended"`
	Trimmed              uint64            `json:"trimmed"`
	EvictedUnread        uint64            `json:"evictedUnread"`
	EvictedBy            map[string]uint64 `json:"evictedBy"`
	CaptureDropped       map[string]uint64 `json:"captureDropped"`
	SkipPeer             uint64            `json:"skipPeer"`
	SkipWill             uint64            `json:"skipWill"`
	Filtered             uint64            `json:"filtered"`
	EchoSuppressed       uint64            `json:"echoSuppressed"`
	SharedSkipped        uint64            `json:"sharedSkipped"`
	RefusedClientIDs     uint64            `json:"refusedClientIds"`
	UsernameStripped     uint64            `json:"usernameStripped"`
	SpareMisses          uint64            `json:"spareMisses"`
	UncapturedAtShutdown uint64            `json:"uncapturedAtShutdown"`
	Sealed               bool              `json:"sealed"`
	Active               bool              `json:"active"`
}

// AdmissionStatus counts connections refused by the peer listener before authentication.
type AdmissionStatus struct {
	Accepted         uint64            `json:"accepted"`
	RefusedNetwork   uint64            `json:"refusedNetwork"`
	RefusedBusy      uint64            `json:"refusedBusy"`
	RefusedSniff     uint64            `json:"refusedSniff"`
	RefusedPlaintext uint64            `json:"refusedPlaintext"`
	RefusedHTTP      uint64            `json:"refusedHttp"`
	TLSFailures      uint64            `json:"tlsFailures"`
	AuthFailures     map[string]uint64 `json:"authFailures"`
	PreAuth          int               `json:"preAuth"`
}

// ConsumerStatus is one consumer (a peer pulling from this node) as seen by the source.
type ConsumerStatus struct {
	NodeID                string            `json:"nodeId"`
	State                 string            `json:"state"`
	Remote                string            `json:"remote"`
	Committed             uint64            `json:"committed"`
	Served                uint64            `json:"served"`
	Lag                   uint64            `json:"lag"`
	LostTotal             uint64            `json:"lostTotal"`
	ServedRecords         uint64            `json:"servedRecords"`
	ServedBytes           uint64            `json:"servedBytes"`
	ServedSkipped         map[string]uint64 `json:"servedSkipped"`
	SnapshotServed        uint64            `json:"snapshotServed"`
	Sessions              uint64            `json:"sessions"`
	DuplicateConsumer     uint64            `json:"duplicateConsumer"`
	AuthFailures          map[string]uint64 `json:"authFailures"`
	LastFetch             string            `json:"lastFetch,omitempty"`
	ShutdownUnserved      uint64            `json:"shutdownUnserved"`
	OARetained            bool              `json:"oaRetained"`
	TopicRootMismatch     bool              `json:"topicRootMismatch"`
	RetainedClassMismatch bool              `json:"retainedClassMismatch"`
	Interest              *InterestStatus   `json:"interest,omitempty"`
}

// SourceStatus is one source (a peer this node pulls from) as seen by the consumer.
type SourceStatus struct {
	NodeID                 string            `json:"nodeId"`
	Address                string            `json:"address"`
	State                  string            `json:"state"`
	Epoch                  uint64            `json:"epoch"`
	AppliedNext            uint64            `json:"appliedNext"`
	SourceLeo              uint64            `json:"sourceLeo"`
	LagRecords             uint64            `json:"lagRecords"`
	Batches                uint64            `json:"batches"`
	Injected               uint64            `json:"injected"`
	RetainOnly             uint64            `json:"retainOnly"`
	AppliedBytes           uint64            `json:"appliedBytes"`
	DupSkipped             uint64            `json:"dupSkipped"`
	Dropped                map[string]uint64 `json:"dropped"`
	RetainedDiverged       map[string]uint64 `json:"retainedDiverged"`
	Rejected               uint64            `json:"rejected"`
	UnknownProps           uint64            `json:"unknownProps"`
	GapLostTotal           uint64            `json:"gapLostTotal"`
	SourceResets           uint64            `json:"sourceResets"`
	ResetLostLowerBound    uint64            `json:"resetLostLowerBound"`
	Reconnects             uint64            `json:"reconnects"`
	Sessions               uint64            `json:"sessions"`
	CRCErrors              uint64            `json:"crcErrors"`
	SnapshotFilled         uint64            `json:"snapshotFilled"`
	SnapshotSkippedPresent uint64            `json:"snapshotSkippedPresent"`
	SnapshotNewer          uint64            `json:"snapshotNewer"`
	SnapshotTruncated      uint64            `json:"snapshotTruncated"`
	Snapshots              uint64            `json:"snapshots"`
	SnapshotsInterrupted   uint64            `json:"snapshotsInterrupted"`
	RetainedFlushErrors    uint64            `json:"retainedFlushErrors"`
	SupersededWillResent   uint64            `json:"supersededWillResent"`
	Paced                  uint64            `json:"paced"`
	LastError              string            `json:"lastError"`
	ClockSkewMs            int64             `json:"clockSkewMs"`
	RTTMs                  float64           `json:"rttMs"`
	TopicRootMismatch      bool              `json:"topicRootMismatch"`
	RetainedClassMismatch  bool              `json:"retainedClassMismatch"`
	OARetained             bool              `json:"oaRetained"`
	ApplyDelayMs           ApplyDelay        `json:"applyDelayMs"`
	Interest               *SourceInterest   `json:"interest,omitempty"`
}

// ApplyDelay holds bucketed apply-delay quantiles in ms (-1 without samples).
type ApplyDelay struct {
	P50  int64 `json:"p50"`
	P99  int64 `json:"p99"`
	P999 int64 `json:"p99_9"`
}

// NativeStatus is the compact peerLink object for the WinCC OA native status JSON (plan 20.2).
type NativeStatus struct {
	Enabled   bool                   `json:"enabled"`
	Consumers []NativeConsumerStatus `json:"consumers"`
	Sources   []NativeSourceStatus   `json:"sources"`
}

type NativeConsumerStatus struct {
	NodeID    string `json:"nodeId"`
	State     string `json:"state"`
	Lag       uint64 `json:"lag"`
	LostTotal uint64 `json:"lostTotal"`
}

type NativeSourceStatus struct {
	NodeID           string `json:"nodeId"`
	State            string `json:"state"`
	LagRecords       uint64 `json:"lagRecords"`
	GapLostTotal     uint64 `json:"gapLostTotal"`
	SourceResets     uint64 `json:"sourceResets"`
	RetainedDiverged uint64 `json:"retainedDiverged"`
	LastError        string `json:"lastError"`
}

// NativeStatus returns the compact status object for the native status JSON.
func (m *Manager) NativeStatus() NativeStatus {
	st := m.Status()
	ns := NativeStatus{Enabled: true}
	for _, c := range st.Consumers {
		ns.Consumers = append(ns.Consumers, NativeConsumerStatus{NodeID: c.NodeID, State: c.State, Lag: c.Lag, LostTotal: c.LostTotal})
	}
	for _, s := range st.Sources {
		var div uint64
		for _, v := range s.RetainedDiverged {
			div += v
		}
		ns.Sources = append(ns.Sources, NativeSourceStatus{NodeID: s.NodeID, State: s.State, LagRecords: s.LagRecords,
			GapLostTotal: s.GapLostTotal, SourceResets: s.SourceResets, RetainedDiverged: div, LastError: s.LastError})
	}
	return ns
}

// serveHTTP answers one request on a sniffed status connection and closes it. loopback allows the
// resync endpoint; a non-loopback caller (an mTLS peer) gets the status only.
func (m *Manager) serveHTTP(conn net.Conn, br *bufio.Reader, loopback bool) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// http.ReadRequest does not bound the request line and headers; the endpoints take no body.
	req, err := http.ReadRequest(bufio.NewReaderSize(io.LimitReader(br, maxHTTPRequest), 4096))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	code, body := m.handleHTTP(req, loopback)
	resp := &http.Response{
		StatusCode:    code,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(string(body))),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	_ = resp.Write(conn)
}

// maxHTTPRequest bounds a status or resync request: request line, headers and body.
const maxHTTPRequest = 16 << 10

// loopbackHost reports whether the Host header names the loopback interface. A browser page that
// reaches 127.0.0.1 through DNS rebinding sends its own name.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m *Manager) handleHTTP(req *http.Request, loopback bool) (int, []byte) {
	jsonErr := func(code int, msg string) (int, []byte) {
		b, _ := json.Marshal(map[string]string{"error": msg})
		return code, b
	}
	// Browsers send Origin on cross-site requests (also no-cors POSTs); the endpoints are for
	// local tools only.
	if loopback && (req.Header.Get("Origin") != "" || !loopbackHost(req.Host)) {
		return jsonErr(http.StatusForbidden, "loopback tools only: Host must be a loopback address and Origin absent")
	}
	switch req.URL.Path {
	case "/peerlink/v1/status":
		if req.Method != http.MethodGet {
			return jsonErr(http.StatusMethodNotAllowed, "GET only")
		}
		b, err := json.Marshal(m.Status())
		if err != nil {
			return jsonErr(http.StatusInternalServerError, err.Error())
		}
		return http.StatusOK, b
	case "/peerlink/v1/resync":
		if !loopback {
			return jsonErr(http.StatusForbidden, "resync is loopback only")
		}
		if req.Method != http.MethodPost {
			return jsonErr(http.StatusMethodNotAllowed, "POST only")
		}
		source := strings.ToLower(req.URL.Query().Get("source"))
		if err := m.Resync(source); err != nil {
			return jsonErr(http.StatusBadRequest, err.Error())
		}
		b, _ := json.Marshal(map[string]string{"source": source, "mode": "NEWER", "status": "requested"})
		return http.StatusAccepted, b
	}
	return jsonErr(http.StatusNotFound, fmt.Sprintf("no handler for %s", req.URL.Path))
}
