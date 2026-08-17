package resolvers

import (
	"context"
	"time"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
)

type dataCatalogTypeResolver struct{ *Resolver }

func (r *dataCatalogTypeResolver) CreatedAt(ctx context.Context, obj *stores.DataCatalogType) (*string, error) {
	if obj.CreatedAt == nil {
		return nil, nil
	}
	s := obj.CreatedAt.Format(time.RFC3339)
	return &s, nil
}

func (r *dataCatalogTypeResolver) UpdatedAt(ctx context.Context, obj *stores.DataCatalogType) (*string, error) {
	if obj.UpdatedAt == nil {
		return nil, nil
	}
	s := obj.UpdatedAt.Format(time.RFC3339)
	return &s, nil
}

type dataCatalogInstanceResolver struct{ *Resolver }

func (r *dataCatalogInstanceResolver) CreatedAt(ctx context.Context, obj *stores.DataCatalogInstance) (*string, error) {
	if obj.CreatedAt == nil {
		return nil, nil
	}
	s := obj.CreatedAt.Format(time.RFC3339)
	return &s, nil
}

func (r *dataCatalogInstanceResolver) UpdatedAt(ctx context.Context, obj *stores.DataCatalogInstance) (*string, error) {
	if obj.UpdatedAt == nil {
		return nil, nil
	}
	s := obj.UpdatedAt.Format(time.RFC3339)
	return &s, nil
}

func (r *Resolver) DataCatalogType() generated.DataCatalogTypeResolver {
	return &dataCatalogTypeResolver{r}
}

func (r *Resolver) DataCatalogInstance() generated.DataCatalogInstanceResolver {
	return &dataCatalogInstanceResolver{r}
}
