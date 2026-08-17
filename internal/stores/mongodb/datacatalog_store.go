package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"monstermq.io/edge/internal/stores"
)

type DataCatalogStore struct {
	client *mongo.Client
	db     *mongo.Database
}

func NewDataCatalogStore(client *mongo.Client) *DataCatalogStore {
	return &DataCatalogStore{
		client: client,
		db:     client.Database("datacatalog"),
	}
}

func (s *DataCatalogStore) typesColl() *mongo.Collection {
	return s.db.Collection("types")
}

func (s *DataCatalogStore) instancesColl() *mongo.Collection {
	return s.db.Collection("instances")
}

func (s *DataCatalogStore) relationsColl() *mongo.Collection {
	return s.db.Collection("relations")
}

func (s *DataCatalogStore) Initialize() error {
	ctx := context.Background()

	_, err := s.typesColl().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return err
	}

	_, err = s.instancesColl().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return err
	}

	_, err = s.relationsColl().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "sourceId", Value: 1},
			{Key: "targetId", Value: 1},
			{Key: "relationType", Value: 1},
		},
		Options: options.Index().SetUnique(true),
	})
	return err
}

func (s *DataCatalogStore) Close() error {
	return nil
}

type mongoType struct {
	ID           string                 `bson:"id"`
	Namespace    string                 `bson:"namespace"`
	Name         string                 `bson:"name"`
	Description  *string                `bson:"description,omitempty"`
	Structure    map[string]interface{} `bson:"structure"`
	TopicPattern *string                `bson:"topicPattern,omitempty"`
	CreatedAt    *time.Time             `bson:"createdAt,omitempty"`
	UpdatedAt    *time.Time             `bson:"updatedAt,omitempty"`
}

func toMongoType(t stores.DataCatalogType) mongoType {
	return mongoType{
		ID:           t.ID,
		Namespace:    t.Namespace,
		Name:         t.Name,
		Description:  t.Description,
		Structure:    t.Structure,
		TopicPattern: t.TopicPattern,
		CreatedAt:    t.CreatedAt,
		UpdatedAt:    t.UpdatedAt,
	}
}

func fromMongoType(m mongoType) stores.DataCatalogType {
	return stores.DataCatalogType{
		ID:           m.ID,
		Namespace:    m.Namespace,
		Name:         m.Name,
		Description:  m.Description,
		Structure:    m.Structure,
		TopicPattern: m.TopicPattern,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

type mongoInstance struct {
	ID         string                 `bson:"id"`
	TypeID     string                 `bson:"typeId"`
	Name       string                 `bson:"name"`
	BaseTopic  string                 `bson:"baseTopic"`
	Properties map[string]interface{} `bson:"properties"`
	CreatedAt  *time.Time             `bson:"createdAt,omitempty"`
	UpdatedAt  *time.Time             `bson:"updatedAt,omitempty"`
}

func toMongoInstance(i stores.DataCatalogInstance) mongoInstance {
	return mongoInstance{
		ID:         i.ID,
		TypeID:     i.TypeID,
		Name:       i.Name,
		BaseTopic:  i.BaseTopic,
		Properties: i.Properties,
		CreatedAt:  i.CreatedAt,
		UpdatedAt:  i.UpdatedAt,
	}
}

func fromMongoInstance(m mongoInstance) stores.DataCatalogInstance {
	return stores.DataCatalogInstance{
		ID:         m.ID,
		TypeID:     m.TypeID,
		Name:       m.Name,
		BaseTopic:  m.BaseTopic,
		Properties: m.Properties,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}

type mongoRelation struct {
	SourceID     string `bson:"sourceId"`
	TargetID     string `bson:"targetId"`
	RelationType string `bson:"relationType"`
}

func toMongoRelation(r stores.DataCatalogRelation) mongoRelation {
	return mongoRelation{
		SourceID:     r.SourceID,
		TargetID:     r.TargetID,
		RelationType: r.RelationType,
	}
}

func fromMongoRelation(m mongoRelation) stores.DataCatalogRelation {
	return stores.DataCatalogRelation{
		SourceID:     m.SourceID,
		TargetID:     m.TargetID,
		RelationType: m.RelationType,
	}
}

func (s *DataCatalogStore) GetTypes(namespace *string) ([]stores.DataCatalogType, error) {
	ctx := context.Background()
	filter := bson.M{}
	if namespace != nil {
		filter["namespace"] = *namespace
	}

	cur, err := s.typesColl().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	res := make([]stores.DataCatalogType, 0)
	for cur.Next(ctx) {
		var m mongoType
		if err := cur.Decode(&m); err != nil {
			return nil, err
		}
		res = append(res, fromMongoType(m))
	}
	return res, cur.Err()
}

func (s *DataCatalogStore) GetType(id string) (*stores.DataCatalogType, error) {
	ctx := context.Background()
	var m mongoType
	err := s.typesColl().FindOne(ctx, bson.M{"id": id}).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := fromMongoType(m)
	return &t, nil
}

func (s *DataCatalogStore) SaveType(t stores.DataCatalogType) (*stores.DataCatalogType, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	t.UpdatedAt = &now

	update := bson.M{
		"$set": bson.M{
			"namespace":    t.Namespace,
			"name":         t.Name,
			"description":  t.Description,
			"structure":    t.Structure,
			"topicPattern": t.TopicPattern,
			"updatedAt":    t.UpdatedAt,
		},
		"$setOnInsert": bson.M{
			"createdAt": now,
		},
	}

	_, err := s.typesColl().UpdateOne(ctx, bson.M{"id": t.ID}, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return nil, err
	}

	return s.GetType(t.ID)
}

func (s *DataCatalogStore) DeleteType(id string) (bool, error) {
	ctx := context.Background()
	res, err := s.typesColl().DeleteOne(ctx, bson.M{"id": id})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func (s *DataCatalogStore) GetInstances(typeID *string) ([]stores.DataCatalogInstance, error) {
	ctx := context.Background()
	filter := bson.M{}
	if typeID != nil {
		filter["typeId"] = *typeID
	}

	cur, err := s.instancesColl().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	res := make([]stores.DataCatalogInstance, 0)
	for cur.Next(ctx) {
		var m mongoInstance
		if err := cur.Decode(&m); err != nil {
			return nil, err
		}
		res = append(res, fromMongoInstance(m))
	}
	return res, cur.Err()
}

func (s *DataCatalogStore) GetInstance(id string) (*stores.DataCatalogInstance, error) {
	ctx := context.Background()
	var m mongoInstance
	err := s.instancesColl().FindOne(ctx, bson.M{"id": id}).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	i := fromMongoInstance(m)
	return &i, nil
}

func (s *DataCatalogStore) SaveInstance(i stores.DataCatalogInstance) (*stores.DataCatalogInstance, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	i.UpdatedAt = &now

	update := bson.M{
		"$set": bson.M{
			"typeId":     i.TypeID,
			"name":       i.Name,
			"baseTopic":  i.BaseTopic,
			"properties": i.Properties,
			"updatedAt":  i.UpdatedAt,
		},
		"$setOnInsert": bson.M{
			"createdAt": now,
		},
	}

	_, err := s.instancesColl().UpdateOne(ctx, bson.M{"id": i.ID}, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return nil, err
	}

	return s.GetInstance(i.ID)
}

func (s *DataCatalogStore) DeleteInstance(id string) (bool, error) {
	ctx := context.Background()
	res, err := s.instancesColl().DeleteOne(ctx, bson.M{"id": id})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func (s *DataCatalogStore) GetRelations(sourceID *string, targetID *string, relationType *string) ([]stores.DataCatalogRelation, error) {
	ctx := context.Background()
	filter := bson.M{}
	if sourceID != nil {
		filter["sourceId"] = *sourceID
	}
	if targetID != nil {
		filter["targetId"] = *targetID
	}
	if relationType != nil {
		filter["relationType"] = *relationType
	}

	cur, err := s.relationsColl().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	res := make([]stores.DataCatalogRelation, 0)
	for cur.Next(ctx) {
		var m mongoRelation
		if err := cur.Decode(&m); err != nil {
			return nil, err
		}
		res = append(res, fromMongoRelation(m))
	}
	return res, cur.Err()
}

func (s *DataCatalogStore) SaveRelation(r stores.DataCatalogRelation) (*stores.DataCatalogRelation, error) {
	ctx := context.Background()

	filter := bson.M{
		"sourceId":     r.SourceID,
		"targetId":     r.TargetID,
		"relationType": r.RelationType,
	}

	update := bson.M{
		"$set": bson.M{
			"sourceId":     r.SourceID,
			"targetId":     r.TargetID,
			"relationType": r.RelationType,
		},
	}

	_, err := s.relationsColl().UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return nil, err
	}

	return &r, nil
}

func (s *DataCatalogStore) DeleteRelation(sourceID string, targetID string, relationType string) (bool, error) {
	ctx := context.Background()
	filter := bson.M{
		"sourceId":     sourceID,
		"targetId":     targetID,
		"relationType": relationType,
	}
	res, err := s.relationsColl().DeleteOne(ctx, filter)
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func (s *DataCatalogStore) ExportCatalog(namespace *string) (map[string]interface{}, error) {
	types, err := s.GetTypes(namespace)
	if err != nil {
		return nil, err
	}

	instances := make([]stores.DataCatalogInstance, 0)
	relations := make([]stores.DataCatalogRelation, 0)

	if namespace == nil {
		instances, err = s.GetInstances(nil)
		if err != nil {
			return nil, err
		}
		relations, err = s.GetRelations(nil, nil, nil)
		if err != nil {
			return nil, err
		}
	} else {
		instMap := make(map[string]bool)
		for _, t := range types {
			insts, err := s.GetInstances(&t.ID)
			if err != nil {
				return nil, err
			}
			for _, inst := range insts {
				instances = append(instances, inst)
				instMap[inst.ID] = true
			}
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
		Errors:  make([]string, 0),
	}

	b, _ := json.Marshal(data)
	var parsed struct {
		Types     []stores.DataCatalogType     `json:"types"`
		Instances []stores.DataCatalogInstance `json:"instances"`
		Relations []stores.DataCatalogRelation `json:"relations"`
	}

	json.Unmarshal(b, &parsed)

	for _, t := range parsed.Types {
		if _, err := s.SaveType(t); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, "Type "+t.ID+": "+err.Error())
		} else {
			res.TypesImported++
		}
	}

	for _, i := range parsed.Instances {
		if _, err := s.SaveInstance(i); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, "Instance "+i.ID+": "+err.Error())
		} else {
			res.InstancesImported++
		}
	}

	for _, r := range parsed.Relations {
		if _, err := s.SaveRelation(r); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, "Relation "+r.SourceID+"->"+r.TargetID+": "+err.Error())
		} else {
			res.RelationsImported++
		}
	}

	if res.Failed > 0 {
		res.Success = false
	}

	return res, nil
}
