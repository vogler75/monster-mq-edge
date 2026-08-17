package stores

type IDataCatalogStore interface {
	Initialize() error
	Close() error

	// Types
	GetTypes(namespace *string) ([]DataCatalogType, error)
	GetType(id string) (*DataCatalogType, error)
	SaveType(t DataCatalogType) (*DataCatalogType, error)
	DeleteType(id string) (bool, error)

	// Instances
	GetInstances(typeID *string) ([]DataCatalogInstance, error)
	GetInstance(id string) (*DataCatalogInstance, error)
	SaveInstance(i DataCatalogInstance) (*DataCatalogInstance, error)
	DeleteInstance(id string) (bool, error)

	// Relations
	GetRelations(sourceID *string, targetID *string, relationType *string) ([]DataCatalogRelation, error)
	SaveRelation(r DataCatalogRelation) (*DataCatalogRelation, error)
	DeleteRelation(sourceID string, targetID string, relationType string) (bool, error)

	// Export / Import
	ExportCatalog(namespace *string) (map[string]interface{}, error)
	ImportCatalog(data map[string]interface{}) (*ImportDataCatalogResult, error)
}
