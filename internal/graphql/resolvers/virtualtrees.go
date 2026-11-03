package resolvers

import (
	"context"
	"encoding/json"
	"fmt"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/virtualtrees"
)

const vtFeatureDisabled = "VirtualTopicTrees feature is disabled"

func (r *Resolver) vtManager() *virtualtrees.Manager {
	if !r.Cfg.Features.VirtualTopicTrees {
		return nil
	}
	return r.VirtualTrees
}

// vtResolver is the active routing snapshot (empty when the feature is off).
func (r *Resolver) vtResolver() *virtualtrees.Resolver {
	return r.vtManager().Resolver()
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func vtIssue(i virtualtrees.Issue) *generated.VirtualTopicIssue {
	return &generated.VirtualTopicIssue{Code: i.Code, NodeID: strPtr(i.NodeID), Path: strPtr(i.Path), Message: i.Message}
}

func vtIssues(in []virtualtrees.Issue) []*generated.VirtualTopicIssue {
	out := make([]*generated.VirtualTopicIssue, 0, len(in))
	for _, i := range in {
		out = append(out, vtIssue(i))
	}
	return out
}

func vtSingle(code, msg string) []*generated.VirtualTopicIssue {
	return []*generated.VirtualTopicIssue{{Code: code, Message: msg}}
}

func vtTreeGQL(t *virtualtrees.Tree) *generated.VirtualTopicTree {
	if t == nil {
		return nil
	}
	out := &generated.VirtualTopicTree{
		SchemaVersion: t.SchemaVersion,
		ID:            t.ID,
		Name:          t.Name,
		RootPath:      t.RootPath,
		Description:   strPtr(t.Description),
		Enabled:       t.Enabled,
		Revision:      t.Revision,
		Nodes:         make([]*generated.VirtualTopicNode, 0, len(t.Nodes)),
	}
	for _, n := range t.Nodes {
		gn := &generated.VirtualTopicNode{ID: n.ID, Segment: n.Segment, Description: strPtr(n.Description)}
		if n.ParentID != nil {
			p := *n.ParentID
			gn.ParentID = &p
		}
		if n.Binding != nil {
			gn.Binding = &generated.VirtualTopicBinding{
				Kind:   generated.VirtualTopicBindingKind(n.Binding.Kind),
				Target: n.Binding.Target,
				Access: generated.VirtualTopicAccess(n.Binding.Access),
			}
		}
		out.Nodes = append(out.Nodes, gn)
	}
	return out
}

// vtParseInput converts the GraphQL input through the portable document
// parser so that GraphQL and import share defaults and normalization.
func vtParseInput(in generated.VirtualTopicTreeInput) (*virtualtrees.Tree, error) {
	if in.Nodes == nil {
		in.Nodes = []*generated.VirtualTopicNodeInput{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return virtualtrees.ParseJSON(b)
}

func vtMutation(res virtualtrees.MutationResult) *generated.VirtualTopicTreeMutationResult {
	out := &generated.VirtualTopicTreeMutationResult{Success: res.Success, Tree: vtTreeGQL(res.Tree), Errors: vtIssues(res.Errors)}
	if res.Success && res.Tree != nil {
		rev := res.Tree.Revision
		out.AppliedRevision = &rev
	}
	return out
}

func vtFailure(code, msg string) *generated.VirtualTopicTreeMutationResult {
	return &generated.VirtualTopicTreeMutationResult{Errors: vtSingle(code, msg)}
}

func (r *queryResolver) VirtualTopicTrees(ctx context.Context) ([]*generated.VirtualTopicTree, error) {
	out := []*generated.VirtualTopicTree{}
	m := r.vtManager()
	if m == nil {
		return out, nil
	}
	for _, t := range m.Trees() {
		out = append(out, vtTreeGQL(t))
	}
	return out, nil
}

func (r *queryResolver) VirtualTopicTree(ctx context.Context, id string) (*generated.VirtualTopicTree, error) {
	m := r.vtManager()
	if m == nil {
		return nil, nil
	}
	return vtTreeGQL(m.Tree(id)), nil
}

func (r *queryResolver) ValidateVirtualTopicTree(ctx context.Context, input generated.VirtualTopicTreeInput) (*generated.VirtualTopicValidationResult, error) {
	empty := []*generated.VirtualTopicIssue{}
	m := r.vtManager()
	if m == nil {
		return &generated.VirtualTopicValidationResult{Errors: vtSingle(virtualtrees.CodeFeatureDisabled, vtFeatureDisabled), Warnings: empty}, nil
	}
	t, err := vtParseInput(input)
	if err != nil {
		return &generated.VirtualTopicValidationResult{Errors: vtSingle(virtualtrees.CodeInvalidInput, err.Error()), Warnings: empty}, nil
	}
	errs := m.Validate(t)
	return &generated.VirtualTopicValidationResult{Valid: len(errs) == 0, Errors: vtIssues(errs), Warnings: vtIssues(m.Warnings(ctx, t))}, nil
}

func (r *queryResolver) PreviewVirtualTopicTree(ctx context.Context, input generated.VirtualTopicTreeInput, topicOrFilter string, operation generated.VirtualTopicOperation) (*generated.VirtualTopicPreviewResult, error) {
	routes := []*generated.VirtualTopicRoute{}
	m := r.vtManager()
	if m == nil {
		return &generated.VirtualTopicPreviewResult{Routes: routes, Errors: vtSingle(virtualtrees.CodeFeatureDisabled, vtFeatureDisabled)}, nil
	}
	t, err := vtParseInput(input)
	if err != nil {
		return &generated.VirtualTopicPreviewResult{Routes: routes, Errors: vtSingle(virtualtrees.CodeInvalidInput, err.Error())}, nil
	}
	if errs := m.Validate(t); len(errs) > 0 {
		return &generated.VirtualTopicPreviewResult{Routes: routes, Errors: vtIssues(errs)}, nil
	}
	res := virtualtrees.NewResolver([]*virtualtrees.Tree{t})
	route := func(nodeID, virtual, canonical, access string) *generated.VirtualTopicRoute {
		return &generated.VirtualTopicRoute{NodeID: nodeID, VirtualPath: virtual, CanonicalPath: canonical, Access: generated.VirtualTopicAccess(access)}
	}
	switch operation {
	case generated.VirtualTopicOperationPublish:
		if w := res.WinnerRoute(topicOrFilter); w != nil {
			routes = append(routes, route(w.NodeID, topicOrFilter, virtualtrees.Canonical(topicOrFilter, w), w.Binding.Access))
		}
	case generated.VirtualTopicOperationProject:
		for _, p := range res.Project(topicOrFilter) {
			routes = append(routes, route(p.NodeID, p.Virtual, topicOrFilter, p.Access))
		}
	case generated.VirtualTopicOperationSubscribe:
		for _, rt := range res.SubscribeRoutes(topicOrFilter) {
			canonical := rt.Binding.Target
			if fs := virtualtrees.CanonicalFilters(topicOrFilter, rt); len(fs) > 0 {
				canonical = fs[0]
			}
			routes = append(routes, route(rt.NodeID, rt.Path, canonical, rt.Binding.Access))
		}
	default:
		return nil, fmt.Errorf("unsupported operation '%s'", operation)
	}
	return &generated.VirtualTopicPreviewResult{Routes: routes, Errors: []*generated.VirtualTopicIssue{}}, nil
}

func (r *mutationResolver) SaveVirtualTopicTree(ctx context.Context, input generated.VirtualTopicTreeInput, expectedRevision *string) (*generated.VirtualTopicTreeMutationResult, error) {
	m := r.vtManager()
	if m == nil {
		return vtFailure(virtualtrees.CodeFeatureDisabled, vtFeatureDisabled), nil
	}
	t, err := vtParseInput(input)
	if err != nil {
		return vtFailure(virtualtrees.CodeInvalidInput, err.Error()), nil
	}
	rev := ""
	if expectedRevision != nil {
		rev = *expectedRevision
	}
	return vtMutation(m.Save(ctx, t, rev)), nil
}

func (r *mutationResolver) DeleteVirtualTopicTree(ctx context.Context, id string, expectedRevision string) (*generated.VirtualTopicTreeMutationResult, error) {
	m := r.vtManager()
	if m == nil {
		return vtFailure(virtualtrees.CodeFeatureDisabled, vtFeatureDisabled), nil
	}
	return vtMutation(m.Delete(ctx, id, expectedRevision)), nil
}
