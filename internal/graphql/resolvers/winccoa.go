package resolvers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"monstermq.io/edge/internal/bridge/winccoa"
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
)

func (r *queryResolver) WinCCOaClients(ctx context.Context, name, node *string) ([]*generated.WinCCOaClient, error) {
	if !r.Cfg.Features.WinCCOa {
		return []*generated.WinCCOaClient{}, nil
	}
	devices, err := r.Storage.DeviceConfig.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	out := []*generated.WinCCOaClient{}
	for _, d := range devices {
		if d.Type != winccoa.DeviceTypeWinCCOaClient {
			continue
		}
		if name != nil && d.Name != *name {
			continue
		}
		if node != nil && d.NodeID != *node {
			continue
		}
		out = append(out, r.deviceToWinCCOaClient(d))
	}
	return out, nil
}

func (r *winCCOaClientResolver) Metrics(ctx context.Context, obj *generated.WinCCOaClient) ([]*generated.WinCCOaClientMetrics, error) {
	if r.WinCCOa == nil || obj == nil {
		return []*generated.WinCCOaClientMetrics{{Timestamp: nowISO()}}, nil
	}
	c := r.WinCCOa.Connector(obj.Name)
	if c == nil {
		return []*generated.WinCCOaClientMetrics{{Timestamp: nowISO()}}, nil
	}
	return []*generated.WinCCOaClientMetrics{{
		MessagesIn: c.MessagesIn(),
		Connected:  c.IsConnected(),
		Timestamp:  nowISO(),
	}}, nil
}

func (r *winCCOaClientResolver) MetricsHistory(ctx context.Context, obj *generated.WinCCOaClient, from, to *string, lastMinutes *int) ([]*generated.WinCCOaClientMetrics, error) {
	if r.Storage.Metrics == nil {
		return []*generated.WinCCOaClientMetrics{}, nil
	}
	now := time.Now()
	end := now
	start := now.Add(-24 * time.Hour)
	if lastMinutes != nil && *lastMinutes > 0 {
		start = now.Add(-time.Duration(*lastMinutes) * time.Minute)
	} else {
		if t, err := parseTimeArg(from); err == nil && t != nil {
			start = *t
		}
		if t, err := parseTimeArg(to); err == nil && t != nil {
			end = *t
		}
	}
	rows, err := r.Storage.Metrics.GetHistory(ctx, stores.MetricWinCCOa, obj.Name, start, end, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]*generated.WinCCOaClientMetrics, 0, len(rows))
	for _, row := range rows {
		var snap winccoa.MetricsSnapshot
		_ = json.Unmarshal([]byte(row.Payload), &snap)
		m := winCCOaSnapshotToMetrics(snap)
		m.Timestamp = formatTime(row.Timestamp)
		out = append(out, m)
	}
	return out, nil
}

func (r *winCCOaDeviceMutationsResolver) Create(ctx context.Context, _ *generated.WinCCOaDeviceMutations, input generated.WinCCOaClientInput) (*generated.WinCCOaClientResult, error) {
	if !r.Cfg.Features.WinCCOa {
		return winCCOaFeatureDisabledResult(), nil
	}
	cfg := winCCOaInputToConfig(input.Config, nil)
	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.WinCCOaClientResult{Success: false, Errors: errs}, nil
	}
	cfgJSON, _ := json.Marshal(cfg)

	existing, _ := r.Storage.DeviceConfig.Get(ctx, input.Name)
	if existing != nil {
		return &generated.WinCCOaClientResult{
			Success: false,
			Errors:  []string{fmt.Sprintf("Device with name %q already exists", input.Name)},
		}, nil
	}

	d := stores.DeviceConfig{
		Name:      input.Name,
		Namespace: input.Namespace,
		NodeID:    input.NodeID,
		Type:      winccoa.DeviceTypeWinCCOaClient,
		Enabled:   boolPtr(input.Enabled, true),
		Config:    string(cfgJSON),
	}
	if err := r.Storage.DeviceConfig.Save(ctx, d); err != nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	saved, _ := r.Storage.DeviceConfig.Get(ctx, d.Name)
	if saved == nil {
		saved = &d
	}
	r.reloadWinCCOa(ctx, "create")
	return &generated.WinCCOaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCOaClient(*saved),
	}, nil
}

func (r *winCCOaDeviceMutationsResolver) Update(ctx context.Context, _ *generated.WinCCOaDeviceMutations, name string, input generated.WinCCOaClientInput) (*generated.WinCCOaClientResult, error) {
	if !r.Cfg.Features.WinCCOa {
		return winCCOaFeatureDisabledResult(), nil
	}
	if name != input.Name {
		return &generated.WinCCOaClientResult{
			Success: false,
			Errors:  []string{"name in path must match name in input"},
		}, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return &generated.WinCCOaClientResult{
			Success: false,
			Errors:  []string{fmt.Sprintf("Device %q not found", name)},
		}, nil
	}
	prevCfg, _ := winccoa.ParseConnectionConfig(existing.Config)
	cfg := winCCOaInputToConfig(input.Config, prevCfg)
	if prevCfg != nil {
		cfg.Addresses = prevCfg.Addresses
	}
	if cfg.Password == "" && prevCfg != nil {
		cfg.Password = prevCfg.Password
	}

	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.WinCCOaClientResult{Success: false, Errors: errs}, nil
	}
	cfgJSON, _ := json.Marshal(cfg)

	updated := stores.DeviceConfig{
		Name:      input.Name,
		Namespace: input.Namespace,
		NodeID:    input.NodeID,
		Type:      winccoa.DeviceTypeWinCCOaClient,
		Enabled:   boolPtr(input.Enabled, existing.Enabled),
		Config:    string(cfgJSON),
		CreatedAt: existing.CreatedAt,
	}
	if err := r.Storage.DeviceConfig.Save(ctx, updated); err != nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	saved, _ := r.Storage.DeviceConfig.Get(ctx, updated.Name)
	if saved == nil {
		saved = &updated
	}
	r.reloadWinCCOa(ctx, "update")
	return &generated.WinCCOaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCOaClient(*saved),
	}, nil
}

func (r *winCCOaDeviceMutationsResolver) Delete(ctx context.Context, _ *generated.WinCCOaDeviceMutations, name string) (bool, error) {
	if !r.Cfg.Features.WinCCOa {
		return false, nil
	}
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, nil
	}
	r.reloadWinCCOa(ctx, "delete")
	return true, nil
}

func (r *winCCOaDeviceMutationsResolver) Start(ctx context.Context, _ *generated.WinCCOaDeviceMutations, name string) (*generated.WinCCOaClientResult, error) {
	return r.toggle(ctx, name, true)
}

func (r *winCCOaDeviceMutationsResolver) Stop(ctx context.Context, _ *generated.WinCCOaDeviceMutations, name string) (*generated.WinCCOaClientResult, error) {
	return r.toggle(ctx, name, false)
}

func (r *winCCOaDeviceMutationsResolver) Toggle(ctx context.Context, _ *generated.WinCCOaDeviceMutations, name string, enabled bool) (*generated.WinCCOaClientResult, error) {
	return r.toggle(ctx, name, enabled)
}

func (r *winCCOaDeviceMutationsResolver) toggle(ctx context.Context, name string, enabled bool) (*generated.WinCCOaClientResult, error) {
	if !r.Cfg.Features.WinCCOa {
		return winCCOaFeatureDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Toggle(ctx, name, enabled)
	if err != nil || d == nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{"not found"}}, nil
	}
	r.reloadWinCCOa(ctx, "toggle")
	return &generated.WinCCOaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCOaClient(*d),
	}, nil
}

func (r *winCCOaDeviceMutationsResolver) Reassign(ctx context.Context, _ *generated.WinCCOaDeviceMutations, name, nodeID string) (*generated.WinCCOaClientResult, error) {
	if !r.Cfg.Features.WinCCOa {
		return winCCOaFeatureDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Reassign(ctx, name, nodeID)
	if err != nil || d == nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{"not found"}}, nil
	}
	r.reloadWinCCOa(ctx, "reassign")
	return &generated.WinCCOaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCOaClient(*d),
	}, nil
}

func (r *winCCOaDeviceMutationsResolver) AddAddress(ctx context.Context, _ *generated.WinCCOaDeviceMutations, deviceName string, input generated.WinCCOaAddressInput) (*generated.WinCCOaClientResult, error) {
	addr := winCCOaAddressInputToConfig(input)
	if errs := addr.Validate(); len(errs) > 0 {
		return &generated.WinCCOaClientResult{Success: false, Errors: errs}, nil
	}
	return r.mutateAddresses(ctx, deviceName, func(addrs []winccoa.Address) ([]winccoa.Address, error) {
		for _, a := range addrs {
			if a.Query == addr.Query {
				return nil, fmt.Errorf("Address with query %q already exists for device %q", addr.Query, deviceName)
			}
		}
		return append(addrs, addr), nil
	})
}

func (r *winCCOaDeviceMutationsResolver) UpdateAddress(ctx context.Context, _ *generated.WinCCOaDeviceMutations, deviceName, query string, input generated.WinCCOaAddressInput) (*generated.WinCCOaClientResult, error) {
	addr := winCCOaAddressInputToConfig(input)
	if errs := addr.Validate(); len(errs) > 0 {
		return &generated.WinCCOaClientResult{Success: false, Errors: errs}, nil
	}
	return r.mutateAddresses(ctx, deviceName, func(addrs []winccoa.Address) ([]winccoa.Address, error) {
		out := make([]winccoa.Address, 0, len(addrs))
		replaced := false
		for _, a := range addrs {
			if !replaced && a.Query == query {
				out = append(out, addr)
				replaced = true
				continue
			}
			out = append(out, a)
		}
		if !replaced {
			return nil, fmt.Errorf("Address with query %q not found for device %q", query, deviceName)
		}
		return out, nil
	})
}

func (r *winCCOaDeviceMutationsResolver) DeleteAddress(ctx context.Context, _ *generated.WinCCOaDeviceMutations, deviceName, query string) (*generated.WinCCOaClientResult, error) {
	return r.mutateAddresses(ctx, deviceName, func(addrs []winccoa.Address) ([]winccoa.Address, error) {
		out := make([]winccoa.Address, 0, len(addrs))
		removed := false
		for _, a := range addrs {
			if a.Query == query {
				removed = true
				continue
			}
			out = append(out, a)
		}
		if !removed {
			return nil, fmt.Errorf("Address with query %q not found for device %q", query, deviceName)
		}
		return out, nil
	})
}

func (r *winCCOaDeviceMutationsResolver) mutateAddresses(ctx context.Context, deviceName string, fn func([]winccoa.Address) ([]winccoa.Address, error)) (*generated.WinCCOaClientResult, error) {
	if !r.Cfg.Features.WinCCOa {
		return winCCOaFeatureDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, deviceName)
	if err != nil || d == nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{"not found"}}, nil
	}
	if d.Type != winccoa.DeviceTypeWinCCOaClient {
		return &generated.WinCCOaClientResult{
			Success: false,
			Errors:  []string{fmt.Sprintf("Device %q is not a WinCC OA client", deviceName)},
		}, nil
	}
	cfg, err := winccoa.ParseConnectionConfig(d.Config)
	if err != nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	updated, err := fn(cfg.Addresses)
	if err != nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	cfg.Addresses = updated
	if errs := cfg.Validate(); len(errs) > 0 {
		return &generated.WinCCOaClientResult{Success: false, Errors: errs}, nil
	}
	cfgJSON, _ := json.Marshal(cfg)
	d.Config = string(cfgJSON)
	d.UpdatedAt = time.Now()
	if err := r.Storage.DeviceConfig.Save(ctx, *d); err != nil {
		return &generated.WinCCOaClientResult{Success: false, Errors: []string{err.Error()}}, nil
	}
	r.reloadWinCCOa(ctx, "address change")
	return &generated.WinCCOaClientResult{
		Success: true, Errors: []string{},
		Client: r.deviceToWinCCOaClient(*d),
	}, nil
}

func (r *Resolver) reloadWinCCOa(ctx context.Context, reason string) {
	if r.WinCCOa == nil {
		return
	}
	if err := r.WinCCOa.Reload(ctx); err != nil {
		r.Logger.Warn("winccoa reload after "+reason+" failed", "err", err)
	}
}

func winCCOaFeatureDisabledResult() *generated.WinCCOaClientResult {
	return &generated.WinCCOaClientResult{
		Success: false,
		Errors:  []string{"WinCCOa feature is not enabled on this node"},
	}
}

func winCCOaSnapshotToMetrics(s winccoa.MetricsSnapshot) *generated.WinCCOaClientMetrics {
	ts := s.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return &generated.WinCCOaClientMetrics{
		MessagesIn: s.MessagesIn,
		Connected:  s.Connected,
		Timestamp:  formatTime(ts),
	}
}

func (r *Resolver) deviceToWinCCOaClient(d stores.DeviceConfig) *generated.WinCCOaClient {
	cfg, err := winccoa.ParseConnectionConfig(d.Config)
	if err != nil || cfg == nil {
		cfg = &winccoa.ConnectionConfig{}
	}
	return &generated.WinCCOaClient{
		Name:            d.Name,
		Namespace:       d.Namespace,
		NodeID:          d.NodeID,
		Enabled:         d.Enabled,
		Config:          winCCOaConfigToGraphQL(cfg),
		CreatedAt:       formatTime(d.CreatedAt),
		UpdatedAt:       formatTime(d.UpdatedAt),
		IsOnCurrentNode: d.NodeID == r.NodeID || d.NodeID == "*",
	}
}

func winCCOaConfigToGraphQL(c *winccoa.ConnectionConfig) *generated.WinCCOaConnectionConfig {
	out := &generated.WinCCOaConnectionConfig{
		ReconnectDelay:    c.ReconnectDelay,
		ConnectionTimeout: c.ConnectionTimeout,
		MessageFormat:     generated.WinCCOaMessageFormat(c.MessageFormat),
		TransformConfig: &generated.WinCCOaTransformConfig{
			RemoveSystemName:         c.TransformConfig.RemoveSystemName,
			ConvertDotToSlash:        c.TransformConfig.ConvertDotToSlash,
			ConvertUnderscoreToSlash: c.TransformConfig.ConvertUnderscoreToSlash,
			RegexPattern:             ptrIfNotEmpty(c.TransformConfig.RegexPattern),
			RegexReplacement:         ptrIfNotEmpty(c.TransformConfig.RegexReplacement),
		},
		Addresses: []*generated.WinCCOaAddress{},
	}
	out.GraphqlEndpoint = c.GraphqlEndpoint
	if v := strings.TrimSpace(c.WebsocketEndpoint); v != "" {
		out.WebsocketEndpoint = &c.WebsocketEndpoint
	}
	if v := strings.TrimSpace(c.Username); v != "" {
		out.Username = &c.Username
	}
	if v := strings.TrimSpace(c.Token); v != "" {
		out.Token = &c.Token
	}
	for _, a := range c.Addresses {
		ga := &generated.WinCCOaAddress{
			Query:       a.Query,
			Topic:       a.Topic,
			Description: a.Description,
			Answer:      a.Answer,
			Retained:    a.Retained,
		}
		out.Addresses = append(out.Addresses, ga)
	}
	return out
}

func winCCOaInputToConfig(in *generated.WinCCOaConnectionConfigInput, prev *winccoa.ConnectionConfig) *winccoa.ConnectionConfig {
	out := &winccoa.ConnectionConfig{}
	if prev != nil {
		out.Addresses = prev.Addresses
	}
	if in == nil {
		out.GraphqlEndpoint = "http://winccoa:4000/graphql"
		out.MessageFormat = winccoa.FormatJSONISO
		out.ReconnectDelay = 5000
		out.ConnectionTimeout = 10000
		out.TransformConfig.RemoveSystemName = true
		out.TransformConfig.ConvertDotToSlash = true
		return out
	}
	if in.GraphqlEndpoint != nil {
		out.GraphqlEndpoint = *in.GraphqlEndpoint
	} else {
		out.GraphqlEndpoint = "http://winccoa:4000/graphql"
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
	if in.Token != nil {
		out.Token = *in.Token
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
		out.MessageFormat = string(*in.MessageFormat)
	} else {
		out.MessageFormat = winccoa.FormatJSONISO
	}
	if in.TransformConfig != nil {
		if in.TransformConfig.RemoveSystemName != nil {
			out.TransformConfig.RemoveSystemName = *in.TransformConfig.RemoveSystemName
		} else {
			out.TransformConfig.RemoveSystemName = true
		}
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
		out.TransformConfig.RemoveSystemName = true
		out.TransformConfig.ConvertDotToSlash = true
	}
	if in.Addresses != nil {
		out.Addresses = make([]winccoa.Address, 0, len(in.Addresses))
		for _, item := range in.Addresses {
			if item == nil {
				continue
			}
			out.Addresses = append(out.Addresses, winCCOaAddressInputToConfig(*item))
		}
	}
	return out
}

func winCCOaAddressInputToConfig(in generated.WinCCOaAddressInput) winccoa.Address {
	a := winccoa.Address{
		Query:    in.Query,
		Topic:    in.Topic,
		Answer:   boolPtr(in.Answer, false),
		Retained: boolPtr(in.Retained, false),
	}
	if in.Description != nil {
		a.Description = *in.Description
	}
	return a
}

var (
	_ generated.WinCCOaClientResolver          = (*winCCOaClientResolver)(nil)
	_ generated.WinCCOaDeviceMutationsResolver = (*winCCOaDeviceMutationsResolver)(nil)
)
