package resolvers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/kafka"
	"monstermq.io/edge/internal/stores"
)

// Kafka protocol server (Kafka-Server devices), ported from Core's
// KafkaServerConfigQueries.kt / KafkaServerConfigMutations.kt.

const kafkaServerFeatureDisabled = "KafkaServer feature is not enabled on this node"

func kafkaServerDisabledResult() *generated.KafkaServerResult {
	return &generated.KafkaServerResult{Success: false, Errors: []string{kafkaServerFeatureDisabled}}
}

func (r *Resolver) KafkaServerMutations() generated.KafkaServerMutationsResolver {
	return &kafkaServerMutationsResolver{r}
}

type kafkaServerMutationsResolver struct{ *Resolver }

func (r *mutationResolver) KafkaServer(ctx context.Context) (*generated.KafkaServerMutations, error) {
	return &generated.KafkaServerMutations{}, nil
}

// Queries ---------------------------------------------------------------------

func (r *queryResolver) KafkaServers(ctx context.Context) ([]*generated.KafkaServerInfo, error) {
	out := []*generated.KafkaServerInfo{}
	if !r.Cfg.Features.KafkaServer {
		return out, nil
	}
	devices, err := r.Storage.DeviceConfig.GetAll(ctx)
	if err != nil {
		return out, nil
	}
	for _, d := range devices {
		if d.Type == kafka.DeviceTypeKafkaServer {
			out = append(out, r.deviceToKafkaServer(d))
		}
	}
	return out, nil
}

func (r *queryResolver) KafkaServer(ctx context.Context, name string) (*generated.KafkaServerInfo, error) {
	if !r.Cfg.Features.KafkaServer {
		return nil, nil
	}
	d, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || d == nil || d.Type != kafka.DeviceTypeKafkaServer {
		return nil, nil
	}
	return r.deviceToKafkaServer(*d), nil
}

// deviceToKafkaServer includes advertisedHost/advertisedPort, which Core's
// kafkaServers query drops (Core issue vogler75/monster-mq#237).
func (r *Resolver) deviceToKafkaServer(d stores.DeviceConfig) *generated.KafkaServerInfo {
	cfg, err := kafka.ParseConfig([]byte(d.Config))
	if err != nil {
		r.Logger.Warn("kafka server config parse failed", "name", d.Name, "err", err)
		cfg, _ = kafka.ParseConfig(nil)
	}
	streams := make([]*generated.KafkaStreamMappingInfo, 0, len(cfg.Streams))
	for _, s := range cfg.Streams {
		streams = append(streams, &generated.KafkaStreamMappingInfo{
			StreamName: s.StreamName, TopicFilter: s.TopicFilter, RetentionHours: s.RetentionHours,
			StoreType: s.StoreType, AllowWrite: s.AllowWrite,
		})
	}
	return &generated.KafkaServerInfo{
		Name: d.Name, Namespace: d.Namespace, NodeID: d.NodeID, Enabled: d.Enabled,
		Host: cfg.Host, Port: cfg.Port, AdvertisedHost: cfg.AdvertisedHost, AdvertisedPort: cfg.AdvertisedPort,
		StoreType: cfg.StoreType, Streams: streams,
		CreatedAt: formatTime(d.CreatedAt), UpdatedAt: formatTime(d.UpdatedAt),
		IsOnCurrentNode: d.NodeID == r.NodeID || d.NodeID == "local" || d.NodeID == "*",
		Status:          r.kafkaServerStatus(d),
	}
}

// kafkaServerStatus follows Core: STOPPED when disabled, else RUNNING,
// STARTING, or ERROR when the server is not active on this node.
func (r *Resolver) kafkaServerStatus(d stores.DeviceConfig) string {
	if !d.Enabled {
		return kafka.StatusStopped
	}
	if r.KafkaServerMgr != nil {
		if s := r.KafkaServerMgr.Status(d.Name); s != kafka.StatusStopped {
			return s
		}
	}
	return kafka.StatusError
}

func (r *queryResolver) KafkaConsumerGroups(ctx context.Context) ([]*generated.KafkaConsumerGroupInfo, error) {
	out := []*generated.KafkaConsumerGroupInfo{}
	if !r.Cfg.Features.KafkaServer || r.KafkaServerMgr == nil {
		return out, nil
	}
	merged := map[string]*kafka.GroupInfo{}
	for _, srv := range r.KafkaServerMgr.Snapshot() {
		groups, err := srv.ConsumerGroups(ctx)
		if err != nil {
			r.Logger.Warn("kafka consumer groups failed", "server", srv.Config().Name, "err", err)
		}
		for _, g := range groups {
			m := merged[g.GroupID]
			if m == nil {
				cp := g
				merged[g.GroupID] = &cp
				continue
			}
			for _, t := range g.Topics {
				if !slices.Contains(m.Topics, t) {
					m.Topics = append(m.Topics, t)
				}
			}
			if g.LastCommit.After(m.LastCommit) {
				m.LastCommit = g.LastCommit
			}
			m.Lag += g.Lag
		}
	}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := merged[id]
		topics := g.Topics
		if topics == nil {
			topics = []string{}
		}
		out = append(out, &generated.KafkaConsumerGroupInfo{
			GroupID: g.GroupID, Topics: topics, LastCommitTime: formatTime(g.LastCommit), Lag: g.Lag,
		})
	}
	return out, nil
}

func (r *queryResolver) KafkaMessages(ctx context.Context, serverName, topic string, startOffset *int64, limit *int) ([]*generated.KafkaMessageInfo, error) {
	out := []*generated.KafkaMessageInfo{}
	if !r.Cfg.Features.KafkaServer || r.KafkaServerMgr == nil {
		return out, nil
	}
	srv := r.KafkaServerMgr.Get(serverName)
	if srv == nil {
		return out, nil
	}
	start := int64(-1) // newest messages, as Core when no start offset is given
	if startOffset != nil && *startOffset >= 0 {
		start = *startOffset
	}
	msgs, err := srv.Messages(ctx, topic, start, intPtr(limit, 100))
	if err != nil {
		r.Logger.Warn("kafka messages failed", "server", serverName, "topic", topic, "err", err)
		return out, nil
	}
	for _, m := range msgs {
		out = append(out, &generated.KafkaMessageInfo{
			Offset: m.Offset, Topic: m.Topic, Partition: m.Partition,
			Timestamp: formatTime(m.Timestamp), Key: m.Key, Value: m.Value, Size: m.Size,
		})
	}
	return out, nil
}

func (r *queryResolver) KafkaTopicOffsets(ctx context.Context, serverName, topic string) (*generated.KafkaTopicOffsetInfo, error) {
	if !r.Cfg.Features.KafkaServer || r.KafkaServerMgr == nil {
		return nil, nil
	}
	srv := r.KafkaServerMgr.Get(serverName)
	if srv == nil {
		return nil, nil
	}
	earliest, latest, ok, err := srv.TopicOffsets(ctx, topic)
	if err != nil || !ok {
		return nil, nil
	}
	return &generated.KafkaTopicOffsetInfo{Topic: topic, EarliestOffset: earliest, LatestOffset: latest}, nil
}

// Mutations -------------------------------------------------------------------

// kafkaServerInputToConfig builds the stored JSON (without name, namespace,
// nodeId and enabled, which are device columns) and the validation errors.
func kafkaServerInputToConfig(in generated.KafkaServerInput) (string, []string) {
	var errs []string
	// Core's DeviceConfigRequest.validate().
	if strings.TrimSpace(in.Name) == "" {
		errs = append(errs, "name cannot be blank")
	}
	if !deviceNamePattern.MatchString(in.Name) {
		errs = append(errs, "name can only contain letters, numbers, underscores, and hyphens")
	}
	if strings.TrimSpace(in.Namespace) == "" {
		errs = append(errs, "namespace cannot be blank")
	}
	if strings.TrimSpace(in.NodeID) == "" {
		errs = append(errs, "nodeId cannot be blank")
	}
	cfg := kafka.ServerConfig{
		Name: "x", Host: "0.0.0.0", Port: 9092,
		AdvertisedHost: in.AdvertisedHost, AdvertisedPort: in.AdvertisedPort, StoreType: in.StoreType,
	}
	if in.Host != nil {
		cfg.Host = *in.Host
	}
	cfg.Port = intPtr(in.Port, 9092)
	cfg.Streams = []kafka.StreamMapping{}
	for _, s := range in.Streams {
		if s == nil {
			continue
		}
		cfg.Streams = append(cfg.Streams, kafka.StreamMapping{
			StreamName: s.StreamName, TopicFilter: s.TopicFilter,
			RetentionHours: intPtr(s.RetentionHours, 168), StoreType: s.StoreType, AllowWrite: boolPtr(s.AllowWrite, true),
		})
	}
	errs = append(errs, cfg.Validate()...)
	stored := map[string]any{
		"host": cfg.Host, "port": cfg.Port, "storeType": cfg.StoreType, "streams": cfg.Streams,
	}
	// Core omits the advertised fields when unset.
	if cfg.AdvertisedHost != nil {
		stored["advertisedHost"] = cfg.AdvertisedHost
	}
	if cfg.AdvertisedPort != nil {
		stored["advertisedPort"] = cfg.AdvertisedPort
	}
	raw, _ := json.Marshal(stored)
	return string(raw), errs
}

func (r *kafkaServerMutationsResolver) Add(ctx context.Context, _ *generated.KafkaServerMutations, input generated.KafkaServerInput) (*generated.KafkaServerResult, error) {
	if !r.Cfg.Features.KafkaServer {
		return kafkaServerDisabledResult(), nil
	}
	raw, errs := kafkaServerInputToConfig(input)
	if len(errs) > 0 {
		return &generated.KafkaServerResult{Success: false, Errors: errs}, nil
	}
	if existing, _ := r.Storage.DeviceConfig.Get(ctx, input.Name); existing != nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{fmt.Sprintf("Device with name '%s' already exists", input.Name)}}, nil
	}
	d := stores.DeviceConfig{
		Name: input.Name, Namespace: input.Namespace, NodeID: input.NodeID,
		Type: kafka.DeviceTypeKafkaServer, Enabled: boolPtr(input.Enabled, true), Config: raw,
	}
	if err := r.Storage.DeviceConfig.Save(ctx, d); err != nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{"Failed to save device: " + err.Error()}}, nil
	}
	return r.kafkaServerResult(ctx, d, "add"), nil
}

func (r *kafkaServerMutationsResolver) Update(ctx context.Context, _ *generated.KafkaServerMutations, name string, input generated.KafkaServerInput) (*generated.KafkaServerResult, error) {
	if !r.Cfg.Features.KafkaServer {
		return kafkaServerDisabledResult(), nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{fmt.Sprintf("Device '%s' not found", name)}}, nil
	}
	raw, errs := kafkaServerInputToConfig(input)
	if len(errs) > 0 {
		return &generated.KafkaServerResult{Success: false, Errors: errs}, nil
	}
	d := stores.DeviceConfig{
		Name: input.Name, Namespace: input.Namespace, NodeID: input.NodeID,
		Type: kafka.DeviceTypeKafkaServer, Enabled: boolPtr(input.Enabled, true), Config: raw,
		CreatedAt: existing.CreatedAt,
	}
	if err := r.Storage.DeviceConfig.Save(ctx, d); err != nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{"Failed to update device: " + err.Error()}}, nil
	}
	return r.kafkaServerResult(ctx, d, "update"), nil
}

func (r *Resolver) kafkaServerResult(ctx context.Context, d stores.DeviceConfig, reason string) *generated.KafkaServerResult {
	saved, _ := r.Storage.DeviceConfig.Get(ctx, d.Name)
	if saved == nil {
		saved = &d
	}
	r.reloadKafkaServers(ctx, reason)
	return &generated.KafkaServerResult{Success: true, Errors: []string{}, Server: r.deviceToKafkaServer(*saved)}
}

func (r *kafkaServerMutationsResolver) Delete(ctx context.Context, _ *generated.KafkaServerMutations, name string) (bool, error) {
	if !r.Cfg.Features.KafkaServer {
		return false, nil
	}
	existing, err := r.Storage.DeviceConfig.Get(ctx, name)
	if err != nil || existing == nil {
		return false, nil
	}
	if err := r.Storage.DeviceConfig.Delete(ctx, name); err != nil {
		return false, nil
	}
	r.reloadKafkaServers(ctx, "delete")
	return true, nil
}

func (r *kafkaServerMutationsResolver) Toggle(ctx context.Context, _ *generated.KafkaServerMutations, name string, enabled bool) (*generated.KafkaServerResult, error) {
	if !r.Cfg.Features.KafkaServer {
		return kafkaServerDisabledResult(), nil
	}
	d, err := r.Storage.DeviceConfig.Toggle(ctx, name, enabled)
	if err != nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{"Failed to toggle device: " + err.Error()}}, nil
	}
	if d == nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{fmt.Sprintf("Device '%s' not found", name)}}, nil
	}
	r.reloadKafkaServers(ctx, "toggle")
	return &generated.KafkaServerResult{Success: true, Errors: []string{}, Server: r.deviceToKafkaServer(*d)}, nil
}

func (r *kafkaServerMutationsResolver) Reassign(ctx context.Context, _ *generated.KafkaServerMutations, name, nodeID string) (*generated.KafkaServerResult, error) {
	if !r.Cfg.Features.KafkaServer {
		return kafkaServerDisabledResult(), nil
	}
	if nodes := r.clusterNodeIDs(); !slices.Contains(nodes, nodeID) {
		return &generated.KafkaServerResult{Success: false, Errors: []string{
			fmt.Sprintf("Cluster node '%s' not found. Available nodes: %s", nodeID, strings.Join(nodes, ", ")),
		}}, nil
	}
	d, err := r.Storage.DeviceConfig.Reassign(ctx, name, nodeID)
	if err != nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{"Failed to reassign device: " + err.Error()}}, nil
	}
	if d == nil {
		return &generated.KafkaServerResult{Success: false, Errors: []string{fmt.Sprintf("Device '%s' not found", name)}}, nil
	}
	r.reloadKafkaServers(ctx, "reassign")
	return &generated.KafkaServerResult{Success: true, Errors: []string{}, Server: r.deviceToKafkaServer(*d)}, nil
}

// DeleteConsumerGroup removes the group's offsets from every running server.
func (r *kafkaServerMutationsResolver) DeleteConsumerGroup(ctx context.Context, _ *generated.KafkaServerMutations, groupID string) (bool, error) {
	if !r.Cfg.Features.KafkaServer || r.KafkaServerMgr == nil {
		return false, nil
	}
	deleted := false
	for _, srv := range r.KafkaServerMgr.Snapshot() {
		ok, err := srv.DeleteConsumerGroup(ctx, groupID)
		if err != nil {
			r.Logger.Warn("kafka delete consumer group failed", "server", srv.Config().Name, "group", groupID, "err", err)
			continue
		}
		deleted = deleted || ok
	}
	return deleted, nil
}

func (r *Resolver) reloadKafkaServers(ctx context.Context, reason string) {
	if r.KafkaServerMgr == nil {
		return
	}
	if err := r.KafkaServerMgr.Reload(ctx); err != nil {
		r.Logger.Warn("kafka server reload after "+reason+" failed", "err", err)
	}
}
