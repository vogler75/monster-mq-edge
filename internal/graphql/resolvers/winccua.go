package resolvers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"monstermq.io/edge/internal/bridge/winccua"
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
)

// Query: winCCUaClients(name, node) -----------------------------------------

func (r *queryResolver) WinCCUaClients(ctx context.Context, name, node *string) ([]*generated.WinCCUaClient, error) {
	if !r.Cfg.Features.WinCCUa {
		return []*generated.WinCCUaClient{}, nil
	}
	devices, err := r.Storage.DeviceConfig.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	out := []*generated.WinCCUaClient{}
	for _, d := range devices {
		if d.Type != winccua.DeviceTypeWinCCUaClient {
			continue
		}
		if name != nil && d.Name != *name {
			continue
		}
		if node != nil && d.NodeID != *node {
			continue
		}
		out = append(out, r.deviceToWinCCUaClient(d))
	}
	return out, nil
}

// Field resolvers on WinCCUaClient -----------------------------------------

func (r *winCCUaClientResolver) Metrics(ctx context.Context, obj *generated.WinCCUaClient) ([]*generated.WinCCUaClientMetrics, error) {
	if r.WinCCUa == nil || obj == nil {
		return []*generated.WinCCUaClientMetrics{{Timestamp: nowISO()}}, nil
	}
	c := r.WinCCUa.Connector(obj.Name)
	if c == nil {
		return []*generated.WinCCUaClientMetrics{{Timestamp: nowISO()}}, nil
	}
	return []*generated.WinCCUaClientMetrics{{
		MessagesIn: c.MessagesIn(),
		Connected:  c.IsConnected(),
		Timestamp:  nowISO(),
	}}, nil
}

func (r *winCCUaClientResolver) MetricsHistory(ctx context.Context, obj *generated.WinCCUaClient, from, to *string, lastMinutes *int) ([]*generated.WinCCUaClientMetrics, error) {
	return []*generated.WinCCUaClientMetrics{}, nil
}

// Mutations group: winCCUaDevice -------------------------------------------

func (r *winCCUaDeviceMutationsResolver) Create(ctx context.Context, _ *generated.WinCCUaDeviceMutations, input generated.WinCCUaClientInput) (*generated.WinCCUaClientResult, error) {
	if !r.Cfg.Features.WinCCUa {
		return featureDisabledResult(), nil
	}
	cfg := winCCUaInputToConfig(input.Config, nil)
	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.WinCCUaClientResult{Success: false, Errors: errs}, nil
	}
	cfgJSON, _ := json.Marshal(cfg)

	existing, _ := r.Storage.DeviceConfig.Get(ctx, input.Name)
	if existing != nil {
		return &generated.WinCCUaClientResult{
			Success: false,
			Errors:  []string{fmt.Sprintf("Device with name %q already exists", input.Name)},
		}, nil
	}

	d := stores.DeviceConfig{
		Name:      input.Name,
		Namespace: input.Namespace,
		NodeID:    input.NodeID,
		Type:      winccua.DeviceTypeWinCCUaClient,
		Enabled:   boolPtr(input.Enabled, true),
		Config:    string(cfgJSON),
	}
	if err := r.Storage.DeviceConfig.Save(ctx, d); err != nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	saved, _ := r.Storage.DeviceConfig.Get(ctx, d.Name)
	if saved == nil {
		saved = &d
	}
	r.reloadWinCCUa(ctx, "create")
	return &generated.WinCCUaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCUaClient(*saved),
	}, nil
}

func (r *winCCUaDeviceMutationsResolver) Update(ctx context.Context, _ *generated.WinCCUaDeviceMutations, name string, input generated.WinCCUaClientInput) (*generated.WinCCUaClientResult, error) {
	if !r.Cfg.Features.WinCCUa {
		return featureDisabledResult(), nil
	}
	if name != input.Name {
		return &generated.WinCCUaClientResult{
			Success: false,
			Errors:  []string{"name in path must match name in input"},
		}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return &generated.WinCCUaClientResult{
			Success: false,
			Errors:  []string{fmt.Sprintf("Device %q not found", name)},
		}, nil
	}
	prevCfg, _ := winccua.ParseConnectionConfig(existing.Config)
	cfg := winCCUaInputToConfig(input.Config, prevCfg)

	// Preserve existing addresses (managed via add/update/deleteAddress).
	if prevCfg != nil {
		cfg.Addresses = prevCfg.Addresses
	}
	// Preserve existing password if blank in input — mirrors the Java
	// updateWinCCUaClient behaviour.
	if cfg.Password == "" && prevCfg != nil {
		cfg.Password = prevCfg.Password
	}

	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.WinCCUaClientResult{Success: false, Errors: errs}, nil
	}
	cfgJSON, _ := json.Marshal(cfg)

	updated := stores.DeviceConfig{
		Name:      input.Name,
		Namespace: input.Namespace,
		NodeID:    input.NodeID,
		Type:      winccua.DeviceTypeWinCCUaClient,
		Enabled:   boolPtr(input.Enabled, existing.Enabled),
		Config:    string(cfgJSON),
		CreatedAt: existing.CreatedAt,
	}
	if err := r.Storage.DeviceConfig.Save(ctx, updated); err != nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	saved, _ := r.Storage.DeviceConfig.Get(ctx, updated.Name)
	if saved == nil {
		saved = &updated
	}
	r.reloadWinCCUa(ctx, "update")
	return &generated.WinCCUaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCUaClient(*saved),
	}, nil
}

func (r *winCCUaDeviceMutationsResolver) Delete(ctx context.Context, _ *generated.WinCCUaDeviceMutations, name string) (bool, error) {
	if !r.Cfg.Features.WinCCUa {
		return false, nil
	}
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, nil
	}
	r.reloadWinCCUa(ctx, "delete")
	return true, nil
}

func (r *winCCUaDeviceMutationsResolver) Start(ctx context.Context, _ *generated.WinCCUaDeviceMutations, name string) (*generated.WinCCUaClientResult, error) {
	return r.toggle(ctx, name, true)
}

func (r *winCCUaDeviceMutationsResolver) Stop(ctx context.Context, _ *generated.WinCCUaDeviceMutations, name string) (*generated.WinCCUaClientResult, error) {
	return r.toggle(ctx, name, false)
}

func (r *winCCUaDeviceMutationsResolver) Toggle(ctx context.Context, _ *generated.WinCCUaDeviceMutations, name string, enabled bool) (*generated.WinCCUaClientResult, error) {
	return r.toggle(ctx, name, enabled)
}

func (r *winCCUaDeviceMutationsResolver) toggle(ctx context.Context, name string, enabled bool) (*generated.WinCCUaClientResult, error) {
	if !r.Cfg.Features.WinCCUa {
		return featureDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Toggle(ctx, name, enabled)
	if err != nil || d == nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{"not found"}}, nil
	}
	r.reloadWinCCUa(ctx, "toggle")
	return &generated.WinCCUaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCUaClient(*d),
	}, nil
}

func (r *winCCUaDeviceMutationsResolver) Reassign(ctx context.Context, _ *generated.WinCCUaDeviceMutations, name, nodeID string) (*generated.WinCCUaClientResult, error) {
	if !r.Cfg.Features.WinCCUa {
		return featureDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Reassign(ctx, name, nodeID)
	if err != nil || d == nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{"not found"}}, nil
	}
	r.reloadWinCCUa(ctx, "reassign")
	return &generated.WinCCUaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCUaClient(*d),
	}, nil
}

// AddAddress / UpdateAddress / DeleteAddress mutate the addresses array
// embedded in the device's stored JSON config.
func (r *winCCUaDeviceMutationsResolver) AddAddress(ctx context.Context, _ *generated.WinCCUaDeviceMutations, deviceName string, input generated.WinCCUaAddressInput) (*generated.WinCCUaClientResult, error) {
	addr := winCCUaAddressInputToConfig(input)
	if errs := addr.Validate(); len(errs) > 0 {
		return &generated.WinCCUaClientResult{Success: false, Errors: errs}, nil
	}
	return r.mutateAddresses(ctx, deviceName, func(addrs []winccua.Address) ([]winccua.Address, error) {
		for _, a := range addrs {
			if a.Topic == addr.Topic {
				return nil, fmt.Errorf("Address with topic %q already exists for device %q", addr.Topic, deviceName)
			}
		}
		return append(addrs, addr), nil
	})
}

func (r *winCCUaDeviceMutationsResolver) UpdateAddress(ctx context.Context, _ *generated.WinCCUaDeviceMutations, deviceName, topic string, input generated.WinCCUaAddressInput) (*generated.WinCCUaClientResult, error) {
	addr := winCCUaAddressInputToConfig(input)
	if errs := addr.Validate(); len(errs) > 0 {
		return &generated.WinCCUaClientResult{Success: false, Errors: errs}, nil
	}
	return r.mutateAddresses(ctx, deviceName, func(addrs []winccua.Address) ([]winccua.Address, error) {
		out := make([]winccua.Address, 0, len(addrs))
		replaced := false
		for _, a := range addrs {
			if !replaced && a.Topic == topic {
				out = append(out, addr)
				replaced = true
				continue
			}
			out = append(out, a)
		}
		if !replaced {
			return nil, fmt.Errorf("Address with topic %q not found for device %q", topic, deviceName)
		}
		return out, nil
	})
}

func (r *winCCUaDeviceMutationsResolver) DeleteAddress(ctx context.Context, _ *generated.WinCCUaDeviceMutations, deviceName, topic string) (*generated.WinCCUaClientResult, error) {
	return r.mutateAddresses(ctx, deviceName, func(addrs []winccua.Address) ([]winccua.Address, error) {
		out := make([]winccua.Address, 0, len(addrs))
		removed := false
		for _, a := range addrs {
			if a.Topic == topic {
				removed = true
				continue
			}
			out = append(out, a)
		}
		if !removed {
			return nil, fmt.Errorf("Address with topic %q not found for device %q", topic, deviceName)
		}
		return out, nil
	})
}

func (r *winCCUaDeviceMutationsResolver) mutateAddresses(ctx context.Context, deviceName string, fn func([]winccua.Address) ([]winccua.Address, error)) (*generated.WinCCUaClientResult, error) {
	if !r.Cfg.Features.WinCCUa {
		return featureDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, deviceName)
	if err != nil || d == nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{"not found"}}, nil
	}
	if d.Type != winccua.DeviceTypeWinCCUaClient {
		return &generated.WinCCUaClientResult{
			Success: false,
			Errors:  []string{fmt.Sprintf("Device %q is not a WinCC Unified client", deviceName)},
		}, nil
	}
	cfg, err := winccua.ParseConnectionConfig(d.Config)
	if err != nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	updated, err := fn(cfg.Addresses)
	if err != nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	cfg.Addresses = updated
	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.WinCCUaClientResult{Success: false, Errors: errs}, nil
	}
	cfgJSON, _ := json.Marshal(cfg)
	d.Config = string(cfgJSON)
	d.UpdatedAt = time.Now()
	if err := r.Storage.DeviceConfig.Save(ctx, *d); err != nil {
		return &generated.WinCCUaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	r.reloadWinCCUa(ctx, "address change")
	return &generated.WinCCUaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCUaClient(*d),
	}, nil
}

// Helpers ------------------------------------------------------------------

func (r *Resolver) reloadWinCCUa(ctx context.Context, reason string) {
	if r.WinCCUa == nil {
		return
	}
	if err := r.WinCCUa.Reload(ctx); err != nil {
		r.Logger.Warn("winccua reload after "+reason+" failed", "err", err)
	}
}

func featureDisabledResult() *generated.WinCCUaClientResult {
	return &generated.WinCCUaClientResult{
		Success: false,
		Errors:  []string{"WinCCUa feature is not enabled on this node"},
	}
}

func (r *Resolver) deviceToWinCCUaClient(d stores.DeviceConfig) *generated.WinCCUaClient {
	cfg, err := winccua.ParseConnectionConfig(d.Config)
	if err != nil || cfg == nil {
		cfg = &winccua.ConnectionConfig{}
	}
	return &generated.WinCCUaClient{
		Name:            d.Name,
		Namespace:       d.Namespace,
		NodeID:          d.NodeID,
		Enabled:         d.Enabled,
		Config:          winCCUaConfigToGraphQL(cfg),
		CreatedAt:       formatTime(d.CreatedAt),
		UpdatedAt:       formatTime(d.UpdatedAt),
		IsOnCurrentNode: d.NodeID == r.NodeID || d.NodeID == "*",
	}
}

func winCCUaConfigToGraphQL(c *winccua.ConnectionConfig) *generated.WinCCUaConnectionConfig {
	out := &generated.WinCCUaConnectionConfig{
		DataAccessMode:    generated.WinCCUaDataAccessMode(c.DataAccessMode),
		ReconnectDelay:    c.ReconnectDelay,
		ConnectionTimeout: c.ConnectionTimeout,
		MessageFormat:     generated.WinCCUaMessageFormat(c.MessageFormat),
		TransformConfig: &generated.WinCCUaTransformConfig{
			ConvertDotToSlash:        c.TransformConfig.ConvertDotToSlash,
			ConvertUnderscoreToSlash: c.TransformConfig.ConvertUnderscoreToSlash,
			RegexPattern:             ptrIfNotEmpty(c.TransformConfig.RegexPattern),
			RegexReplacement:         ptrIfNotEmpty(c.TransformConfig.RegexReplacement),
		},
		Addresses: []*generated.WinCCUaAddress{},
	}
	if v := strings.TrimSpace(c.GraphqlEndpoint); v != "" {
		out.GraphqlEndpoint = &c.GraphqlEndpoint
	}
	if v := strings.TrimSpace(c.WebsocketEndpoint); v != "" {
		out.WebsocketEndpoint = &c.WebsocketEndpoint
	}
	if v := strings.TrimSpace(c.Username); v != "" {
		out.Username = &c.Username
	}
	if v := strings.TrimSpace(c.Password); v != "" {
		out.Password = &c.Password
	}
	if v := strings.TrimSpace(c.PipePath); v != "" {
		out.PipePath = &c.PipePath
	}
	for _, a := range c.Addresses {
		ga := &generated.WinCCUaAddress{
			Type:     generated.WinCCUaAddressType(a.Type),
			Topic:    a.Topic,
			Retained: a.Retained,
		}
		if a.Description != "" {
			d := a.Description
			ga.Description = &d
		}
		if len(a.NameFilters) > 0 {
			ga.NameFilters = append([]string(nil), a.NameFilters...)
		}
		if a.Type == winccua.AddressTypeTagValues {
			iq := a.IncludeQuality
			ga.IncludeQuality = &iq
			ptm := a.PipeTagMode
			if ptm == "" {
				ptm = winccua.PipeTagModeSingle
			}
			ga.PipeTagMode = &ptm
		}
		if len(a.SystemNames) > 0 {
			ga.SystemNames = append([]string(nil), a.SystemNames...)
		}
		if a.FilterString != "" {
			fs := a.FilterString
			ga.FilterString = &fs
		}
		out.Addresses = append(out.Addresses, ga)
	}
	return out
}

func winCCUaInputToConfig(in *generated.WinCCUaConnectionConfigInput, prev *winccua.ConnectionConfig) *winccua.ConnectionConfig {
	out := &winccua.ConnectionConfig{}
	if prev != nil {
		out.Addresses = prev.Addresses
	}
	if in == nil {
		out.DataAccessMode = winccua.ModeGraphQL
		out.MessageFormat = winccua.FormatJSONISO
		out.ReconnectDelay = 5000
		out.ConnectionTimeout = 10000
		return out
	}
	if in.DataAccessMode != nil {
		out.DataAccessMode = string(*in.DataAccessMode)
	} else {
		out.DataAccessMode = winccua.ModeGraphQL
	}
	if in.GraphqlEndpoint != nil {
		out.GraphqlEndpoint = *in.GraphqlEndpoint
	} else if out.DataAccessMode == winccua.ModeGraphQL {
		out.GraphqlEndpoint = "http://winccua:4000/graphql"
	}
	if in.WebsocketEndpoint != nil {
		out.WebsocketEndpoint = *in.WebsocketEndpoint
	}
	if in.Username != nil {
		out.Username = *in.Username
	}
	if in.Password != nil {
		out.Password = *in.Password
	}
	if in.PipePath != nil {
		out.PipePath = *in.PipePath
	}
	if in.ReconnectDelay != nil {
		out.ReconnectDelay = *in.ReconnectDelay
	} else {
		out.ReconnectDelay = 5000
	}
	if in.ConnectionTimeout != nil {
		out.ConnectionTimeout = *in.ConnectionTimeout
	} else {
		out.ConnectionTimeout = 10000
	}
	if in.MessageFormat != nil {
		out.MessageFormat = *in.MessageFormat
	} else {
		out.MessageFormat = winccua.FormatJSONISO
	}
	if in.TransformConfig != nil {
		if in.TransformConfig.ConvertDotToSlash != nil {
			out.TransformConfig.ConvertDotToSlash = *in.TransformConfig.ConvertDotToSlash
		} else {
			out.TransformConfig.ConvertDotToSlash = true
		}
		if in.TransformConfig.ConvertUnderscoreToSlash != nil {
			out.TransformConfig.ConvertUnderscoreToSlash = *in.TransformConfig.ConvertUnderscoreToSlash
		}
		if in.TransformConfig.RegexPattern != nil {
			out.TransformConfig.RegexPattern = *in.TransformConfig.RegexPattern
		}
		if in.TransformConfig.RegexReplacement != nil {
			out.TransformConfig.RegexReplacement = *in.TransformConfig.RegexReplacement
		}
	} else {
		out.TransformConfig.ConvertDotToSlash = true
	}
	return out
}

func winCCUaAddressInputToConfig(in generated.WinCCUaAddressInput) winccua.Address {
	a := winccua.Address{
		Type:     string(in.Type),
		Topic:    in.Topic,
		Retained: boolPtr(in.Retained, false),
	}
	if in.Description != nil {
		a.Description = *in.Description
	}
	if len(in.NameFilters) > 0 {
		a.NameFilters = append([]string(nil), in.NameFilters...)
	}
	if in.IncludeQuality != nil {
		a.IncludeQuality = *in.IncludeQuality
	}
	if len(in.SystemNames) > 0 {
		a.SystemNames = append([]string(nil), in.SystemNames...)
	}
	if in.FilterString != nil {
		a.FilterString = *in.FilterString
	}
	if in.PipeTagMode != nil {
		a.PipeTagMode = *in.PipeTagMode
	}
	if a.PipeTagMode == "" && a.Type == winccua.AddressTypeTagValues {
		a.PipeTagMode = winccua.PipeTagModeSingle
	}
	return a
}

// Compile-time check that the resolver methods satisfy the generated
// interfaces. If a signature drifts after a future schema change this fails
// fast at build time.
var (
	_ generated.WinCCUaClientResolver          = (*winCCUaClientResolver)(nil)
	_ generated.WinCCUaDeviceMutationsResolver = (*winCCUaDeviceMutationsResolver)(nil)
)
