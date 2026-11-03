package mqtt

import (
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/virtualtrees"
)

// SetVirtualTrees installs the source of the active virtual topic tree
// snapshot. Set before Serve; nil disables virtual routing.
func (s *Server) SetVirtualTrees(fn func() *virtualtrees.Resolver) {
	s.virtual = fn
}

// VirtualTrees returns the active virtual topic tree snapshot, or nil when no
// tree is configured.
func (s *Server) VirtualTrees() *virtualtrees.Resolver {
	if s.virtual == nil {
		return nil
	}
	r := s.virtual()
	if r == nil || r.IsEmpty() {
		return nil
	}
	return r
}

// resolveVirtualPublish maps a publish to a virtual topic onto its canonical
// topic. It returns a non-success code when the publish must be refused.
func (s *Server) resolveVirtualPublish(cl *Client, pk *packets.Packet) packets.Code {
	if pk.Forward != nil {
		return packets.CodeSuccess
	}
	vr := s.VirtualTrees()
	if vr == nil || !vr.InVirtualNamespace(pk.TopicName) {
		return packets.CodeSuccess
	}
	canonical, reject := vr.Publish(pk.TopicName)
	if reject != "" {
		return packets.ErrTopicNameInvalid
	}
	if !cl.Net.Inline && !s.hooks.OnACLCheck(cl, canonical, true) {
		return packets.ErrNotAuthorized
	}
	pk.TopicName = canonical
	return packets.CodeSuccess
}

// publishToSubscribers delivers a canonical message under each of its
// presentation names: the topic itself outside reserved roots plus every
// virtual name projected by the active trees.
func (s *Server) publishToSubscribers(pk packets.Packet) {
	vr := s.VirtualTrees()
	if vr == nil || pk.Ignore || len(pk.TopicName) == 0 {
		s.deliverToSubscribers(pk, true)
		return
	}
	canonical := pk.TopicName
	for _, name := range vr.Presentations(canonical) {
		if name == canonical {
			s.deliverToSubscribers(pk, true)
			continue
		}
		p := pk
		p.TopicName = name
		p.Canonical = canonical
		s.deliverToSubscribers(p, false)
	}
}

// selectRetained returns the retained messages for a filter as stored.
func (s *Server) selectRetained(filter string) ([]packets.Packet, error) {
	if s.hooks.Provides(OnSelectRetainedMessages) {
		return s.hooks.OnSelectRetainedMessages(filter)
	}
	return s.Topics.Messages(filter), nil
}

// selectPresentedRetained returns the retained messages a subscription with
// the filter sees, renamed to their presentation names.
func (s *Server) selectPresentedRetained(filter string) ([]packets.Packet, error) {
	vr := s.VirtualTrees()
	if vr == nil {
		return s.selectRetained(filter)
	}
	var out []packets.Packet
	var firstErr error
	virtualtrees.FindPresented(vr, filter,
		func(f string, cb func(packets.Packet) bool) {
			pks, err := s.selectRetained(f)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, p := range pks {
				if !cb(p) {
					return
				}
			}
		},
		func(p packets.Packet) string { return p.TopicName },
		func(name string, p packets.Packet) bool {
			if name != p.TopicName {
				p.Canonical = p.TopicName
				p.TopicName = name
			}
			out = append(out, p)
			return true
		})
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
