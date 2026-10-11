package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	testSecret  = "cGVlcmxpbmstdGVzdC1ncm91cC1zZWNyZXQtdjEtMDEyMw=="
	testSecret2 = "cGVlcmxpbmstdGVzdC1zZWNyZXQtYS1iLTAwMDEtMDEyMw=="
	testPin     = "b6572a5c1146070ae6af4d3cf7a308a2bb0038b20f06081cf726a56c20434f2c"
)

func withHostname(t *testing.T, name string, err error) {
	t.Helper()
	prev := hostname
	hostname = func() (string, error) { return name, err }
	t.Cleanup(func() { hostname = prev })
}

func ptr[T any](v T) *T { return &v }

// validPeerLink is an enabled pair setup that passes validation: TLS with a
// group secret, one peer that is pulled from and served.
func validPeerLink() *Config {
	c := Default()
	c.NodeID = "node-a"
	c.PeerLink = PeerLinkConfig{
		Enabled:       true,
		Tls:           PeerLinkTLS{Enabled: true, AutoGenerate: true},
		SharedSecrets: []string{testSecret},
		Peers:         []PeerConfig{{NodeID: "node-b", Address: "node-b.local:1890"}},
	}
	return c
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPeerLinkDefaults(t *testing.T) {
	c := Default()
	p := &c.PeerLink
	if p.Enabled || p.AllowUnauthenticatedPeers {
		t.Fatal("PeerLink must be disabled by default")
	}
	if c.Runtime.MemoryLimitMB != 0 || c.Runtime.MemoryLimitBytes() != 0 {
		t.Errorf("Runtime.MemoryLimitMB default %d", c.Runtime.MemoryLimitMB)
	}
	ints := map[string][2]int{
		"Listener.Port":             {p.Listener.GetPort(), 1890},
		"Listener.MaxPreAuthPerIp":  {p.Listener.GetMaxPreAuthPerIp(), 2},
		"KeepAliveSeconds":          {p.GetKeepAliveSeconds(), 10},
		"Log.MaxMessages":           {p.Log.GetMaxMessages(), 2000000},
		"Log.MaxBytes":              {int(p.Log.GetMaxBytes()), 256 << 20},
		"Log.MaxRecordBytes":        {p.Log.GetMaxRecordBytes(c.MaxMessageSize), 1<<20 + 64<<10},
		"Log.MaxRecordBytes(0)":     {p.Log.GetMaxRecordBytes(0), 1<<20 + 64<<10},
		"Log.MaxRecordBytes(4096)":  {p.Log.GetMaxRecordBytes(4096), 4096 + 64<<10},
		"Log.DrainOnShutdownMs":     {p.Log.GetDrainOnShutdownMs(), 2000},
		"Log.NeverConnectedWarnSec": {p.Log.GetNeverConnectedWarnSec(), 300},
		"Capture.EchoSuppressMs":    {p.Capture.EchoSuppressMs, 0},
		"Snapshot.MaxTopics":        {p.Snapshot.GetMaxTopics(), 1000000},
		"Fetch.MaxRecords":          {p.Fetch.GetMaxRecords(), 4096},
		"Fetch.MaxBytes":            {p.Fetch.GetMaxBytes(), 1 << 20},
		"Fetch.MaxWaitMs":           {p.Fetch.GetMaxWaitMs(), 1000},
		"Fetch.LingerMs":            {p.Fetch.LingerMs, 0},
		"Fetch.Pipeline":            {p.Fetch.GetPipeline(), 1},
		"Fetch.ReconnectMaxMs":      {p.Fetch.GetReconnectMaxMs(), 30000},
		"Receive.MaxApplyRate":      {p.Receive.MaxApplyRate, 0},
		"Receive.MaxRecordAgeMs":    {p.Receive.MaxRecordAgeMs, 0},
		"Receive.MaxFrameBytes":     {p.Receive.GetMaxFrameBytes(), 16<<20 + 64<<10},
		"Receive.InjectWorkers":     {p.Receive.GetInjectWorkers(), 1},
		"Interest.FlushMs":          {p.Interest.GetFlushMs(), 5},
		"Interest.MaxScanPerFetch":  {p.Interest.GetMaxScanPerFetch(), 65536},
		"Interest.MaxFiltersPer":    {p.Interest.GetMaxFiltersPerPeer(), 100000},
		"Interest.MaxFilterBytes":   {p.Interest.GetMaxFilterBytes(), 1024},
	}
	for name, v := range ints {
		if v[0] != v[1] {
			t.Errorf("%s = %d, want %d", name, v[0], v[1])
		}
	}
	bools := map[string][2]bool{
		"Listener.AllowPlaintext": {p.Listener.AllowPlaintext, false},
		"Tls.Enabled":             {p.Tls.Enabled, false},
		"Tls.AutoGenerate":        {p.Tls.AutoGenerate, false},
		"Capture.Wills":           {p.Capture.GetWills(), true},
		"Interest.Enabled":        {p.Interest.GetEnabled(), true},
		"Fetch.CrcOnTls":          {p.Fetch.CrcOnTls, false},
		"Receive.Bus":             {p.Receive.GetBus(), true},
		"Receive.BridgeOutbound":  {p.Receive.BridgeOutbound, false},
		"Receive.Archive":         {p.Receive.GetArchive(), true},
		"Receive.Queue":           {p.Receive.GetQueue(), true},
		"Receive.MarkReplicas":    {p.Receive.MarkReplicas, false},
	}
	for name, v := range bools {
		if v[0] != v[1] {
			t.Errorf("%s = %v, want %v", name, v[0], v[1])
		}
	}
	strs := map[string][2]string{
		"Listener.Address":            {p.Listener.ListenAddress(), "0.0.0.0"},
		"Tls.TrustStoreType":          {p.Tls.GetTrustStoreType(), "PEM"},
		"Tls.ClientAuth":              {string(p.Tls.GetClientAuth()), "NONE"},
		"Tls.IdentityFallback":        {p.Tls.GetIdentityFallback(), "NONE"},
		"Tls.CertPath":                {p.Tls.GetCertPath(), ""},
		"Tls.KeyPath":                 {p.Tls.GetKeyPath(), ""},
		"Snapshot.Mode":               {p.Snapshot.GetMode(), "FILL"},
		"Receive.SharedSubscriptions": {p.Receive.GetSharedSubscriptions(), "SKIP"},
	}
	for name, v := range strs {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", name, v[0], v[1])
		}
	}
	if f := p.Receive.GetCatchUpRateFactor(); f != 3 {
		t.Errorf("Receive.CatchUpRateFactor = %v, want 3", f)
	}
	if got := p.Capture.GetInclude(); !reflect.DeepEqual(got, []string{"#"}) {
		t.Errorf("Capture.Include = %v", got)
	}
	if got := p.Capture.GetExclude(c.HMI.SyncBaseTopic); !reflect.DeepEqual(got, []string{"monstermq/hmi/sync/#"}) {
		t.Errorf("Capture.Exclude = %v", got)
	}
	if got := p.Capture.GetExclude("plant/hmi/"); !reflect.DeepEqual(got, []string{"plant/hmi/#"}) {
		t.Errorf("Capture.Exclude with a custom HMI base = %v", got)
	}
	p.Capture.Exclude = &[]string{}
	if got := p.Capture.GetExclude(c.HMI.SyncBaseTopic); len(got) != 0 {
		t.Errorf("Capture.Exclude [] = %v, want none", got)
	}

	auto := PeerLinkTLS{AutoGenerate: true}
	if auto.GetCertPath() != "certs/peer-{NodeId}.pem" || auto.GetKeyPath() != "certs/peer-{NodeId}.key" {
		t.Errorf("AutoGenerate paths %q %q", auto.GetCertPath(), auto.GetKeyPath())
	}

	peer := PeerConfig{NodeID: "b"}
	if !peer.GetServe() || peer.Pulls() {
		t.Errorf("peer defaults: serve %v pull %v", peer.GetServe(), peer.Pulls())
	}
	if got := peer.Receive.GetInclude(); !reflect.DeepEqual(got, []string{"#"}) {
		t.Errorf("peer Receive.Include = %v", got)
	}
	pl := PeerLinkConfig{Tls: PeerLinkTLS{Enabled: true}, SharedSecrets: []string{testSecret}}
	if !pl.DialerTLS(peer) || !reflect.DeepEqual(pl.SecretsFor(peer), []string{testSecret}) {
		t.Error("a peer without overrides must inherit the node TLS and group secrets")
	}
	peer.Tls.Enabled = ptr(false)
	peer.SharedSecrets = []string{testSecret2}
	if pl.DialerTLS(peer) || !reflect.DeepEqual(pl.SecretsFor(peer), []string{testSecret2}) {
		t.Error("per-peer TLS and secrets must override the node values")
	}
}

func TestPeerLinkStrictDecode(t *testing.T) {
	for name, doc := range map[string]string{
		"top level":   "PeerLink:\n  Enabled: false\n  AllowUnauthenticatedPeer: true\n",
		"tls":         "PeerLink:\n  Enabled: false\n  Tls:\n    Enabeld: true\n",
		"peer":        "PeerLink:\n  Peers:\n    - NodeId: b\n      SharedSecret: [x]\n",
		"peer tls":    "PeerLink:\n  Peers:\n    - NodeId: b\n      Tls: { InsecureSkipVerfy: true }\n",
		"receive":     "PeerLink:\n  Receive:\n    Busses: true\n",
		"type":        "PeerLink:\n  Enabled: false\n  KeepAliveSeconds: often\n",
		"lower case":  "PeerLink:\n  enabled: false\n",
		"capture key": "PeerLink:\n  Capture:\n    Excludes: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, doc))
			if err == nil {
				t.Fatalf("misspelled PeerLink key accepted:\n%s", doc)
			}
			if !strings.Contains(err.Error(), "PeerLink") {
				t.Errorf("error does not name PeerLink: %v", err)
			}
		})
	}

	// The rest of the file stays lenient, and a valid disabled block loads.
	cfg, err := Load(writeConfig(t, "SomeFutureKey: 1\nPeerLink:\n  Enabled: false\n  Listener: { Port: 1999 }\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PeerLink.Listener.GetPort() != 1999 {
		t.Errorf("Listener.Port = %d", cfg.PeerLink.Listener.Port)
	}
	if _, err := Load(writeConfig(t, "")); err != nil {
		t.Errorf("empty file: %v", err)
	}
	if _, err := Load(writeConfig(t, "PeerLink:\n")); err != nil {
		t.Errorf("empty PeerLink section: %v", err)
	}
}

// A disabled section is decoded but not validated, so the example block with
// placeholders stays loadable.
func TestPeerLinkDisabledIsNotValidated(t *testing.T) {
	c := validPeerLink()
	c.PeerLink.Enabled = false
	c.PeerLink.SharedSecrets = []string{"<placeholder>"}
	c.PeerLink.Listener.AllowedNetworks = []string{"nonsense"}
	c.PeerLink.Peers = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled PeerLink validated: %v", err)
	}
}

func TestPeerLinkValidation(t *testing.T) {
	withHostname(t, "build-host.plant.local", nil)
	if err := validPeerLink().Validate(); err != nil {
		t.Fatalf("base config: %v", err)
	}

	cases := []struct {
		name string
		mod  func(*Config)
		want string // empty = valid
	}{
		// 3. Canonical NodeIds.
		{"own NodeId invalid", func(c *Config) { c.NodeID = "node a" }, "may only contain"},
		{"own NodeId too long", func(c *Config) { c.NodeID = strings.Repeat("n", 65) }, "1 to 64"},
		{"own NodeId mixed case", func(c *Config) { c.NodeID = "Node-A" }, ""},
		{"peer NodeId invalid", func(c *Config) { c.PeerLink.Peers[0].NodeID = "node/b" }, "Peers[0]"},
		{"peer NodeId empty", func(c *Config) { c.PeerLink.Peers[0].NodeID = "" }, "1 to 64"},
		{"duplicate NodeIds", func(c *Config) {
			c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: "node-b", Address: "x:1890"})
		}, "already used by Peers[0]"},
		{"duplicate NodeIds case only", func(c *Config) {
			c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: "NODE-B", Address: "x:1890"})
		}, "already used by Peers[0]"},

		// 5. Peers.
		{"no peers", func(c *Config) { c.PeerLink.Peers = nil }, "at least one peer"},
		{"only the own entry", func(c *Config) {
			c.PeerLink.Peers = []PeerConfig{{NodeID: "NODE-A", Address: "node-a:1890"}}
		}, "at least one peer"},
		{"neither Address nor Serve", func(c *Config) {
			c.PeerLink.Peers[0].Address, c.PeerLink.Peers[0].Serve = "", ptr(false)
		}, "needs an Address"},
		{"serve only", func(c *Config) { c.PeerLink.Peers[0].Address = "" }, ""},
		{"pull only", func(c *Config) { c.PeerLink.Peers[0].Serve = ptr(false) }, ""},
		{"Address without port", func(c *Config) { c.PeerLink.Peers[0].Address = "node-b.local" }, "Address"},
		{"Address port 0", func(c *Config) { c.PeerLink.Peers[0].Address = "node-b.local:0" }, "port must be 1..65535"},
		{"Address port name", func(c *Config) { c.PeerLink.Peers[0].Address = "node-b.local:peer" }, "port must be 1..65535"},
		{"Address without host", func(c *Config) { c.PeerLink.Peers[0].Address = ":1890" }, "no host"},
		{"Address IPv6", func(c *Config) { c.PeerLink.Peers[0].Address = "[fd00::2]:1890" }, ""},

		// 6. Listener.
		{"Listener.Port range", func(c *Config) { c.PeerLink.Listener.Port = 70000 }, "Listener.Port"},
		{"Listener.Port negative", func(c *Config) { c.PeerLink.Listener.Port = -1 }, "Listener.Port"},
		{"AllowedNetworks not CIDR", func(c *Config) {
			c.PeerLink.Listener.AllowedNetworks = []string{"10.0.0.0/24", "10.0.0.1"}
		}, "AllowedNetworks[1]"},
		{"MaxPreAuthPerIp 0", func(c *Config) { c.PeerLink.Listener.MaxPreAuthPerIp = ptr(0) }, "MaxPreAuthPerIp"},

		// 7. Authentication.
		{"plain TCP without waiver", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
		}, "is not authenticated when it pulls from this node"},
		{"plain TCP pull only without waiver", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.Peers[0].Serve = ptr(false)
		}, "is not authenticated when this node pulls from it"},
		{"plain TCP with waiver", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.AllowUnauthenticatedPeers = true
			c.PeerLink.Listener.AllowedNetworks = []string{"10.10.0.0/24"}
		}, ""},
		{"waiver without AllowedNetworks", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.AllowUnauthenticatedPeers = true
		}, "AllowedNetworks"},
		{"waiver with UserManagement", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.AllowUnauthenticatedPeers = true
			c.PeerLink.Listener.AllowedNetworks = []string{"10.10.0.0/24"}
			c.UserManagement.Enabled = true
		}, "UserManagement"},
		{"group secret without TLS", func(c *Config) { c.PeerLink.Tls.Enabled = false }, "SharedSecrets need Tls.Enabled"},
		{"peer secret with dialer TLS off", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].SharedSecrets = []string{testSecret2}
			c.PeerLink.Peers[0].Tls.Enabled = ptr(false)
		}, "need TLS in every direction"},
		{"peer secret with dialer TLS on, node TLS off", func(c *Config) {
			c.PeerLink.Tls = PeerLinkTLS{}
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].SharedSecrets = []string{testSecret2}
			c.PeerLink.Peers[0].Tls.Enabled = ptr(true)
			c.PeerLink.Peers[0].Serve = ptr(false)
		}, ""},
		{"secret too short", func(c *Config) { c.PeerLink.SharedSecrets = []string{"c2hvcnQ="} }, "need at least 16"},
		{"secret not base64", func(c *Config) { c.PeerLink.Peers[0].SharedSecrets = []string{"not base64!"} }, "not base64"},
		{"Tls.Enabled without key pair", func(c *Config) {
			c.PeerLink.Tls.AutoGenerate = false
			c.PeerLink.Tls.CertPath = "certs/peer.pem"
		}, "CertPath and Tls.KeyPath"},
		{"Tls.Enabled with key pair", func(c *Config) {
			c.PeerLink.Tls = PeerLinkTLS{Enabled: true, CertPath: "certs/peer.pem", KeyPath: "certs/peer.key"}
		}, ""},
		{"ClientAuth without trust", func(c *Config) { c.PeerLink.Tls.ClientAuth = ClientAuthRequired }, "needs Tls.TrustStorePath or PinnedSha256"},
		{"ClientAuth with pins", func(c *Config) {
			c.PeerLink.Tls.ClientAuth = ClientAuthRequired
			c.PeerLink.Peers[0].Tls.PinnedSha256 = []string{testPin}
		}, ""},
		{"ClientAuth without TLS", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.Tls.ClientAuth = ClientAuthRequest
			c.PeerLink.Tls.TrustStorePath = "certs/ca.pem"
		}, "Tls.ClientAuth REQUEST needs Tls.Enabled"},
		{"ClientAuth invalid", func(c *Config) { c.PeerLink.Tls.ClientAuth = "SOMETIMES" }, "Tls.ClientAuth"},
		{"TrustStoreType invalid", func(c *Config) { c.PeerLink.Tls.TrustStoreType = "JKS" }, "Tls.TrustStoreType"},
		{"IdentityFallback invalid", func(c *Config) { c.PeerLink.Tls.IdentityFallback = "SAN" }, "Tls.IdentityFallback"},
		{"RequireClientCert with ClientAuth NONE", func(c *Config) {
			c.PeerLink.Peers[0].Tls.RequireClientCert = true
		}, "RequireClientCert needs Tls.ClientAuth"},
		{"mTLS only", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Tls.ClientAuth = ClientAuthRequired
			c.PeerLink.Tls.TrustStorePath = "certs/peer-ca.pem"
		}, ""},
		{"mTLS REQUEST with RequireClientCert", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Tls.ClientAuth = ClientAuthRequest
			c.PeerLink.Tls.TrustStorePath = "certs/peer-ca.pem"
			c.PeerLink.Peers[0].Tls.RequireClientCert = true
		}, ""},
		{"mTLS REQUEST without RequireClientCert", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Tls.ClientAuth = ClientAuthRequest
			c.PeerLink.Tls.TrustStorePath = "certs/peer-ca.pem"
		}, "is not authenticated when it pulls from this node"},
		{"pins without TLS", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.AllowUnauthenticatedPeers = true
			c.PeerLink.Listener.AllowedNetworks = []string{"10.10.0.0/24"}
			c.PeerLink.Peers[0].Tls.PinnedSha256 = []string{testPin}
		}, "PinnedSha256 needs TLS"},
		{"pin malformed", func(c *Config) { c.PeerLink.Peers[0].Tls.PinnedSha256 = []string{"abcd"} }, "64 hex digits"},
		{"pin with colons", func(c *Config) {
			c.PeerLink.Peers[0].Tls.PinnedSha256 = []string{"6A:83:94:FB:60:35:EF:9C:36:64:7D:01:C8:A2:79:2B:B5:F8:E9:19:73:B1:3E:51:4A:5B:23:64:F3:22:77:55"}
		}, ""},
		{"dialer TLS without any trust", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].Serve = ptr(false)
		}, "set Tls.InsecureSkipVerify"},
		{"InsecureSkipVerify without secret", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].Serve = ptr(false)
			c.PeerLink.Peers[0].Tls.InsecureSkipVerify = true
		}, "is not authenticated when this node pulls from it"},
		{"InsecureSkipVerify with secret", func(c *Config) {
			c.PeerLink.Peers[0].Tls.InsecureSkipVerify = true
		}, ""},
		{"InsecureSkipVerify with waiver", func(c *Config) {
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].Serve = ptr(false)
			c.PeerLink.Peers[0].Tls.InsecureSkipVerify = true
			c.PeerLink.AllowUnauthenticatedPeers = true
			c.PeerLink.Listener.AllowedNetworks = []string{"10.10.0.0/24"}
		}, ""},
		{"AllowPlaintext without waiver", func(c *Config) { c.PeerLink.Listener.AllowPlaintext = true }, "AllowPlaintext"},
		{"AllowPlaintext with only the own entry serving", func(c *Config) {
			c.PeerLink.Listener.AllowPlaintext = true
			c.PeerLink.Peers[0].Serve = ptr(false)
			c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: "node-a"})
		}, ""},

		// 8. Bounds.
		{"MaxMessages below 100", func(c *Config) {
			c.PeerLink.Log.MaxMessages = ptr(50)
			c.PeerLink.Fetch.MaxRecords = ptr(10)
		}, "Log.MaxMessages"},
		{"MaxMessages below Fetch.MaxRecords", func(c *Config) { c.PeerLink.Log.MaxMessages = ptr(1000) }, "Log.MaxMessages"},
		{"MaxBytes below 1 MiB", func(c *Config) {
			c.PeerLink.Log.MaxBytes = ptr(int64(512 << 10))
			c.PeerLink.Log.MaxRecordBytes = 1024
		}, "Log.MaxBytes"},
		{"MaxBytes below 4 records", func(c *Config) { c.PeerLink.Log.MaxBytes = ptr(int64(4 << 20)) }, "Log.MaxBytes"},
		{"MaxBytes exactly 4 records", func(c *Config) { c.PeerLink.Log.MaxBytes = ptr(int64(4 * (1<<20 + 64<<10))) }, ""},
		{"MaxRecordBytes negative", func(c *Config) { c.PeerLink.Log.MaxRecordBytes = -1 }, "Log.MaxRecordBytes"},
		{"DrainOnShutdownMs negative", func(c *Config) { c.PeerLink.Log.DrainOnShutdownMs = ptr(-1) }, "DrainOnShutdownMs"},
		{"DrainOnShutdownMs off", func(c *Config) { c.PeerLink.Log.DrainOnShutdownMs = ptr(0) }, ""},
		{"Pipeline 0", func(c *Config) { c.PeerLink.Fetch.Pipeline = ptr(0) }, "Fetch.Pipeline"},
		{"Pipeline 3", func(c *Config) { c.PeerLink.Fetch.Pipeline = ptr(3) }, "Fetch.Pipeline"},
		{"Pipeline 2", func(c *Config) { c.PeerLink.Fetch.Pipeline = ptr(2) }, ""},
		{"MaxWaitMs not below keepalive", func(c *Config) { c.PeerLink.Fetch.MaxWaitMs = ptr(10000) }, "Fetch.MaxWaitMs"},
		{"MaxWaitMs 0 busy loop", func(c *Config) { c.PeerLink.Fetch.MaxWaitMs = ptr(0) }, "Fetch.MaxWaitMs"},
		{"MaxWaitMs below floor", func(c *Config) { c.PeerLink.Fetch.MaxWaitMs = ptr(9) }, "Fetch.MaxWaitMs"},
		{"MaxWaitMs with longer keepalive", func(c *Config) {
			c.PeerLink.Fetch.MaxWaitMs = ptr(10000)
			c.PeerLink.KeepAliveSeconds = ptr(11)
		}, ""},
		{"KeepAliveSeconds 0", func(c *Config) { c.PeerLink.KeepAliveSeconds = ptr(0) }, "KeepAliveSeconds"},
		{"CatchUpRateFactor 1.2", func(c *Config) { c.PeerLink.Receive.CatchUpRateFactor = ptr(1.2) }, "CatchUpRateFactor"},
		{"CatchUpRateFactor negative", func(c *Config) { c.PeerLink.Receive.CatchUpRateFactor = ptr(-1.0) }, "CatchUpRateFactor"},
		{"CatchUpRateFactor 0", func(c *Config) { c.PeerLink.Receive.CatchUpRateFactor = ptr(0.0) }, ""},
		{"CatchUpRateFactor 1.5", func(c *Config) { c.PeerLink.Receive.CatchUpRateFactor = ptr(1.5) }, ""},
		{"InjectWorkers 0", func(c *Config) { c.PeerLink.Receive.InjectWorkers = ptr(0) }, "InjectWorkers"},
		{"InjectWorkers 17", func(c *Config) { c.PeerLink.Receive.InjectWorkers = ptr(17) }, "InjectWorkers"},
		{"InjectWorkers 16", func(c *Config) { c.PeerLink.Receive.InjectWorkers = ptr(16) }, ""},

		// Interest routing.
		{"Interest enabled", func(c *Config) { c.PeerLink.Interest.Enabled = ptr(true) }, ""},
		{"Interest disabled", func(c *Config) { c.PeerLink.Interest.Enabled = ptr(false) }, ""},
		{"Interest.Unknown", func(c *Config) { c.PeerLink.Interest.Unknown = "SOME" }, "Interest.Unknown"},
		{"Interest.Unknown NONE", func(c *Config) { c.PeerLink.Interest.Unknown = "NONE" }, ""},
		{"Interest.FlushMs 0", func(c *Config) { c.PeerLink.Interest.FlushMs = ptr(0) }, "Interest.FlushMs"},
		{"Interest.MaxScanPerFetch 1023", func(c *Config) { c.PeerLink.Interest.MaxScanPerFetch = ptr(1023) }, "MaxScanPerFetch"},
		{"Interest.MaxScanPerFetch 1024", func(c *Config) { c.PeerLink.Interest.MaxScanPerFetch = ptr(1024) }, ""},
		{"Interest.MaxFilterBytes 0", func(c *Config) { c.PeerLink.Interest.MaxFilterBytes = ptr(0) }, "MaxFilterBytes"},
		{"Interest.MaxFilterBytes 32769", func(c *Config) { c.PeerLink.Interest.MaxFilterBytes = ptr(32769) }, "MaxFilterBytes"},
		{"Interest.MaxFiltersPerPeer 0", func(c *Config) { c.PeerLink.Interest.MaxFiltersPerPeer = ptr(0) }, "MaxFiltersPerPeer"},
		{"peer Interest invalid", func(c *Config) { c.PeerLink.Peers[0].Interest = "ON" }, "Peers[0] (node-b).Interest"},
		{"peer Interest OFF", func(c *Config) { c.PeerLink.Peers[0].Interest = "OFF" }, ""},
		{"Interest with 65 consumers", func(c *Config) { interestPeers(c, 65, true) }, "at most 64 consumers"},
		{"Interest with 64 consumers", func(c *Config) { interestPeers(c, 64, true) }, ""},
		{"65 consumers without Interest", func(c *Config) { interestPeers(c, 65, false) }, ""},
		{"MaxFrameBytes below a fetch", func(c *Config) { c.PeerLink.Receive.MaxFrameBytes = ptr(1 << 20) }, "MaxFrameBytes"},
		{"Fetch.MaxRecords 0", func(c *Config) { c.PeerLink.Fetch.MaxRecords = ptr(0) }, "Fetch.MaxRecords"},
		{"Snapshot.Mode invalid", func(c *Config) { c.PeerLink.Snapshot.Mode = "ALL" }, "Snapshot.Mode"},
		{"SharedSubscriptions invalid", func(c *Config) { c.PeerLink.Receive.SharedSubscriptions = "DROP" }, "SharedSubscriptions"},
		{"Capture.Include hash inside", func(c *Config) { c.PeerLink.Capture.Include = []string{"a/#/b"} }, "Capture.Include[0]"},
		{"Capture.Include empty filter", func(c *Config) { c.PeerLink.Capture.Include = []string{"plant/#", ""} }, "Capture.Include[1]"},
		{"Capture.Exclude partial plus", func(c *Config) { c.PeerLink.Capture.Exclude = &[]string{"a+/b"} }, "Capture.Exclude[0]"},
		{"Capture.Exclude none", func(c *Config) { c.PeerLink.Capture.Exclude = &[]string{} }, ""},
		{"Capture filters valid", func(c *Config) {
			c.PeerLink.Capture.Include = []string{"plant/#", "+/status", "#"}
			c.PeerLink.Capture.Exclude = &[]string{"plant/+/debug/#", "monstermq/hmi/sync/#"}
		}, ""},
		{"peer Receive.Exclude invalid", func(c *Config) { c.PeerLink.Peers[0].Receive.Exclude = []string{"#/x"} }, "Receive.Exclude[0]"},
		{"peer Receive.Include invalid", func(c *Config) { c.PeerLink.Peers[0].Receive.Include = []string{"a/b#"} }, "Receive.Include[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validPeerLink()
			tc.mod(c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func interestPeers(c *Config, n int, enabled bool) {
	c.PeerLink.Interest.Enabled = ptr(enabled)
	c.PeerLink.Peers = nil
	for i := range n {
		c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: fmt.Sprintf("node-%d", i+100)})
	}
}

func TestPeerLinkSecretNotInError(t *testing.T) {
	c := validPeerLink()
	c.PeerLink.SharedSecrets = []string{"dG9vLXNob3J0"} // "too-short"
	err := c.Validate()
	if err == nil || strings.Contains(err.Error(), "dG9vLXNob3J0") {
		t.Fatalf("want an error without the secret, got %v", err)
	}
}

func TestPeerLinkNodeIDFallback(t *testing.T) {
	withHostname(t, "", errors.New("no hostname"))

	c := Default()
	if err := c.Validate(); err != nil || c.NodeID != "edge" {
		t.Fatalf("without PeerLink the fallback stays: %v %q", err, c.NodeID)
	}

	c = validPeerLink()
	c.NodeID = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("the 'edge' fallback must be rejected with PeerLink, got %v", err)
	}
	// Validate filled NodeID; a second call must still know its origin.
	if err := c.Validate(); err == nil {
		t.Fatal("second Validate accepted the fallback")
	}

	c = validPeerLink()
	c.NodeID = "edge"
	if err := c.Validate(); err != nil {
		t.Fatalf("an explicit NodeId 'edge' is a choice, not the fallback: %v", err)
	}
}

func TestPeerLinkOwnEntry(t *testing.T) {
	twoPeers := func(c *Config, a, b string) {
		c.PeerLink.Peers = []PeerConfig{{NodeID: a, Address: a + ":1890"}, {NodeID: b, Address: b + ":1890"}}
	}

	t.Run("explicit NodeId, case-insensitive", func(t *testing.T) {
		withHostname(t, "build-host", nil)
		c := validPeerLink()
		c.NodeID = "Node-A"
		twoPeers(c, "NODE-A", "Node-B")
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		s, err := c.ResolvePeerLink()
		if err != nil {
			t.Fatal(err)
		}
		if s.NodeID != "node-a" || len(s.Peers) != 1 || s.Peers[0].NodeID != "node-b" {
			t.Fatalf("setup %+v", s)
		}
		if len(s.Infos) != 1 || !strings.Contains(s.Infos[0], `"NODE-A"`) || len(s.Warnings) != 0 {
			t.Fatalf("infos %q warnings %q", s.Infos, s.Warnings)
		}
		if c.PeerLink.Peers[1].NodeID != "Node-B" || len(c.PeerLink.Peers) != 2 {
			t.Fatal("ResolvePeerLink must not change the configuration")
		}
	})

	t.Run("NodeId from hostname matches the first label", func(t *testing.T) {
		withHostname(t, "OA-Host-A.plant.local", nil)
		c := validPeerLink()
		c.NodeID = ""
		twoPeers(c, "oa-host-a", "oa-host-b")
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.NodeID != "OA-Host-A.plant.local" {
			t.Fatalf("NodeID %q", c.NodeID)
		}
		s, err := c.ResolvePeerLink()
		if err != nil {
			t.Fatal(err)
		}
		// The link adopts the entry's short id, which the peers of the shared file use (finding 22).
		if s.NodeID != "oa-host-a" || len(s.Peers) != 1 || s.Peers[0].NodeID != "oa-host-b" || len(s.Warnings) != 0 {
			t.Fatalf("setup %+v", s)
		}
		if len(s.Infos) != 2 || !strings.Contains(s.Infos[1], `"oa-host-a"`) {
			t.Fatalf("infos %q", s.Infos)
		}
	})

	t.Run("config built in code without Validate", func(t *testing.T) {
		withHostname(t, "oa-host-b", nil)
		c := validPeerLink()
		c.NodeID = ""
		twoPeers(c, "oa-host-a", "oa-host-b")
		s, err := c.ResolvePeerLink()
		if err != nil {
			t.Fatal(err)
		}
		if s.NodeID != "oa-host-b" || len(s.Peers) != 1 || s.Peers[0].NodeID != "oa-host-a" {
			t.Fatalf("setup %+v", s)
		}
	})

	t.Run("explicit NodeId does not match the hostname label", func(t *testing.T) {
		withHostname(t, "node-b.plant.local", nil)
		c := validPeerLink()
		c.NodeID = "node-x"
		twoPeers(c, "node-b", "node-c")
		c.PeerLink.SharedSecrets = nil
		c.PeerLink.Peers[0].SharedSecrets = []string{testSecret}
		c.PeerLink.Peers[1].SharedSecrets = []string{testSecret2}
		s, err := c.ResolvePeerLink()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Peers) != 2 {
			t.Fatalf("peers %+v", s.Peers)
		}
		if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "node-b.plant.local") || !strings.Contains(s.Warnings[0], "no Peers entry matches") {
			t.Fatalf("warnings %q", s.Warnings)
		}
	})

	t.Run("single entry without match is a per-host file", func(t *testing.T) {
		withHostname(t, "build-host", nil)
		s, err := validPeerLink().ResolvePeerLink()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Peers) != 1 || len(s.Warnings) != 0 || len(s.Infos) != 0 {
			t.Fatalf("setup %+v", s)
		}
	})
}

func TestPeerLinkWarnings(t *testing.T) {
	withHostname(t, "build-host", nil)
	cases := []struct {
		name string
		mod  func(*Config)
		want string // empty = no warning
	}{
		{"none", func(*Config) {}, ""},
		{"waiver", func(c *Config) {
			c.PeerLink.Tls.Enabled, c.PeerLink.SharedSecrets = false, nil
			c.PeerLink.AllowUnauthenticatedPeers = true
			c.PeerLink.Listener.AllowedNetworks = []string{"10.10.0.0/24"}
		}, "AllowUnauthenticatedPeers"},
		{"group secret with three nodes", func(c *Config) {
			c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: "node-c", Address: "node-c:1890"}, PeerConfig{NodeID: "node-a"})
		}, "more than two nodes"},
		{"per-peer secrets with three nodes", func(c *Config) {
			c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: "node-c", Address: "node-c:1890"}, PeerConfig{NodeID: "node-a"})
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].SharedSecrets = []string{testSecret}
			c.PeerLink.Peers[1].SharedSecrets = []string{testSecret2}
		}, ""},
		{"mesh file without own entry", func(c *Config) {
			c.PeerLink.Peers = append(c.PeerLink.Peers, PeerConfig{NodeID: "node-c", Address: "node-c:1890", SharedSecrets: []string{testSecret2}})
			c.PeerLink.SharedSecrets = nil
			c.PeerLink.Peers[0].SharedSecrets = []string{testSecret}
		}, "no Peers entry matches this node"},
		{"truststore shared with TCPS", func(c *Config) {
			c.SSL.TrustStorePath = "certs/ca.pem"
			c.PeerLink.Tls.TrustStorePath = "./certs/ca.pem"
		}, "TCPS truststore"},
		{"MEMORY retained without snapshot", func(c *Config) {
			c.RetainedStoreType = StoreMemory
			c.PeerLink.Snapshot.Mode = PeerLinkSnapshotOff
		}, "lost when this node restarts"},
		{"memory limit too low", func(c *Config) { c.Runtime.MemoryLimitMB = 512 }, "Runtime.MemoryLimitMB"},
		{"memory limit sufficient", func(c *Config) { c.Runtime.MemoryLimitMB = 1024 }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validPeerLink()
			tc.mod(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			s, err := c.ResolvePeerLink()
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(s.Warnings) != 0 {
					t.Fatalf("unexpected warnings %q", s.Warnings)
				}
				return
			}
			if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], tc.want) {
				t.Fatalf("warnings %q, want one containing %q", s.Warnings, tc.want)
			}
		})
	}
}

func TestRuntimeMemoryLimit(t *testing.T) {
	c := Default()
	c.Runtime.MemoryLimitMB = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "Runtime.MemoryLimitMB") {
		t.Fatalf("negative MemoryLimitMB: %v", err)
	}
	cfg, err := Load(writeConfig(t, "Runtime:\n  MemoryLimitMB: 768\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.MemoryLimitBytes() != 768<<20 {
		t.Fatalf("MemoryLimitBytes %d", cfg.Runtime.MemoryLimitBytes())
	}
}

// A nil []string marshals as [] and reads back empty, so Capture.Exclude is a
// *[]string: YAML null (default exclusion) and [] (no exclusion) must survive
// YAML -> struct -> YAML -> struct.
func TestPeerLinkRoundTripExclude(t *testing.T) {
	const hmi = "monstermq/hmi/sync"
	for _, tc := range []struct {
		name    string
		capture string
		isNil   bool
		want    []string
	}{
		{"null", "    Exclude: null\n", true, []string{hmi + "/#"}},
		{"omitted", "    Wills: true\n", true, []string{hmi + "/#"}},
		{"empty", "    Exclude: []\n", false, []string{}},
		{"set", "    Exclude: [plant/debug/#]\n", false, []string{"plant/debug/#"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, "PeerLink:\n  Capture:\n"+tc.capture))
			if err != nil {
				t.Fatal(err)
			}
			first := cfg.PeerLink
			out, err := yaml.Marshal(&first)
			if err != nil {
				t.Fatal(err)
			}
			if wantText := map[string]string{"null": "Exclude: null", "omitted": "Exclude: null", "empty": "Exclude: []"}[tc.name]; !strings.Contains(string(out), wantText) {
				t.Fatalf("marshalled without %q:\n%s", wantText, out)
			}
			var second PeerLinkConfig
			if err := yaml.Unmarshal(out, &second); err != nil {
				t.Fatal(err)
			}
			for i, p := range []PeerLinkConfig{first, second} {
				if (p.Capture.Exclude == nil) != tc.isNil {
					t.Fatalf("pass %d: Exclude nil = %v, want %v\n%s", i, p.Capture.Exclude == nil, tc.isNil, out)
				}
				if got := p.Capture.GetExclude(hmi); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("pass %d: GetExclude = %#v, want %#v", i, got, tc.want)
				}
			}
		})
	}
}

const fixturePath = "../../test/integration/testdata/peerlink-full.yaml"

func TestPeerLinkFixture(t *testing.T) {
	withHostname(t, "build-host", nil)
	cfg, err := Load(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	p := &cfg.PeerLink
	if !p.Enabled || !p.Tls.Enabled || p.Tls.GetClientAuth() != ClientAuthRequired || p.Fetch.GetPipeline() != 2 ||
		p.Receive.GetArchive() || !p.Receive.GetQueue() || p.Receive.GetCatchUpRateFactor() != 2.5 || p.Listener.GetMaxPreAuthPerIp() != 4 {
		t.Fatalf("fixture not decoded: %+v", p)
	}
	s, err := cfg.ResolvePeerLink()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, peer := range s.Peers {
		ids = append(ids, peer.NodeID)
	}
	if s.NodeID != "oa-host-a" || !reflect.DeepEqual(ids, []string{"oa-host-b", "oa-host-c", "oa-host-d"}) {
		t.Fatalf("NodeId %q peers %v", s.NodeID, ids)
	}
	if len(s.Infos) != 1 || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "more than two nodes") {
		t.Fatalf("infos %q warnings %q", s.Infos, s.Warnings)
	}
	if !s.AnyServe() {
		t.Error("AnyServe")
	}
	in := p.Interest
	if !in.GetEnabled() || in.GetUnknown() != PeerLinkInterestNone || in.GetFlushMs() != 10 || in.GetMaxScanPerFetch() != 32768 ||
		in.GetMaxFiltersPerPeer() != 5000 || in.GetMaxFilterBytes() != 512 {
		t.Errorf("Interest: %+v", in)
	}
	b, c, d := s.Peers[0], s.Peers[1], s.Peers[2]
	if !p.InterestOn(b) || p.InterestOn(c) || c.GetInterest() != PeerLinkInterestOff {
		t.Errorf("per-peer Interest: b %q c %q", b.Interest, c.Interest)
	}
	if !b.Pulls() || !b.GetServe() || !p.DialerTLS(b) || len(p.SecretsFor(b)) != 1 {
		t.Errorf("oa-host-b: %+v", b)
	}
	if !c.Pulls() || c.GetServe() || c.Tls.Enabled != nil || !p.DialerTLS(c) || len(p.SecretsFor(c)) != 2 {
		t.Errorf("oa-host-c: %+v", c)
	}
	if d.Pulls() || !d.GetServe() || !d.Tls.RequireClientCert {
		t.Errorf("oa-host-d: %+v", d)
	}
}

// YAML -> struct -> YAML -> struct over every key: the second YAML equals the
// first, it decodes strictly, and Exclude keeps its nil-ness.
func TestPeerLinkRoundTripFull(t *testing.T) {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		t.Fatal(err)
	}
	example := Default()
	exampleData, err := os.ReadFile("../../config.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(exampleData, example); err != nil {
		t.Fatal(err)
	}
	for name, first := range map[string]PeerLinkConfig{"fixture": cfg.PeerLink, "example": example.PeerLink} {
		t.Run(name, func(t *testing.T) {
			out1, err := yaml.Marshal(map[string]any{"PeerLink": first})
			if err != nil {
				t.Fatal(err)
			}
			if err := decodePeerLinkStrict(out1); err != nil {
				t.Fatalf("marshalled section does not decode strictly: %v\n%s", err, out1)
			}
			var doc struct {
				PeerLink PeerLinkConfig `yaml:"PeerLink"`
			}
			if err := yaml.Unmarshal(out1, &doc); err != nil {
				t.Fatal(err)
			}
			out2, err := yaml.Marshal(map[string]any{"PeerLink": doc.PeerLink})
			if err != nil {
				t.Fatal(err)
			}
			if string(out1) != string(out2) {
				t.Fatalf("round trip changed the section:\n%s\n---\n%s", out1, out2)
			}
			if (first.Capture.Exclude == nil) != (doc.PeerLink.Capture.Exclude == nil) {
				t.Fatal("Capture.Exclude nil-ness changed")
			}
		})
	}
	if example.PeerLink.Capture.Exclude != nil || cfg.PeerLink.Capture.Exclude == nil {
		t.Fatal("the example keeps the default exclusion (null), the fixture sets one")
	}
}
