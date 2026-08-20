package resolvers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/redfish"
	"monstermq.io/edge/internal/stores"
)

const DeviceTypeRedfish = "Redfish"

// Query: redfishMappings ----------------------------------------------------

func (r *queryResolver) RedfishMappings(ctx context.Context) ([]*generated.RedfishMapping, error) {
	if r.Redfish == nil && !r.Cfg.Features.Redfish && !r.Cfg.Redfish.Enabled {
		return []*generated.RedfishMapping{}, nil
	}
	devices, err := r.Storage.DeviceConfig.GetByType(ctx, DeviceTypeRedfish)
	if err != nil {
		return nil, err
	}
	var out []*generated.RedfishMapping
	for _, d := range devices {
		out = append(out, r.deviceToRedfishMapping(d))
	}
	return out, nil
}

// Query: redfishMapping(name) -----------------------------------------------

func (r *queryResolver) RedfishMapping(ctx context.Context, name string) (*generated.RedfishMapping, error) {
	d, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || d == nil || d.Type != DeviceTypeRedfish {
		return nil, nil
	}
	return r.deviceToRedfishMapping(*d), nil
}

// Query: redfishLiveSensors(chassisId) --------------------------------------

func (r *queryResolver) RedfishLiveSensors(ctx context.Context, chassisId *string) ([]*generated.RedfishSensorStatus, error) {
	if r.Redfish == nil {
		return []*generated.RedfishSensorStatus{}, nil
	}
	records := r.Redfish.GetLiveSensors(ctx, chassisId)
	var out []*generated.RedfishSensorStatus
	for _, rec := range records {
		topicPrefix := rec.TopicPrefix
		if topicPrefix == "" {
			topicPrefix = "redfish"
		}
		topic := fmt.Sprintf("%s/%s/sensors/%s", topicPrefix, rec.ChassisID, rec.SensorID)
		out = append(out, &generated.RedfishSensorStatus{
			ID:           rec.SensorID,
			Name:         rec.Name,
			ChassisID:    rec.ChassisID,
			Topic:        topic,
			Reading:      rec.Reading,
			ReadingType:  rec.ReadingType,
			ReadingUnits: rec.ReadingUnits,
			Health:       rec.Health,
			State:        rec.State,
			LastUpdated:  rec.Timestamp,
		})
	}
	return out, nil
}

// Mutation: saveRedfishMapping ----------------------------------------------

func (r *mutationResolver) SaveRedfishMapping(
	ctx context.Context,
	name string,
	input generated.RedfishMappingConfigInput,
	enabled *bool,
) (*generated.RedfishResult, error) {
	topicPrefix := "redfish"
	if input.TopicPrefix != nil && *input.TopicPrefix != "" {
		topicPrefix = *input.TopicPrefix
	}
	chassisID := "EdgeNode"
	if input.ChassisID != nil && *input.ChassisID != "" {
		chassisID = *input.ChassisID
	}
	defaultReadingType := "Temperature"
	if input.DefaultReadingType != nil && *input.DefaultReadingType != "" {
		defaultReadingType = *input.DefaultReadingType
	}
	defaultReadingUnits := "Cel"
	if input.DefaultReadingUnits != nil && *input.DefaultReadingUnits != "" {
		defaultReadingUnits = *input.DefaultReadingUnits
	}

	var th *redfish.ThresholdsConfig
	if input.Thresholds != nil {
		th = &redfish.ThresholdsConfig{
			UpperCaution:  input.Thresholds.UpperCaution,
			UpperCritical: input.Thresholds.UpperCritical,
			LowerCaution:  input.Thresholds.LowerCaution,
			LowerCritical: input.Thresholds.LowerCritical,
		}
	}

	gwCfg := redfish.GatewayConfig{
		TopicPrefix:         topicPrefix,
		TopicFilters:        input.TopicFilters,
		ChassisID:           chassisID,
		DefaultReadingType:  defaultReadingType,
		DefaultReadingUnits: defaultReadingUnits,
		Thresholds:          th,
		JSONSchema:          input.JSONSchema,
	}

	cfgBytes, err := json.Marshal(gwCfg)
	if err != nil {
		msg := fmt.Sprintf("Failed to marshal config: %v", err)
		return &generated.RedfishResult{
			Success: false,
			Message: &msg,
		}, nil
	}

	isEnabled := true
	if enabled != nil {
		isEnabled = *enabled
	}

	now := time.Now().UTC()
	dev := stores.DeviceConfig{
		Name:      name,
		Namespace: "default",
		NodeID:    r.NodeID,
		Type:      DeviceTypeRedfish,
		Enabled:   isEnabled,
		Config:    string(cfgBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := r.Storage.DeviceConfig.Save(ctx, dev); err != nil {
		msg := err.Error()
		return &generated.RedfishResult{
			Success: false,
			Message: &msg,
		}, nil
	}

	if r.Redfish != nil {
		_ = r.Redfish.Reload(ctx)
	}

	saved, _ := r.Storage.DeviceConfig.Get(ctx, name)
	if saved == nil {
		saved = &dev
	}

	return &generated.RedfishResult{
		Success: true,
		Redfish: r.deviceToRedfishMapping(*saved),
	}, nil
}

// Mutation: deleteRedfishMapping --------------------------------------------

func (r *mutationResolver) DeleteRedfishMapping(ctx context.Context, name string) (bool, error) {
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, err
	}
	if r.Redfish != nil {
		_ = r.Redfish.Reload(ctx)
	}
	return true, nil
}

// Mutation: toggleRedfishMapping --------------------------------------------

func (r *mutationResolver) ToggleRedfishMapping(ctx context.Context, name string, enabled bool) (*generated.RedfishResult, error) {
	updated, err := r.Storage.DeviceConfig.Toggle(ctx, name, enabled)
	if err != nil {
		msg := err.Error()
		return &generated.RedfishResult{
			Success: false,
			Message: &msg,
		}, nil
	}
	if r.Redfish != nil {
		_ = r.Redfish.Reload(ctx)
	}
	return &generated.RedfishResult{
		Success: true,
		Redfish: r.deviceToRedfishMapping(*updated),
	}, nil
}

// Helper: deviceToRedfishMapping --------------------------------------------

func (r *Resolver) deviceToRedfishMapping(d stores.DeviceConfig) *generated.RedfishMapping {
	var cfg redfish.GatewayConfig
	_ = json.Unmarshal([]byte(d.Config), &cfg)

	topicPrefix := cfg.TopicPrefix
	if topicPrefix == "" {
		topicPrefix = "redfish"
	}
	chassisID := cfg.ChassisID
	if chassisID == "" {
		chassisID = "EdgeNode"
	}

	var th *generated.RedfishThresholds
	if cfg.Thresholds != nil {
		th = &generated.RedfishThresholds{
			UpperCaution:  cfg.Thresholds.UpperCaution,
			UpperCritical: cfg.Thresholds.UpperCritical,
			LowerCaution:  cfg.Thresholds.LowerCaution,
			LowerCritical: cfg.Thresholds.LowerCritical,
		}
	}

	mappingConfig := &generated.RedfishMappingConfig{
		TopicPrefix:         topicPrefix,
		TopicFilters:        cfg.TopicFilters,
		ChassisID:           &chassisID,
		DefaultReadingType:  &cfg.DefaultReadingType,
		DefaultReadingUnits: &cfg.DefaultReadingUnits,
		Thresholds:          th,
		JSONSchema:          cfg.JSONSchema,
	}

	return &generated.RedfishMapping{
		Name:            d.Name,
		NodeID:          d.NodeID,
		Enabled:         d.Enabled,
		Config:          mappingConfig,
		CreatedAt:       d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       d.UpdatedAt.UTC().Format(time.RFC3339),
		IsOnCurrentNode: d.NodeID == r.NodeID || d.NodeID == "",
	}
}
