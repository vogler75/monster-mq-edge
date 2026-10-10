package integration

import (
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
)

const peerLinkGQL = `{ peerLink { enabled nodeId listen tls peers {
	nodeId address pull serve interest pullState serveState remote lastError source consumer } status } }`

// The peerLink query reports the configured peers with link direction and state: A serves B,
// B pulls from A, and B also pulls from an unreachable C.
func TestPeerLinkGraphQLQuery(t *testing.T) {
	gql := func(port int) plOpt {
		return func(c *config.Config) {
			c.GraphQL.Enabled = true
			c.GraphQL.Port = port
			c.GraphQL.TLSPort = 0
		}
	}
	a := startPL(t, "plgqa", 0, 27911, []config.PeerConfig{plPeer("plgqb", 0)}, gql(27912))
	b := startPL(t, "plgqb", 0, 0, []config.PeerConfig{plPullOnly("plgqa", 27911), plPullOnly("plgqc", 27919)}, gql(27913))
	b.waitStreaming("plgqa")
	plEventually(t, 5*time.Second, "A serving B", func() bool { return a.consumer("plgqb").State == "CONNECTED" })
	waitForHTTP(t, "http://127.0.0.1:27912/health")
	waitForHTTP(t, "http://127.0.0.1:27913/health")

	peerLink := func(port string) (map[string]any, []map[string]any) {
		res := gqlQuery(t, "http://127.0.0.1:"+port+"/graphql", peerLinkGQL, nil)
		pl := res["peerLink"].(map[string]any)
		var peers []map[string]any
		for _, p := range pl["peers"].([]any) {
			peers = append(peers, p.(map[string]any))
		}
		return pl, peers
	}

	plA, peersA := peerLink("27912")
	if plA["enabled"] != true || plA["nodeId"] != "plgqa" || plA["listen"] == nil || plA["status"] == nil {
		t.Fatalf("peerLink on A: %v", plA)
	}
	if len(peersA) != 1 {
		t.Fatalf("A peers: %v", peersA)
	}
	pa := peersA[0]
	if pa["nodeId"] != "plgqb" || pa["pull"] != false || pa["serve"] != true || pa["address"] != nil ||
		pa["pullState"] != nil || pa["serveState"] != "CONNECTED" || pa["remote"] == nil ||
		pa["consumer"] == nil || pa["source"] != nil || pa["interest"] != "INHERIT" {
		t.Fatalf("A's peer B: %v", pa)
	}

	plB, peersB := peerLink("27913")
	if plB["enabled"] != true || plB["nodeId"] != "plgqb" || plB["listen"] != nil {
		t.Fatalf("peerLink on B: %v", plB)
	}
	if len(peersB) != 2 {
		t.Fatalf("B peers: %v", peersB)
	}
	pb := peersB[0]
	if pb["nodeId"] != "plgqa" || pb["pull"] != true || pb["serve"] != false || pb["address"] != "127.0.0.1:27911" ||
		pb["pullState"] != "STREAMING" || pb["serveState"] != nil || pb["source"] == nil || pb["consumer"] != nil {
		t.Fatalf("B's peer A: %v", pb)
	}
	pc := peersB[1]
	if pc["nodeId"] != "plgqc" || pc["pull"] != true || pc["pullState"] == "STREAMING" {
		t.Fatalf("B's peer C: %v", pc)
	}
}

// Without PeerLink the query answers enabled false and no peers.
func TestPeerLinkGraphQLQueryDisabled(t *testing.T) {
	startWithGraphQL(t, 27914, 27915, func(c *config.Config) { c.NodeID = "plgqoff" })
	res := gqlQuery(t, "http://127.0.0.1:27915/graphql", peerLinkGQL, nil)
	pl := res["peerLink"].(map[string]any)
	if pl["enabled"] != false || pl["nodeId"] != "plgqoff" || len(pl["peers"].([]any)) != 0 || pl["status"] != nil {
		t.Fatalf("peerLink without PeerLink: %v", pl)
	}
}
