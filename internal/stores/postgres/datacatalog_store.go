package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"monstermq.io/edge/internal/stores"
)

type DataCatalogStore struct {
	db *DB
}

func NewDataCatalogStore(db *DB) *DataCatalogStore {
	return &DataCatalogStore{db: db}
}

func (s *DataCatalogStore) Initialize() error {
	ctx := context.Background()
	queries := []string{
		`CREATE TABLE IF NOT EXISTS data_catalog_types (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			structure JSONB,
			topic_pattern TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS data_catalog_instances (
			id TEXT PRIMARY KEY,
			type_id TEXT NOT NULL,
			name TEXT NOT NULL,
			base_topic TEXT NOT NULL,
			properties JSONB,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			FOREIGN KEY (type_id) REFERENCES data_catalog_types (id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS data_catalog_relations (
			source_id TEXT NOT NULL,
			target_id TEXT NOT NULL,
			relation_type TEXT NOT NULL,
			PRIMARY KEY (source_id, target_id, relation_type),
			FOREIGN KEY (source_id) REFERENCES data_catalog_instances (id) ON DELETE CASCADE,
			FOREIGN KEY (target_id) REFERENCES data_catalog_instances (id) ON DELETE CASCADE
		)`,
	}
	for _, q := range queries {
		if _, err := s.db.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to create data catalog tables: %w", err)
		}
	}
	return nil
}

func (s *DataCatalogStore) Close() error {
	return nil
}

// Types
func (s *DataCatalogStore) GetTypes(namespace *string) ([]stores.DataCatalogType, error) {
	ctx := context.Background()
	query := `SELECT id, namespace, name, description, structure, topic_pattern, created_at, updated_at FROM data_catalog_types`
	var rows pgx.Rows
	var err error

	if namespace != nil && *namespace != "" {
		query += ` WHERE namespace = $1`
		rows, err = s.db.pool.Query(ctx, query, *namespace)
	} else {
		rows, err = s.db.pool.Query(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []stores.DataCatalogType
	for rows.Next() {
		var t stores.DataCatalogType
		var structStr []byte
		if err := rows.Scan(&t.ID, &t.Namespace, &t.Name, &t.Description, &structStr, &t.TopicPattern, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if len(structStr) > 0 {
			json.Unmarshal(structStr, &t.Structure)
		}
		if t.Structure == nil {
			t.Structure = make(map[string]interface{})
		}
		result = append(result, t)
	}
	if result == nil {
	    result = []stores.DataCatalogType{}
	}
	return result, rows.Err()
}

func (s *DataCatalogStore) GetType(id string) (*stores.DataCatalogType, error) {
	ctx := context.Background()
	query := `SELECT id, namespace, name, description, structure, topic_pattern, created_at, updated_at FROM data_catalog_types WHERE id = $1`
	var t stores.DataCatalogType
	var structStr []byte
	err := s.db.pool.QueryRow(ctx, query, id).Scan(&t.ID, &t.Namespace, &t.Name, &t.Description, &structStr, &t.TopicPattern, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if len(structStr) > 0 {
		json.Unmarshal(structStr, &t.Structure)
	}
	if t.Structure == nil {
		t.Structure = make(map[string]interface{})
	}
	return &t, nil
}

func (s *DataCatalogStore) SaveType(t stores.DataCatalogType) (*stores.DataCatalogType, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	if t.CreatedAt == nil {
		t.CreatedAt = &now
	}
	t.UpdatedAt = &now

	structJson := t.GetStructureJSON()

	query := `INSERT INTO data_catalog_types (id, namespace, name, description, structure, topic_pattern, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			namespace = EXCLUDED.namespace,
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			structure = EXCLUDED.structure,
			topic_pattern = EXCLUDED.topic_pattern,
			updated_at = EXCLUDED.updated_at
		RETURNING created_at`

	err := s.db.pool.QueryRow(ctx, query,
		t.ID, t.Namespace, t.Name, t.Description, structJson, t.TopicPattern, t.CreatedAt, t.UpdatedAt).Scan(&t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if t.Structure == nil {
		t.Structure = make(map[string]interface{})
	}
	return &t, nil
}

func (s *DataCatalogStore) DeleteType(id string) (bool, error) {
	ctx := context.Background()
	res, err := s.db.pool.Exec(ctx, `DELETE FROM data_catalog_types WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return res.RowsAffected() > 0, nil
}

// Instances
func (s *DataCatalogStore) GetInstances(typeID *string) ([]stores.DataCatalogInstance, error) {
	ctx := context.Background()
	query := `SELECT id, type_id, name, base_topic, properties, created_at, updated_at FROM data_catalog_instances`
	var rows pgx.Rows
	var err error

	if typeID != nil && *typeID != "" {
		query += ` WHERE type_id = $1`
		rows, err = s.db.pool.Query(ctx, query, *typeID)
	} else {
		rows, err = s.db.pool.Query(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []stores.DataCatalogInstance
	for rows.Next() {
		var i stores.DataCatalogInstance
		var propStr []byte
		if err := rows.Scan(&i.ID, &i.TypeID, &i.Name, &i.BaseTopic, &propStr, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, err
		}
		if len(propStr) > 0 {
			json.Unmarshal(propStr, &i.Properties)
		}
		if i.Properties == nil {
			i.Properties = make(map[string]interface{})
		}
		result = append(result, i)
	}
	if result == nil {
	    result = []stores.DataCatalogInstance{}
	}
	return result, rows.Err()
}

func (s *DataCatalogStore) GetInstance(id string) (*stores.DataCatalogInstance, error) {
	ctx := context.Background()
	query := `SELECT id, type_id, name, base_topic, properties, created_at, updated_at FROM data_catalog_instances WHERE id = $1`
	var i stores.DataCatalogInstance
	var propStr []byte
	err := s.db.pool.QueryRow(ctx, query, id).Scan(&i.ID, &i.TypeID, &i.Name, &i.BaseTopic, &propStr, &i.CreatedAt, &i.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if len(propStr) > 0 {
		json.Unmarshal(propStr, &i.Properties)
	}
	if i.Properties == nil {
		i.Properties = make(map[string]interface{})
	}
	return &i, nil
}

func (s *DataCatalogStore) SaveInstance(i stores.DataCatalogInstance) (*stores.DataCatalogInstance, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	if i.CreatedAt == nil {
		i.CreatedAt = &now
	}
	i.UpdatedAt = &now

	propJson := i.GetPropertiesJSON()

	query := `INSERT INTO data_catalog_instances (id, type_id, name, base_topic, properties, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			type_id = EXCLUDED.type_id,
			name = EXCLUDED.name,
			base_topic = EXCLUDED.base_topic,
			properties = EXCLUDED.properties,
			updated_at = EXCLUDED.updated_at
		RETURNING created_at`

	err := s.db.pool.QueryRow(ctx, query,
		i.ID, i.TypeID, i.Name, i.BaseTopic, propJson, i.CreatedAt, i.UpdatedAt).Scan(&i.CreatedAt)
	if err != nil {
		return nil, err
	}
	if i.Properties == nil {
		i.Properties = make(map[string]interface{})
	}
	return &i, nil
}

func (s *DataCatalogStore) DeleteInstance(id string) (bool, error) {
	ctx := context.Background()
	res, err := s.db.pool.Exec(ctx, `DELETE FROM data_catalog_instances WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return res.RowsAffected() > 0, nil
}

// Relations
func (s *DataCatalogStore) GetRelations(sourceID *string, targetID *string, relationType *string) ([]stores.DataCatalogRelation, error) {
	ctx := context.Background()
	query := `SELECT source_id, target_id, relation_type FROM data_catalog_relations WHERE 1=1`
	var args []interface{}
	idx := 1
	if sourceID != nil && *sourceID != "" {
		query += fmt.Sprintf(` AND source_id = $%d`, idx)
		args = append(args, *sourceID)
		idx++
	}
	if targetID != nil && *targetID != "" {
		query += fmt.Sprintf(` AND target_id = $%d`, idx)
		args = append(args, *targetID)
		idx++
	}
	if relationType != nil && *relationType != "" {
		query += fmt.Sprintf(` AND relation_type = $%d`, idx)
		args = append(args, *relationType)
		idx++
	}

	rows, err := s.db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []stores.DataCatalogRelation
	for rows.Next() {
		var r stores.DataCatalogRelation
		if err := rows.Scan(&r.SourceID, &r.TargetID, &r.RelationType); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if result == nil {
		result = []stores.DataCatalogRelation{}
	}
	return result, rows.Err()
}

func (s *DataCatalogStore) SaveRelation(r stores.DataCatalogRelation) (*stores.DataCatalogRelation, error) {
	ctx := context.Background()
	query := `INSERT INTO data_catalog_relations (source_id, target_id, relation_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (source_id, target_id, relation_type) DO NOTHING`

	_, err := s.db.pool.Exec(ctx, query, r.SourceID, r.TargetID, r.RelationType)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *DataCatalogStore) DeleteRelation(sourceID string, targetID string, relationType string) (bool, error) {
	ctx := context.Background()
	res, err := s.db.pool.Exec(ctx, `DELETE FROM data_catalog_relations WHERE source_id = $1 AND target_id = $2 AND relation_type = $3`, sourceID, targetID, relationType)
	if err != nil {
		return false, err
	}
	return res.RowsAffected() > 0, nil
}

// Export / Import
func (s *DataCatalogStore) ExportCatalog(namespace *string) (map[string]interface{}, error) {
	types, err := s.GetTypes(namespace)
	if err != nil {
		return nil, err
	}
	if types == nil {
		types = []stores.DataCatalogType{}
	}

	var instances []stores.DataCatalogInstance
	var relations []stores.DataCatalogRelation

	if namespace != nil && *namespace != "" {
		for _, t := range types {
			insts, err := s.GetInstances(&t.ID)
			if err != nil {
				return nil, err
			}
			instances = append(instances, insts...)
		}

		instMap := make(map[string]bool)
		for _, inst := range instances {
			instMap[inst.ID] = true
		}

		allRels, err := s.GetRelations(nil, nil, nil)
		if err != nil {
			return nil, err
		}
		for _, r := range allRels {
			if instMap[r.SourceID] || instMap[r.TargetID] {
				relations = append(relations, r)
			}
		}
	} else {
		instances, err = s.GetInstances(nil)
		if err != nil {
			return nil, err
		}
		relations, err = s.GetRelations(nil, nil, nil)
		if err != nil {
			return nil, err
		}
	}

	if instances == nil {
		instances = []stores.DataCatalogInstance{}
	}
	if relations == nil {
		relations = []stores.DataCatalogRelation{}
	}

	return map[string]interface{}{
		"types":     types,
		"instances": instances,
		"relations": relations,
	}, nil
}

func (s *DataCatalogStore) ImportCatalog(data map[string]interface{}) (*stores.ImportDataCatalogResult, error) {
	res := &stores.ImportDataCatalogResult{
		Success: true,
		Errors:  []string{},
	}

	if typesIface, ok := data["types"].([]interface{}); ok {
		for _, tIface := range typesIface {
			b, err := json.Marshal(tIface)
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to marshal type: %v", err))
				continue
			}
			var t stores.DataCatalogType
			if err := json.Unmarshal(b, &t); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to unmarshal type: %v", err))
				continue
			}
			if _, err := s.SaveType(t); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to save type %s: %v", t.ID, err))
			} else {
				res.TypesImported++
			}
		}
	}

	if instsIface, ok := data["instances"].([]interface{}); ok {
		for _, iIface := range instsIface {
			b, err := json.Marshal(iIface)
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to marshal instance: %v", err))
				continue
			}
			var inst stores.DataCatalogInstance
			if err := json.Unmarshal(b, &inst); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to unmarshal instance: %v", err))
				continue
			}
			if _, err := s.SaveInstance(inst); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to save instance %s: %v", inst.ID, err))
			} else {
				res.InstancesImported++
			}
		}
	}

	if relsIface, ok := data["relations"].([]interface{}); ok {
		for _, rIface := range relsIface {
			b, err := json.Marshal(rIface)
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to marshal relation: %v", err))
				continue
			}
			var rel stores.DataCatalogRelation
			if err := json.Unmarshal(b, &rel); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to unmarshal relation: %v", err))
				continue
			}
			if _, err := s.SaveRelation(rel); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("failed to save relation %s->%s: %v", rel.SourceID, rel.TargetID, err))
			} else {
				res.RelationsImported++
			}
		}
	}

	res.Success = res.Failed == 0
	return res, nil
}
