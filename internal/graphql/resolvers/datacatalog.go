package resolvers

import (
	"context"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
)

func (r *queryResolver) DataCatalogTypes(ctx context.Context, namespace *string) ([]*stores.DataCatalogType, error) {
	res, err := r.Storage.DataCatalog.GetTypes(namespace)
	if err != nil {
		return nil, err
	}
	out := make([]*stores.DataCatalogType, len(res))
	for i := range res {
		out[i] = &res[i]
	}
	return out, nil
}

func (r *queryResolver) DataCatalogType(ctx context.Context, id string) (*stores.DataCatalogType, error) {
	return r.Storage.DataCatalog.GetType(id)
}

func (r *queryResolver) DataCatalogInstances(ctx context.Context, typeID *string) ([]*stores.DataCatalogInstance, error) {
	res, err := r.Storage.DataCatalog.GetInstances(typeID)
	if err != nil {
		return nil, err
	}
	out := make([]*stores.DataCatalogInstance, len(res))
	for i := range res {
		out[i] = &res[i]
	}
	return out, nil
}

func (r *queryResolver) DataCatalogInstance(ctx context.Context, id string) (*stores.DataCatalogInstance, error) {
	return r.Storage.DataCatalog.GetInstance(id)
}

func (r *queryResolver) DataCatalogRelations(ctx context.Context, sourceID *string, targetID *string, relationType *string) ([]*stores.DataCatalogRelation, error) {
	res, err := r.Storage.DataCatalog.GetRelations(sourceID, targetID, relationType)
	if err != nil {
		return nil, err
	}
	out := make([]*stores.DataCatalogRelation, len(res))
	for i := range res {
		out[i] = &res[i]
	}
	return out, nil
}

func (r *queryResolver) InferDataCatalog(ctx context.Context, topicPattern string, archiveGroup *string) (*generated.DataCatalogProposal, error) {
	return &generated.DataCatalogProposal{
		Types:          []*stores.DataCatalogType{},
		Instances:      []*stores.DataCatalogInstance{},
		Relations:      []*stores.DataCatalogRelation{},
		TopicsAnalyzed: 0,
	}, nil
}

func (r *mutationResolver) DataCatalog(ctx context.Context) (*generated.DataCatalogMutations, error) {
	return &generated.DataCatalogMutations{}, nil
}

type dataCatalogMutationsResolver struct{ *Resolver }

func (r *dataCatalogMutationsResolver) SaveType(ctx context.Context, obj *generated.DataCatalogMutations, input generated.DataCatalogTypeInput) (*stores.DataCatalogType, error) {
	t := stores.DataCatalogType{
		ID:           input.ID,
		Namespace:    input.Namespace,
		Name:         input.Name,
		Description:  input.Description,
		Structure:    input.Structure,
		TopicPattern: input.TopicPattern,
	}
	return r.Storage.DataCatalog.SaveType(t)
}

func (r *dataCatalogMutationsResolver) DeleteType(ctx context.Context, obj *generated.DataCatalogMutations, id string) (bool, error) {
	return r.Storage.DataCatalog.DeleteType(id)
}

func (r *dataCatalogMutationsResolver) SaveInstance(ctx context.Context, obj *generated.DataCatalogMutations, input generated.DataCatalogInstanceInput) (*stores.DataCatalogInstance, error) {
	i := stores.DataCatalogInstance{
		ID:         input.ID,
		TypeID:     input.TypeID,
		Name:       input.Name,
		BaseTopic:  input.BaseTopic,
		Properties: input.Properties,
	}
	return r.Storage.DataCatalog.SaveInstance(i)
}

func (r *dataCatalogMutationsResolver) DeleteInstance(ctx context.Context, obj *generated.DataCatalogMutations, id string) (bool, error) {
	return r.Storage.DataCatalog.DeleteInstance(id)
}

func (r *dataCatalogMutationsResolver) SaveRelation(ctx context.Context, obj *generated.DataCatalogMutations, input generated.DataCatalogRelationInput) (*stores.DataCatalogRelation, error) {
	rel := stores.DataCatalogRelation{
		SourceID:     input.SourceID,
		TargetID:     input.TargetID,
		RelationType: input.RelationType,
	}
	return r.Storage.DataCatalog.SaveRelation(rel)
}

func (r *dataCatalogMutationsResolver) DeleteRelation(ctx context.Context, obj *generated.DataCatalogMutations, sourceID string, targetID string, relationType string) (bool, error) {
	return r.Storage.DataCatalog.DeleteRelation(sourceID, targetID, relationType)
}

func (r *dataCatalogMutationsResolver) ExportCatalog(ctx context.Context, obj *generated.DataCatalogMutations, namespace *string) (map[string]any, error) {
	return r.Storage.DataCatalog.ExportCatalog(namespace)
}

func (r *dataCatalogMutationsResolver) ImportCatalog(ctx context.Context, obj *generated.DataCatalogMutations, data map[string]any) (*stores.ImportDataCatalogResult, error) {
	return r.Storage.DataCatalog.ImportCatalog(data)
}

func (r *Resolver) DataCatalogMutations() generated.DataCatalogMutationsResolver {
	return &dataCatalogMutationsResolver{r}
}
