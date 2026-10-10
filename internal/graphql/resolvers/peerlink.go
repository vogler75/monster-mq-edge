package resolvers

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/peerlink"
)

// Query: peerLink -------------------------------------------------------------

func (r *queryResolver) PeerLink(ctx context.Context) (*generated.PeerLinkInfo, error) {
	if r.PeerLinkMgr == nil {
		return &generated.PeerLinkInfo{NodeID: r.NodeID, Peers: []*generated.PeerLinkPeer{}}, nil
	}
	return peerLinkInfo(r.PeerLinkMgr.Peers(), r.PeerLinkMgr.Status(), r.PeerLinkMgr.Serving())
}

// peerLinkInfo merges the configured peers with the source and consumer entries of the status
// document. The documents go through JSON so that they match GET /peerlink/v1/status.
func peerLinkInfo(peers []config.PeerConfig, st peerlink.Status, serving bool) (*generated.PeerLinkInfo, error) {
	doc, err := jsonDocument(st)
	if err != nil {
		return nil, err
	}
	sources := entriesByID(doc["sources"])
	consumers := entriesByID(doc["consumers"])
	info := &generated.PeerLinkInfo{
		Enabled: st.Enabled,
		NodeID:  st.NodeID,
		TLS:     st.TLS,
		Peers:   make([]*generated.PeerLinkPeer, 0, len(peers)),
		Status:  doc,
	}
	if serving {
		info.Listen = ptrIfNotEmpty(st.Listen)
	}
	for _, p := range peers {
		id := strings.ToLower(strings.TrimSpace(p.NodeID))
		peer := &generated.PeerLinkPeer{
			NodeID:   id,
			Pull:     p.Pulls(),
			Serve:    p.GetServe(),
			Interest: strings.ToUpper(p.GetInterest()),
		}
		if p.Pulls() {
			peer.Address = ptr(p.Address)
			peer.Source = sources[id]
			peer.PullState = ptr(stringField(peer.Source, "state", "STOPPED"))
			peer.LastError = ptrIfNotEmpty(stringField(peer.Source, "lastError", ""))
		}
		if p.GetServe() {
			peer.Consumer = consumers[id]
			peer.ServeState = ptr(stringField(peer.Consumer, "state", "NEVER_CONNECTED"))
			peer.Remote = ptrIfNotEmpty(stringField(peer.Consumer, "remote", ""))
		}
		info.Peers = append(info.Peers, peer)
	}
	return info, nil
}

// jsonDocument converts v to a JSON object; numbers stay exact (uint64 epochs).
func jsonDocument(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func entriesByID(list any) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, _ := list.([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			id, _ := m["nodeId"].(string)
			out[id] = m
		}
	}
	return out
}

func stringField(m map[string]any, key, def string) string {
	if s, ok := m[key].(string); ok && s != "" {
		return s
	}
	return def
}
