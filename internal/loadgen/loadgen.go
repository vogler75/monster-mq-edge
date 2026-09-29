// Package loadgen measures the native WinCC OA path end to end: writers
// publish numbered values to .../set, WinCC OA applies them, the hotlinks
// come back through the broker to subscribers, and the round trip of every
// value is timed (plan AC-34).
package loadgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Config struct {
	Broker      string        // tcp://host:port
	Username    string        // optional
	Password    string        // optional
	Subscribers int           // subscribing clients
	Writers     int           // writing clients
	DPEs        int           // number of elements Name(0..DPEs-1)
	TopicFmt    string        // e.g. "winccoa/local/tags/Load%05d/value"
	Rate        int           // total writes per second
	Duration    time.Duration // measuring time
	Settle      time.Duration // wait for late values after the last write
	// Change, when set, replaces the MQTT writers: it changes element dpe
	// to value seq directly in WinCC OA (measures only the OA -> MQTT leg).
	Change func(dpe int, seq int64) error
	// SubBroker and SubFilter, when set, subscribe elsewhere (e.g. the output
	// of a query bridge): every subscriber uses SubFilter on SubBroker.
	SubBroker string
	SubFilter string
}

type Result struct {
	Sent        int64         `json:"sent"`
	Received    int64         `json:"received"`
	Lost        int64         `json:"lost"`
	Rejected    int64         `json:"rejected"`
	Throughput  float64       `json:"throughputPerSec"`
	P50         time.Duration `json:"p50"`
	P95         time.Duration `json:"p95"`
	P99         time.Duration `json:"p99"`
	Max         time.Duration `json:"max"`
	Subscribers int           `json:"subscribers"`
	DPEs        int           `json:"dpes"`
	Rate        int           `json:"rate"`
	Duration    time.Duration `json:"duration"`
}

func (r Result) String() string {
	b, _ := json.MarshalIndent(map[string]any{
		"sent": r.Sent, "received": r.Received, "lost": r.Lost, "rejected": r.Rejected,
		"throughputPerSec": math.Round(r.Throughput), "p50": r.P50.String(), "p95": r.P95.String(),
		"p99": r.P99.String(), "max": r.Max.String(), "subscribers": r.Subscribers, "dpes": r.DPEs,
		"rate": r.Rate, "duration": r.Duration.String(),
	}, "", "  ")
	return string(b)
}

func connect(cfg Config, id string) (mqtt.Client, error) {
	// Ordered delivery runs the handler on the client's router goroutine
	// instead of one goroutine per message, which keeps the tool itself from
	// becoming the bottleneck at thousands of messages per second.
	o := mqtt.NewClientOptions().AddBroker(cfg.Broker).SetClientID(id).SetCleanSession(true).
		SetAutoReconnect(false).SetOrderMatters(true).SetWriteTimeout(5 * time.Second).
		SetMessageChannelDepth(65536)
	if cfg.Username != "" {
		o.SetUsername(cfg.Username).SetPassword(cfg.Password)
	}
	c := mqtt.NewClient(o)
	tok := c.Connect()
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		return nil, fmt.Errorf("connect %s: %v", id, tok.Error())
	}
	return c, nil
}

// Run executes one measurement. Each element is subscribed by exactly one
// subscriber; values are unique sequence numbers.
func Run(cfg Config) (Result, error) {
	res := Result{Subscribers: cfg.Subscribers, DPEs: cfg.DPEs, Rate: cfg.Rate, Duration: cfg.Duration}
	if cfg.Subscribers < 1 || cfg.Writers < 1 || cfg.DPEs < 1 || cfg.Rate < 1 {
		return res, fmt.Errorf("invalid load config")
	}
	var (
		recvd  atomic.Int64
		shards [64]struct {
			mu   sync.Mutex
			sent map[int64]time.Time
			lat  []time.Duration
		}
	)
	for i := range shards {
		shards[i].sent = map[int64]time.Time{}
	}
	record := func(seq int64, t time.Time) {
		sh := &shards[seq&63]
		sh.mu.Lock()
		sh.sent[seq] = t
		sh.mu.Unlock()
	}
	onMsg := func(_ mqtt.Client, m mqtt.Message) {
		now := time.Now()
		seq, ok := sequenceOf(m.Payload())
		if !ok {
			return
		}
		sh := &shards[seq&63]
		sh.mu.Lock()
		if t0, ok := sh.sent[seq]; ok {
			sh.lat = append(sh.lat, now.Sub(t0))
			delete(sh.sent, seq)
			recvd.Add(1)
		}
		sh.mu.Unlock()
	}

	var clients []mqtt.Client
	defer func() {
		for _, c := range clients {
			c.Disconnect(100)
		}
	}()
	for i := 0; i < cfg.Subscribers; i++ {
		subCfg := cfg
		if cfg.SubBroker != "" {
			subCfg.Broker = cfg.SubBroker
		}
		c, err := connect(subCfg, fmt.Sprintf("load-sub-%d", i))
		if err != nil {
			return res, err
		}
		clients = append(clients, c)
		if cfg.SubFilter != "" {
			tok := c.Subscribe(cfg.SubFilter, 0, onMsg)
			if !tok.WaitTimeout(30*time.Second) || tok.Error() != nil {
				return res, fmt.Errorf("subscribe: %v", tok.Error())
			}
			continue
		}
		filters := map[string]byte{}
		for d := i; d < cfg.DPEs; d += cfg.Subscribers {
			filters[fmt.Sprintf(cfg.TopicFmt, d)] = 0
		}
		for len(filters) > 0 {
			batch := map[string]byte{}
			for k, v := range filters {
				batch[k] = v
				delete(filters, k)
				if len(batch) == 100 {
					break
				}
			}
			tok := c.SubscribeMultiple(batch, onMsg)
			if !tok.WaitTimeout(30*time.Second) || tok.Error() != nil {
				return res, fmt.Errorf("subscribe: %v", tok.Error())
			}
		}
	}
	// Initial values arrive now; they carry no known sequence number.
	time.Sleep(2 * time.Second)

	var writers []mqtt.Client
	for i := 0; i < cfg.Writers && cfg.Change == nil; i++ {
		c, err := connect(cfg, fmt.Sprintf("load-writer-%d", i))
		if err != nil {
			return res, err
		}
		clients = append(clients, c)
		writers = append(writers, c)
	}

	var seq, rejected atomic.Int64
	seq.Store(1_000_000)
	interval := time.Second / time.Duration(cfg.Rate)
	start := time.Now()
	deadline := start.Add(cfg.Duration)
	var wg sync.WaitGroup
	nWriters := len(writers)
	if cfg.Change != nil {
		nWriters = cfg.Writers
	}
	for w := 0; w < nWriters; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			step := interval * time.Duration(nWriters)
			next := start.Add(interval * time.Duration(w))
			for n := w; ; n += nWriters {
				if time.Now().After(deadline) {
					return
				}
				if d := time.Until(next); d > 0 {
					time.Sleep(d)
				}
				next = next.Add(step)
				s := seq.Add(1)
				record(s, time.Now())
				if cfg.Change != nil {
					if err := cfg.Change(n%cfg.DPEs, s); err != nil {
						rejected.Add(1)
					}
					continue
				}
				topic := fmt.Sprintf(cfg.TopicFmt, n%cfg.DPEs) + "/set"
				payload := fmt.Sprintf(`{"value":%d}`, s)
				tok := writers[w].Publish(topic, 0, false, payload)
				if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
					rejected.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	settle := cfg.Settle
	if settle <= 0 {
		settle = 3 * time.Second
	}
	time.Sleep(settle)

	var lat []time.Duration
	for i := range shards {
		shards[i].mu.Lock()
		res.Lost += int64(len(shards[i].sent))
		lat = append(lat, shards[i].lat...)
		shards[i].mu.Unlock()
	}
	res.Sent = seq.Load() - 1_000_000
	res.Received = recvd.Load()
	res.Rejected = rejected.Load()
	res.Throughput = float64(res.Received) / elapsed.Seconds()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		i := int(math.Ceil(p*float64(len(lat)))) - 1
		if i < 0 {
			i = 0
		}
		return lat[i]
	}
	res.P50, res.P95, res.P99 = pct(0.50), pct(0.95), pct(0.99)
	if len(lat) > 0 {
		res.Max = lat[len(lat)-1]
	}
	return res, nil
}

// sequenceOf extracts the written sequence number from a native payload
// {"value":n}, a query-bridge JSON row {"_online.._value":n} or a RAW_VALUE
// payload.
func sequenceOf(payload []byte) (int64, bool) {
	// Fast path for {"value":<n>,...} and {"_online.._value":<n>,...}.
	for _, key := range [][]byte{[]byte(`"value":`), []byte(`"_online.._value":`)} {
		if i := bytes.Index(payload, key); i >= 0 {
			rest := payload[i+len(key):]
			j := 0
			for j < len(rest) && (rest[j] == '-' || rest[j] == '.' || (rest[j] >= '0' && rest[j] <= '9')) {
				j++
			}
			if f, err := strconv.ParseFloat(string(rest[:j]), 64); err == nil {
				return int64(f), true
			}
		}
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) == nil {
		for _, k := range []string{"value", "_online.._value"} {
			if v, ok := obj[k].(float64); ok {
				return int64(v), true
			}
		}
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscan(strings.TrimSpace(string(payload)), &f); err == nil {
		return int64(f), true
	}
	return 0, false
}

// TopicFmtFor builds the topic format for local tags named prefix%05d.
func TopicFmtFor(prefix, element string) string {
	return "winccoa/local/tags/" + prefix + "%05d/" + strings.Trim(element, "/")
}
