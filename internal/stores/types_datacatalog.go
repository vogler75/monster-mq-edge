package stores

import (
	"encoding/json"
	"time"
)

type DataCatalogType struct {
	ID           string                 `json:"id"`
	Namespace    string                 `json:"namespace"`
	Name         string                 `json:"name"`
	Description  *string                `json:"description,omitempty"`
	Structure    map[string]interface{} `json:"structure"`
	TopicPattern *string                `json:"topicPattern,omitempty"`
	CreatedAt    *time.Time             `json:"createdAt,omitempty"`
	UpdatedAt    *time.Time             `json:"updatedAt,omitempty"`
}

type DataCatalogInstance struct {
	ID         string                 `json:"id"`
	TypeID     string                 `json:"typeId"`
	Name       string                 `json:"name"`
	BaseTopic  string                 `json:"baseTopic"`
	Properties map[string]interface{} `json:"properties"`
	CreatedAt  *time.Time             `json:"createdAt,omitempty"`
	UpdatedAt  *time.Time             `json:"updatedAt,omitempty"`
}

type DataCatalogRelation struct {
	SourceID     string `json:"sourceId"`
	TargetID     string `json:"targetId"`
	RelationType string `json:"relationType"`
}

type ImportDataCatalogResult struct {
	Success           bool     `json:"success"`
	TypesImported     int      `json:"typesImported"`
	InstancesImported int      `json:"instancesImported"`
	RelationsImported int      `json:"relationsImported"`
	Failed            int      `json:"failed"`
	Errors            []string `json:"errors"`
}

// Ensure proper raw JSON structure serialization
func (d *DataCatalogType) GetStructureJSON() string {
	if d.Structure == nil {
		return "{}"
	}
	b, err := json.Marshal(d.Structure)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (d *DataCatalogInstance) GetPropertiesJSON() string {
	if d.Properties == nil {
		return "{}"
	}
	b, err := json.Marshal(d.Properties)
	if err != nil {
		return "{}"
	}
	return string(b)
}
