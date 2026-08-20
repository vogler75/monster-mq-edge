package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"monstermq.io/edge/internal/stores"
)

const (
	dcTypesTable     = "datacatalogtypes"
	dcInstancesTable = "datacataloginstances"
	dcRelationsTable = "datacatalogrelations"
)

type DataCatalogStore struct {
	db *DB
}

func NewDataCatalogStore(db *DB) *DataCatalogStore { return &DataCatalogStore{db: db} }

func (d *DataCatalogStore) Initialize() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + dcTypesTable + ` (
            id TEXT PRIMARY KEY,
            namespace TEXT NOT NULL,
            name TEXT NOT NULL,
            description TEXT,
            structure TEXT NOT NULL,
            topic_pattern TEXT,
            created_at TEXT DEFAULT (datetime('now')),
            updated_at TEXT DEFAULT (datetime('now'))
        )`,
		`CREATE TABLE IF NOT EXISTS ` + dcInstancesTable + ` (
            id TEXT PRIMARY KEY,
            type_id TEXT NOT NULL,
            name TEXT NOT NULL,
            base_topic TEXT NOT NULL,
            properties TEXT NOT NULL,
            created_at TEXT DEFAULT (datetime('now')),
            updated_at TEXT DEFAULT (datetime('now')),
            FOREIGN KEY(type_id) REFERENCES ` + dcTypesTable + `(id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS ` + dcRelationsTable + ` (
            source_id TEXT NOT NULL,
            target_id TEXT NOT NULL,
            relation_type TEXT NOT NULL,
            PRIMARY KEY (source_id, target_id, relation_type)
        )`,
		`CREATE INDEX IF NOT EXISTS idx_dct_ns ON ` + dcTypesTable + ` (namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_dci_type ON ` + dcInstancesTable + ` (type_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dcr_src ON ` + dcRelationsTable + ` (source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dcr_tgt ON ` + dcRelationsTable + ` (target_id)`,
	}
	for _, q := range stmts {
		if _, err := d.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func (d *DataCatalogStore) Close() error { return nil }

func scanType(scanner interface{ Scan(...any) error }) (*stores.DataCatalogType, error) {
	var (
		t            stores.DataCatalogType
		desc         sql.NullString
		structureStr string
		tp           sql.NullString
		ca, ua       sql.NullString
	)
	if err := scanner.Scan(&t.ID, &t.Namespace, &t.Name, &desc, &structureStr, &tp, &ca, &ua); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if desc.Valid {
		t.Description = &desc.String
	}
	if tp.Valid {
		t.TopicPattern = &tp.String
	}
	caT := parseTime(ca)
	uaT := parseTime(ua)
	t.CreatedAt = &caT
	t.UpdatedAt = &uaT
	
	t.Structure = make(map[string]interface{})
	if structureStr != "" {
		_ = json.Unmarshal([]byte(structureStr), &t.Structure)
	}
	
	return &t, nil
}

func scanInstance(scanner interface{ Scan(...any) error }) (*stores.DataCatalogInstance, error) {
	var (
		i        stores.DataCatalogInstance
		propsStr string
		ca, ua   sql.NullString
	)
	if err := scanner.Scan(&i.ID, &i.TypeID, &i.Name, &i.BaseTopic, &propsStr, &ca, &ua); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	caT := parseTime(ca)
	uaT := parseTime(ua)
	i.CreatedAt = &caT
	i.UpdatedAt = &uaT
	
	i.Properties = make(map[string]interface{})
	if propsStr != "" {
		_ = json.Unmarshal([]byte(propsStr), &i.Properties)
	}
	
	return &i, nil
}

func (d *DataCatalogStore) GetTypes(namespace *string) ([]stores.DataCatalogType, error) {
	ctx := context.Background()
	var rows *sql.Rows
	var err error
	if namespace != nil {
		rows, err = d.db.Conn().QueryContext(ctx, `SELECT id, namespace, name, description, structure, topic_pattern, created_at, updated_at FROM `+dcTypesTable+` WHERE namespace = ?`, *namespace)
	} else {
		rows, err = d.db.Conn().QueryContext(ctx, `SELECT id, namespace, name, description, structure, topic_pattern, created_at, updated_at FROM `+dcTypesTable)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var out []stores.DataCatalogType
	for rows.Next() {
		t, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, rows.Err()
}

func (d *DataCatalogStore) GetType(id string) (*stores.DataCatalogType, error) {
	ctx := context.Background()
	row := d.db.Conn().QueryRowContext(ctx, `SELECT id, namespace, name, description, structure, topic_pattern, created_at, updated_at FROM `+dcTypesTable+` WHERE id = ?`, id)
	return scanType(row)
}

func (d *DataCatalogStore) SaveType(t stores.DataCatalogType) (*stores.DataCatalogType, error) {
	ctx := context.Background()
	q := `INSERT INTO ` + dcTypesTable + ` (id, namespace, name, description, structure, topic_pattern, created_at, updated_at) 
	      VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	      ON CONFLICT(id) DO UPDATE SET 
	      namespace=excluded.namespace, name=excluded.name, description=excluded.description, 
	      structure=excluded.structure, topic_pattern=excluded.topic_pattern, updated_at=datetime('now')`
	      
	_, err := d.db.Conn().ExecContext(ctx, q, t.ID, t.Namespace, t.Name, t.Description, t.GetStructureJSON(), t.TopicPattern)
	if err != nil {
		return nil, err
	}
	return d.GetType(t.ID)
}

func (d *DataCatalogStore) DeleteType(id string) (bool, error) {
	ctx := context.Background()
	res, err := d.db.Conn().ExecContext(ctx, `DELETE FROM `+dcTypesTable+` WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	aff, _ := res.RowsAffected()
	return aff > 0, nil
}

func (d *DataCatalogStore) GetInstances(typeID *string) ([]stores.DataCatalogInstance, error) {
	ctx := context.Background()
	var rows *sql.Rows
	var err error
	if typeID != nil {
		rows, err = d.db.Conn().QueryContext(ctx, `SELECT id, type_id, name, base_topic, properties, created_at, updated_at FROM `+dcInstancesTable+` WHERE type_id = ?`, *typeID)
	} else {
		rows, err = d.db.Conn().QueryContext(ctx, `SELECT id, type_id, name, base_topic, properties, created_at, updated_at FROM `+dcInstancesTable)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var out []stores.DataCatalogInstance
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		if i != nil {
			out = append(out, *i)
		}
	}
	return out, rows.Err()
}

func (d *DataCatalogStore) GetInstance(id string) (*stores.DataCatalogInstance, error) {
	ctx := context.Background()
	row := d.db.Conn().QueryRowContext(ctx, `SELECT id, type_id, name, base_topic, properties, created_at, updated_at FROM `+dcInstancesTable+` WHERE id = ?`, id)
	return scanInstance(row)
}

func (d *DataCatalogStore) SaveInstance(i stores.DataCatalogInstance) (*stores.DataCatalogInstance, error) {
	ctx := context.Background()
	q := `INSERT INTO ` + dcInstancesTable + ` (id, type_id, name, base_topic, properties, created_at, updated_at) 
	      VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	      ON CONFLICT(id) DO UPDATE SET 
	      type_id=excluded.type_id, name=excluded.name, base_topic=excluded.base_topic, 
	      properties=excluded.properties, updated_at=datetime('now')`
	      
	_, err := d.db.Conn().ExecContext(ctx, q, i.ID, i.TypeID, i.Name, i.BaseTopic, i.GetPropertiesJSON())
	if err != nil {
		return nil, err
	}
	return d.GetInstance(i.ID)
}

func (d *DataCatalogStore) DeleteInstance(id string) (bool, error) {
	ctx := context.Background()
	res, err := d.db.Conn().ExecContext(ctx, `DELETE FROM `+dcInstancesTable+` WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	aff, _ := res.RowsAffected()
	return aff > 0, nil
}

func (d *DataCatalogStore) GetRelations(sourceID *string, targetID *string, relationType *string) ([]stores.DataCatalogRelation, error) {
	ctx := context.Background()
	q := `SELECT source_id, target_id, relation_type FROM `+dcRelationsTable+` WHERE 1=1`
	args := []any{}
	
	if sourceID != nil {
		q += " AND source_id = ?"
		args = append(args, *sourceID)
	}
	if targetID != nil {
		q += " AND target_id = ?"
		args = append(args, *targetID)
	}
	if relationType != nil {
		q += " AND relation_type = ?"
		args = append(args, *relationType)
	}
	
	rows, err := d.db.Conn().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var out []stores.DataCatalogRelation
	for rows.Next() {
		var r stores.DataCatalogRelation
		if err := rows.Scan(&r.SourceID, &r.TargetID, &r.RelationType); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DataCatalogStore) SaveRelation(r stores.DataCatalogRelation) (*stores.DataCatalogRelation, error) {
	ctx := context.Background()
	q := `INSERT INTO ` + dcRelationsTable + ` (source_id, target_id, relation_type) 
	      VALUES (?, ?, ?)
	      ON CONFLICT DO NOTHING`
	      
	_, err := d.db.Conn().ExecContext(ctx, q, r.SourceID, r.TargetID, r.RelationType)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DataCatalogStore) DeleteRelation(sourceID string, targetID string, relationType string) (bool, error) {
	ctx := context.Background()
	res, err := d.db.Conn().ExecContext(ctx, `DELETE FROM `+dcRelationsTable+` WHERE source_id = ? AND target_id = ? AND relation_type = ?`, sourceID, targetID, relationType)
	if err != nil {
		return false, err
	}
	aff, _ := res.RowsAffected()
	return aff > 0, nil
}

func (d *DataCatalogStore) ExportCatalog(namespace *string) (map[string]interface{}, error) {
	res := make(map[string]interface{})
	
	types, err := d.GetTypes(namespace)
	if err != nil {
		return nil, err
	}
	res["types"] = types
	
	instances := []stores.DataCatalogInstance{}
	for _, t := range types {
		ins, _ := d.GetInstances(&t.ID)
		instances = append(instances, ins...)
	}
	res["instances"] = instances
	
	relations := []stores.DataCatalogRelation{}
	seen := make(map[string]bool)
	
	for _, t := range types {
		rels, _ := d.GetRelations(&t.ID, nil, nil)
		for _, r := range rels {
			key := r.SourceID + "-" + r.TargetID + "-" + r.RelationType
			if !seen[key] {
				relations = append(relations, r)
				seen[key] = true
			}
		}
	}
	for _, i := range instances {
		rels, _ := d.GetRelations(&i.ID, nil, nil)
		for _, r := range rels {
			key := r.SourceID + "-" + r.TargetID + "-" + r.RelationType
			if !seen[key] {
				relations = append(relations, r)
				seen[key] = true
			}
		}
	}
	res["relations"] = relations
	
	return res, nil
}

func (d *DataCatalogStore) ImportCatalog(data map[string]interface{}) (*stores.ImportDataCatalogResult, error) {
	// Simple implementation
	res := &stores.ImportDataCatalogResult{Success: true}
	
	if typesIface, ok := data["types"].([]interface{}); ok {
		for _, tIface := range typesIface {
			if tMap, ok := tIface.(map[string]interface{}); ok {
				b, _ := json.Marshal(tMap)
				var t stores.DataCatalogType
				json.Unmarshal(b, &t)
				if _, err := d.SaveType(t); err == nil {
					res.TypesImported++
				} else {
					res.Failed++
					res.Errors = append(res.Errors, err.Error())
				}
			}
		}
	}
	
	if instancesIface, ok := data["instances"].([]interface{}); ok {
		for _, iIface := range instancesIface {
			if iMap, ok := iIface.(map[string]interface{}); ok {
				b, _ := json.Marshal(iMap)
				var inst stores.DataCatalogInstance
				json.Unmarshal(b, &inst)
				if _, err := d.SaveInstance(inst); err == nil {
					res.InstancesImported++
				} else {
					res.Failed++
					res.Errors = append(res.Errors, err.Error())
				}
			}
		}
	}
	
	if relationsIface, ok := data["relations"].([]interface{}); ok {
		for _, rIface := range relationsIface {
			if rMap, ok := rIface.(map[string]interface{}); ok {
				b, _ := json.Marshal(rMap)
				var rel stores.DataCatalogRelation
				json.Unmarshal(b, &rel)
				if _, err := d.SaveRelation(rel); err == nil {
					res.RelationsImported++
				} else {
					res.Failed++
					res.Errors = append(res.Errors, err.Error())
				}
			}
		}
	}
	
	res.Success = res.Failed == 0
	return res, nil
}
