package resolvers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"monstermq.io/edge/internal/bridge/rtspcamera"
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
)

// Query: rtspCameras(name, node) --------------------------------------------

func (r *queryResolver) RtspCameras(ctx context.Context, name, node *string) ([]*generated.RtspCamera, error) {
	if !r.Cfg.Features.RtspCamera {
		return []*generated.RtspCamera{}, nil
	}
	devices, err := r.Storage.DeviceConfig.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	out := []*generated.RtspCamera{}
	for _, d := range devices {
		if d.Type != rtspcamera.DeviceTypeRtspCamera {
			continue
		}
		if name != nil && d.Name != *name {
			continue
		}
		if node != nil && d.NodeID != *node {
			continue
		}
		out = append(out, r.deviceToRtspCamera(d))
	}
	return out, nil
}

// Query: rtspCamera(name) ---------------------------------------------------

func (r *queryResolver) RtspCamera(ctx context.Context, name string) (*generated.RtspCamera, error) {
	if !r.Cfg.Features.RtspCamera {
		return nil, nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || d == nil || d.Type != rtspcamera.DeviceTypeRtspCamera {
		return nil, nil
	}
	return r.deviceToRtspCamera(*d), nil
}

// Field resolvers on RtspCamera ---------------------------------------------

type rtspCameraResolver struct{ *Resolver }

func (r *rtspCameraResolver) Metrics(ctx context.Context, obj *generated.RtspCamera) ([]*generated.RtspCameraMetrics, error) {
	if r.RtspCameras == nil || obj == nil {
		return []*generated.RtspCameraMetrics{{Timestamp: nowISO()}}, nil
	}
	c := r.RtspCameras.Connector(obj.Name)
	if c == nil {
		return []*generated.RtspCameraMetrics{{Timestamp: nowISO()}}, nil
	}
	m := c.Metrics()
	var lastSnap *string
	if m.LastSnapshotAt != "" {
		lastSnap = &m.LastSnapshotAt
	}
	var lastErr *string
	if m.LastError != "" {
		lastErr = &m.LastError
	}
	return []*generated.RtspCameraMetrics{{
		Connected:          m.Connected,
		FramesReceived:     m.FramesReceived,
		SnapshotsPublished: m.SnapshotsPublished,
		CurrentSlot:        m.CurrentSlot,
		LastSnapshotAt:     lastSnap,
		LastError:          lastErr,
		Timestamp:          m.Timestamp,
	}}, nil
}

func (r *rtspCameraResolver) MetricsHistory(ctx context.Context, obj *generated.RtspCamera, from, to *string, lastMinutes *int) ([]*generated.RtspCameraMetrics, error) {
	return []*generated.RtspCameraMetrics{}, nil
}

// Mutation: rtspCamera ------------------------------------------------------

type rtspCameraDeviceMutationsResolver struct{ *Resolver }

func (r *mutationResolver) RtspCamera(ctx context.Context) (*generated.RtspCameraDeviceMutations, error) {
	return &generated.RtspCameraDeviceMutations{}, nil
}

func (r *rtspCameraDeviceMutationsResolver) Create(ctx context.Context, _ *generated.RtspCameraDeviceMutations, input generated.RtspCameraInput) (*generated.RtspCameraResult, error) {
	if !r.Cfg.Features.RtspCamera {
		return &generated.RtspCameraResult{Success: false, Errors: []string{"RtspCamera feature is disabled"}}, nil
	}
	if strings.TrimSpace(input.Name) == "" {
		return &generated.RtspCameraResult{Success: false, Errors: []string{"name cannot be empty"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, input.Name)
	if err == nil && existing != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{fmt.Sprintf("Camera %q already exists", input.Name)}}, nil
	}

	cfg := rtspCameraInputToConfig(input.Config)
	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.RtspCameraResult{Success: false, Errors: errs}, nil
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	nodeID := input.NodeID
	if nodeID == "" {
		nodeID = r.NodeID
	}

	d := stores.DeviceConfig{
		Name:      input.Name,
		Namespace: "default",
		NodeID:    nodeID,
		Type:      rtspcamera.DeviceTypeRtspCamera,
		Enabled:   boolPtr(input.Enabled, true),
		Config:    string(cfgJSON),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := r.Storage.DeviceConfig.Save(ctx, d); err != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	r.reloadRtspCamera(ctx)
	saved, _ := r.Storage.DeviceConfig.Get(ctx, d.Name)
	if saved == nil {
		saved = &d
	}

	return &generated.RtspCameraResult{
		Success: true,
		Camera:  r.deviceToRtspCamera(*saved),
		Errors:  []string{},
	}, nil
}

func (r *rtspCameraDeviceMutationsResolver) Update(ctx context.Context, _ *generated.RtspCameraDeviceMutations, name string, input generated.RtspCameraInput) (*generated.RtspCameraResult, error) {
	if !r.Cfg.Features.RtspCamera {
		return &generated.RtspCameraResult{Success: false, Errors: []string{"RtspCamera feature is disabled"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{fmt.Sprintf("Camera %q not found", name)}}, nil
	}

	cfg := rtspCameraInputToConfig(input.Config)
	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.RtspCameraResult{Success: false, Errors: errs}, nil
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	nodeID := existing.NodeID
	if input.NodeID != "" {
		nodeID = input.NodeID
	}

	updated := stores.DeviceConfig{
		Name:      existing.Name,
		Namespace: existing.Namespace,
		NodeID:    nodeID,
		Type:      rtspcamera.DeviceTypeRtspCamera,
		Enabled:   boolPtr(input.Enabled, existing.Enabled),
		Config:    string(cfgJSON),
		CreatedAt: existing.CreatedAt,
		UpdatedAt: time.Now().UTC(),
	}

	if err := r.Storage.DeviceConfig.Save(ctx, updated); err != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{err.Error()}}, nil
	}

	r.reloadRtspCamera(ctx)
	saved, _ := r.Storage.DeviceConfig.Get(ctx, updated.Name)
	if saved == nil {
		saved = &updated
	}

	return &generated.RtspCameraResult{
		Success: true,
		Camera:  r.deviceToRtspCamera(*saved),
		Errors:  []string{},
	}, nil
}

func (r *rtspCameraDeviceMutationsResolver) Delete(ctx context.Context, _ *generated.RtspCameraDeviceMutations, name string) (bool, error) {
	if !r.Cfg.Features.RtspCamera {
		return false, nil
	}
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, err
	}
	r.reloadRtspCamera(ctx)
	return true, nil
}

func (r *rtspCameraDeviceMutationsResolver) Start(ctx context.Context, _ *generated.RtspCameraDeviceMutations, name string) (*generated.RtspCameraResult, error) {
	return r.Toggle(ctx, nil, name, true)
}

func (r *rtspCameraDeviceMutationsResolver) Stop(ctx context.Context, _ *generated.RtspCameraDeviceMutations, name string) (*generated.RtspCameraResult, error) {
	return r.Toggle(ctx, nil, name, false)
}

func (r *rtspCameraDeviceMutationsResolver) Toggle(ctx context.Context, _ *generated.RtspCameraDeviceMutations, name string, enabled bool) (*generated.RtspCameraResult, error) {
	if !r.Cfg.Features.RtspCamera {
		return &generated.RtspCameraResult{Success: false, Errors: []string{"RtspCamera feature is disabled"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{fmt.Sprintf("Camera %q not found", name)}}, nil
	}
	existing.Enabled = enabled
	existing.UpdatedAt = time.Now().UTC()
	if err := r.Storage.DeviceConfig.Save(ctx, *existing); err != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	r.reloadRtspCamera(ctx)
	return &generated.RtspCameraResult{
		Success: true,
		Camera:  r.deviceToRtspCamera(*existing),
		Errors:  []string{},
	}, nil
}

func (r *rtspCameraDeviceMutationsResolver) TriggerSnapshot(ctx context.Context, _ *generated.RtspCameraDeviceMutations, name string) (*generated.RtspCameraResult, error) {
	if !r.Cfg.Features.RtspCamera {
		return &generated.RtspCameraResult{Success: false, Errors: []string{"RtspCamera feature is disabled"}}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{fmt.Sprintf("Camera %q not found", name)}}, nil
	}
	if r.RtspCameras == nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{"RtspCameras manager not initialized"}}, nil
	}
	if err := r.RtspCameras.TriggerSnapshot(name); err != nil {
		return &generated.RtspCameraResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	return &generated.RtspCameraResult{
		Success: true,
		Camera:  r.deviceToRtspCamera(*existing),
		Errors:  []string{},
	}, nil
}

// Helpers -------------------------------------------------------------------

func (r *Resolver) deviceToRtspCamera(d stores.DeviceConfig) *generated.RtspCamera {
	cfg, _ := rtspcamera.ParseConfig(d.Config)
	transport := generated.RtspTransportTCP
	if strings.ToUpper(cfg.Transport) == rtspcamera.TransportUDP {
		transport = generated.RtspTransportUDP
	}
	mode := generated.RtspCaptureModeContinuous
	switch strings.ToUpper(cfg.Mode) {
	case rtspcamera.ModeTriggered:
		mode = generated.RtspCaptureModeTriggered
	case rtspcamera.ModeBoth:
		mode = generated.RtspCaptureModeBoth
	}

	var triggerTopicPtr *string
	if cfg.TriggerTopic != "" {
		triggerTopicPtr = &cfg.TriggerTopic
	}

	return &generated.RtspCamera{
		Name:            d.Name,
		NodeID:          d.NodeID,
		Enabled:         d.Enabled,
		CreatedAt:       d.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       d.UpdatedAt.Format(time.RFC3339),
		IsOnCurrentNode: d.NodeID == r.NodeID,
		Config: &generated.RtspCameraConfig{
			URL:             cfg.URL,
			Transport:       transport,
			TopicPrefix:     cfg.TopicPrefix,
			Mode:            mode,
			IntervalMs:      cfg.IntervalMs,
			Slots:           cfg.Slots,
			TriggerTopic:    triggerTopicPtr,
			Retain:          cfg.Retain,
			Qos:             cfg.QoS,
			PublishMetadata: cfg.PublishMetadata,
		},
	}
}

func rtspCameraInputToConfig(input *generated.RtspCameraConfigInput) rtspcamera.Config {
	cfg := rtspcamera.DefaultConfig()
	if input == nil {
		return cfg
	}
	cfg.URL = input.URL
	if input.Transport != nil && *input.Transport == generated.RtspTransportUDP {
		cfg.Transport = rtspcamera.TransportUDP
	} else {
		cfg.Transport = rtspcamera.TransportTCP
	}
	if input.Mode != nil {
		switch *input.Mode {
		case generated.RtspCaptureModeTriggered:
			cfg.Mode = rtspcamera.ModeTriggered
		case generated.RtspCaptureModeBoth:
			cfg.Mode = rtspcamera.ModeBoth
		default:
			cfg.Mode = rtspcamera.ModeContinuous
		}
	}
	if input.IntervalMs != nil {
		cfg.IntervalMs = *input.IntervalMs
	}
	if input.Slots != nil {
		cfg.Slots = *input.Slots
	}
	if input.TopicPrefix != "" {
		cfg.TopicPrefix = input.TopicPrefix
	}
	if input.TriggerTopic != nil {
		cfg.TriggerTopic = *input.TriggerTopic
	}
	if input.Retain != nil {
		cfg.Retain = *input.Retain
	}
	if input.Qos != nil {
		cfg.QoS = *input.Qos
	}
	if input.PublishMetadata != nil {
		cfg.PublishMetadata = *input.PublishMetadata
	}
	cfg.ApplyDefaults()
	return cfg
}

func (r *Resolver) reloadRtspCamera(ctx context.Context) {
	if r.RtspCameras != nil {
		if err := r.RtspCameras.Reload(ctx); err != nil {
			r.Logger.Warn("rtsp camera reload failed", "err", err)
		}
	}
}
