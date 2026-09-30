package oastore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
)

const (
	kindDevice  = "device"
	kindArchive = "archive"
	kindDBConn  = "dbconn"
	kindSession = "session"
)

// EnsureTypes creates the MMQConfigs / MMQSessions / MMQRetained datapoint
// types that are missing and checks the layout of all of them. An existing
// type with a different layout is never changed: it stops the broker from
// becoming ready instead of writing an incompatible layout.
func EnsureTypes(ctx context.Context, api oahost.API, needConfig, needSessions, needRetained bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	str, tim, bl := uint32(oahost.KindString), uint32(oahost.KindTime), uint32(oahost.KindBool)
	types := []struct {
		need     bool
		name     string
		elements []string
		kinds    []uint32
	}{
		{needConfig, ConfigType, []string{"config", "type", "updated"}, []uint32{str, str, tim}},
		{needSessions, SessionType, []string{"session", "subs", "connected", "nodeId", "updated"}, []uint32{str, str, bl, str, tim}},
		{needRetained, RetainedType, retainedElements, retainedElementKinds()},
	}
	for _, t := range types {
		if !t.need {
			continue
		}
		if err := api.EnsureType(ctx, t.name, t.elements, t.kinds); err != nil {
			return fmt.Errorf("datapoint type %s: %w (an existing type with a different layout is not changed)", t.name, err)
		}
	}
	return nil
}

// Stores bundles the OA-backed stores sharing one MMQConfigs cache.
type Stores struct {
	Device   *DeviceConfigStore
	Archive  *ArchiveConfigStore
	Sessions *SessionStore
	Retained *RetainedStore
}

func New(api oahost.API, timeout time.Duration, logger *slog.Logger) *Stores {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cfg := newRecordSet(api, ConfigType, "config", timeout, logger)
	ses := newRecordSet(api, SessionType, "session", timeout, logger)
	return &Stores{
		Device:   &DeviceConfigStore{r: cfg},
		Archive:  &ArchiveConfigStore{r: cfg},
		Sessions: &SessionStore{r: ses},
		Retained: newRetainedStore(api, timeout, logger),
	}
}

func typeValue(kind string) map[string]oahost.Value {
	return map[string]oahost.Value{"type": {Kind: oahost.KindString, Str: kind}}
}

// DeviceConfigStore implements stores.DeviceConfigStore on MMQConfigs.
type DeviceConfigStore struct{ r *recordSet }

var _ stores.DeviceConfigStore = (*DeviceConfigStore)(nil)

func (d *DeviceConfigStore) Close() error { return nil }

func (d *DeviceConfigStore) Load(ctx context.Context) error { return d.r.load(ctx) }

func (d *DeviceConfigStore) filter(ctx context.Context, keep func(stores.DeviceConfig) bool) ([]stores.DeviceConfig, error) {
	out := []stores.DeviceConfig{}
	err := d.r.all(ctx, kindDevice, func(_ string, data json.RawMessage) error {
		var dc stores.DeviceConfig
		if err := json.Unmarshal(data, &dc); err != nil {
			return err
		}
		if keep(dc) {
			out = append(out, dc)
		}
		return nil
	})
	return out, err
}

func onNode(dc stores.DeviceConfig, node string) bool {
	return dc.NodeID == node || dc.NodeID == "local" || dc.NodeID == "*"
}

func (d *DeviceConfigStore) GetAll(ctx context.Context) ([]stores.DeviceConfig, error) {
	return d.filter(ctx, func(stores.DeviceConfig) bool { return true })
}

func (d *DeviceConfigStore) GetByType(ctx context.Context, t string) ([]stores.DeviceConfig, error) {
	return d.filter(ctx, func(dc stores.DeviceConfig) bool { return dc.Type == t })
}

func (d *DeviceConfigStore) GetByNode(ctx context.Context, node string) ([]stores.DeviceConfig, error) {
	return d.filter(ctx, func(dc stores.DeviceConfig) bool { return onNode(dc, node) })
}

func (d *DeviceConfigStore) GetEnabledByNode(ctx context.Context, node string) ([]stores.DeviceConfig, error) {
	return d.filter(ctx, func(dc stores.DeviceConfig) bool { return dc.Enabled && onNode(dc, node) })
}

func (d *DeviceConfigStore) Get(ctx context.Context, name string) (*stores.DeviceConfig, error) {
	var dc stores.DeviceConfig
	ok, err := d.r.get(ctx, kindDevice, name, &dc)
	if err != nil || !ok {
		return nil, err
	}
	return &dc, nil
}

func (d *DeviceConfigStore) Save(ctx context.Context, dc stores.DeviceConfig) error {
	return d.r.update(ctx, kindDevice, dc.Name, func(cur json.RawMessage) (any, map[string]oahost.Value, bool, error) {
		now := time.Now().UTC()
		dc.CreatedAt = now
		if cur != nil {
			var prev stores.DeviceConfig
			if err := json.Unmarshal(cur, &prev); err != nil {
				return nil, nil, false, err
			}
			dc.CreatedAt = prev.CreatedAt
		}
		dc.UpdatedAt = now
		return dc, typeValue(kindDevice), false, nil
	})
}

func (d *DeviceConfigStore) Delete(ctx context.Context, name string) error {
	return d.r.del(ctx, kindDevice, name)
}

func (d *DeviceConfigStore) update(ctx context.Context, name string, fn func(*stores.DeviceConfig)) (*stores.DeviceConfig, error) {
	var out *stores.DeviceConfig
	err := d.r.update(ctx, kindDevice, name, func(cur json.RawMessage) (any, map[string]oahost.Value, bool, error) {
		if cur == nil {
			return nil, nil, true, nil
		}
		var dc stores.DeviceConfig
		if err := json.Unmarshal(cur, &dc); err != nil {
			return nil, nil, false, err
		}
		fn(&dc)
		dc.UpdatedAt = time.Now().UTC()
		out = &dc
		return dc, typeValue(kindDevice), false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (d *DeviceConfigStore) Toggle(ctx context.Context, name string, enabled bool) (*stores.DeviceConfig, error) {
	return d.update(ctx, name, func(dc *stores.DeviceConfig) { dc.Enabled = enabled })
}

func (d *DeviceConfigStore) Reassign(ctx context.Context, name, node string) (*stores.DeviceConfig, error) {
	return d.update(ctx, name, func(dc *stores.DeviceConfig) { dc.NodeID = node })
}

// ArchiveConfigStore implements stores.ArchiveConfigStore on MMQConfigs,
// including database-connection definitions.
type ArchiveConfigStore struct{ r *recordSet }

var _ stores.ArchiveConfigStore = (*ArchiveConfigStore)(nil)

func (a *ArchiveConfigStore) Close() error { return nil }

func (a *ArchiveConfigStore) GetAll(ctx context.Context) ([]stores.ArchiveGroupConfig, error) {
	out := []stores.ArchiveGroupConfig{}
	err := a.r.all(ctx, kindArchive, func(_ string, data json.RawMessage) error {
		var c stores.ArchiveGroupConfig
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

func (a *ArchiveConfigStore) Get(ctx context.Context, name string) (*stores.ArchiveGroupConfig, error) {
	var c stores.ArchiveGroupConfig
	ok, err := a.r.get(ctx, kindArchive, name, &c)
	if err != nil || !ok {
		return nil, err
	}
	return &c, nil
}

func (a *ArchiveConfigStore) Save(ctx context.Context, c stores.ArchiveGroupConfig) error {
	return a.r.put(ctx, kindArchive, c.Name, c, typeValue(kindArchive))
}

func (a *ArchiveConfigStore) Update(ctx context.Context, c stores.ArchiveGroupConfig) error {
	return a.Save(ctx, c)
}

func (a *ArchiveConfigStore) Delete(ctx context.Context, name string) error {
	return a.r.del(ctx, kindArchive, name)
}

func (a *ArchiveConfigStore) GetAllDatabaseConnections(ctx context.Context) ([]stores.DatabaseConnectionConfig, error) {
	out := []stores.DatabaseConnectionConfig{}
	err := a.r.all(ctx, kindDBConn, func(_ string, data json.RawMessage) error {
		var c stores.DatabaseConnectionConfig
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

func (a *ArchiveConfigStore) GetDatabaseConnection(ctx context.Context, name string) (*stores.DatabaseConnectionConfig, error) {
	var c stores.DatabaseConnectionConfig
	ok, err := a.r.get(ctx, kindDBConn, name, &c)
	if err != nil || !ok {
		return nil, err
	}
	return &c, nil
}

func (a *ArchiveConfigStore) SaveDatabaseConnection(ctx context.Context, c stores.DatabaseConnectionConfig) error {
	return a.r.update(ctx, kindDBConn, c.Name, func(cur json.RawMessage) (any, map[string]oahost.Value, bool, error) {
		now := time.Now().UTC()
		c.CreatedAt = now
		if cur != nil {
			var prev stores.DatabaseConnectionConfig
			if err := json.Unmarshal(cur, &prev); err != nil {
				return nil, nil, false, err
			}
			c.CreatedAt = prev.CreatedAt
		}
		c.UpdatedAt = now
		return c, typeValue(kindDBConn), false, nil
	})
}

func (a *ArchiveConfigStore) DeleteDatabaseConnection(ctx context.Context, name string) error {
	return a.r.del(ctx, kindDBConn, name)
}

// sessionRecord is the committed state of one client: metadata and its
// subscriptions are one envelope, so a reader never sees them torn apart.
type sessionRecord struct {
	Info stores.SessionInfo        `json:"info"`
	Subs []stores.MqttSubscription `json:"subs"`
}

// SessionStore implements stores.SessionStore on MMQSessions.
type SessionStore struct{ r *recordSet }

var _ stores.SessionStore = (*SessionStore)(nil)

func (s *SessionStore) Close() error { return nil }

func (s *SessionStore) Load(ctx context.Context) error { return s.r.load(ctx) }

func (s *SessionStore) read(ctx context.Context, clientID string) (*sessionRecord, error) {
	var rec sessionRecord
	ok, err := s.r.get(ctx, kindSession, clientID, &rec)
	if err != nil || !ok {
		return nil, err
	}
	return &rec, nil
}

func sessionExtra(rec *sessionRecord) map[string]oahost.Value {
	return map[string]oahost.Value{
		"connected": {Kind: oahost.KindBool, Bool: rec.Info.Connected},
		"nodeId":    {Kind: oahost.KindString, Str: rec.Info.NodeID},
	}
}

// change runs a serialized read-modify-write of one client's record. fn
// gets nil for a missing client and returns the record to write, or nil to
// write nothing.
func (s *SessionStore) change(ctx context.Context, clientID string, fn func(*sessionRecord) *sessionRecord) error {
	return s.r.update(ctx, kindSession, clientID, func(cur json.RawMessage) (any, map[string]oahost.Value, bool, error) {
		var rec *sessionRecord
		if cur != nil {
			rec = &sessionRecord{}
			if err := json.Unmarshal(cur, rec); err != nil {
				return nil, nil, false, err
			}
		}
		next := fn(rec)
		if next == nil {
			return nil, nil, true, nil
		}
		next.Info.ClientID = clientID
		next.Info.UpdateTime = time.Now().UTC()
		return next, sessionExtra(next), false, nil
	})
}

// modify applies fn to an existing session; a missing session is a no-op,
// matching the SQL UPDATE ... WHERE client_id semantics.
func (s *SessionStore) modify(ctx context.Context, clientID string, fn func(*sessionRecord)) error {
	return s.change(ctx, clientID, func(rec *sessionRecord) *sessionRecord {
		if rec == nil {
			return nil
		}
		fn(rec)
		return rec
	})
}

func (s *SessionStore) SetClient(ctx context.Context, info stores.SessionInfo) error {
	return s.change(ctx, info.ClientID, func(rec *sessionRecord) *sessionRecord {
		if rec == nil {
			rec = &sessionRecord{}
		}
		// Last-will fields are owned by SetLastWill, as in the SQL stores.
		info.LastWillTopic, info.LastWillPayload = rec.Info.LastWillTopic, rec.Info.LastWillPayload
		info.LastWillQoS, info.LastWillRetain = rec.Info.LastWillQoS, rec.Info.LastWillRetain
		rec.Info = info
		return rec
	})
}

func (s *SessionStore) SetConnected(ctx context.Context, clientID string, connected bool) error {
	return s.modify(ctx, clientID, func(r *sessionRecord) { r.Info.Connected = connected })
}

func (s *SessionStore) SetLastWill(ctx context.Context, clientID, topic string, payload []byte, qos byte, retain bool) error {
	return s.modify(ctx, clientID, func(r *sessionRecord) {
		r.Info.LastWillTopic = topic
		r.Info.LastWillPayload = append([]byte(nil), payload...)
		r.Info.LastWillQoS = qos
		r.Info.LastWillRetain = retain
	})
}

func (s *SessionStore) IsConnected(ctx context.Context, clientID string) (bool, error) {
	rec, err := s.read(ctx, clientID)
	if err != nil || rec == nil {
		return false, err
	}
	return rec.Info.Connected, nil
}

func (s *SessionStore) IsPresent(ctx context.Context, clientID string) (bool, error) {
	rec, err := s.read(ctx, clientID)
	return rec != nil, err
}

func (s *SessionStore) GetSession(ctx context.Context, clientID string) (*stores.SessionInfo, error) {
	rec, err := s.read(ctx, clientID)
	if err != nil || rec == nil {
		return nil, err
	}
	info := rec.Info
	return &info, nil
}

func (s *SessionStore) each(ctx context.Context, fn func(*sessionRecord) bool) error {
	stop := fmt.Errorf("stop")
	err := s.r.all(ctx, kindSession, func(_ string, data json.RawMessage) error {
		var rec sessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		if !fn(&rec) {
			return stop
		}
		return nil
	})
	if err == stop {
		return nil
	}
	return err
}

func (s *SessionStore) IterateSessions(ctx context.Context, yield func(stores.SessionInfo) bool) error {
	return s.each(ctx, func(r *sessionRecord) bool { return yield(r.Info) })
}

func (s *SessionStore) IterateSubscriptions(ctx context.Context, yield func(stores.MqttSubscription) bool) error {
	return s.each(ctx, func(r *sessionRecord) bool {
		for _, sub := range r.Subs {
			if !yield(sub) {
				return false
			}
		}
		return true
	})
}

func (s *SessionStore) GetSubscriptionsForClient(ctx context.Context, clientID string) ([]stores.MqttSubscription, error) {
	rec, err := s.read(ctx, clientID)
	if err != nil || rec == nil {
		return []stores.MqttSubscription{}, err
	}
	return append([]stores.MqttSubscription{}, rec.Subs...), nil
}

func groupByClient(subs []stores.MqttSubscription) map[string][]stores.MqttSubscription {
	out := map[string][]stores.MqttSubscription{}
	for _, sub := range subs {
		out[sub.ClientID] = append(out[sub.ClientID], sub)
	}
	return out
}

// AddSubscriptions upserts subscriptions. A subscription for a client
// without a session row creates a minimal row, like the SQL stores whose
// subscriptions table has no foreign key.
func (s *SessionStore) AddSubscriptions(ctx context.Context, subs []stores.MqttSubscription) error {
	for client, add := range groupByClient(subs) {
		err := s.change(ctx, client, func(rec *sessionRecord) *sessionRecord {
			if rec == nil {
				rec = &sessionRecord{}
			}
			for _, sub := range add {
				replaced := false
				for i := range rec.Subs {
					if rec.Subs[i].TopicFilter == sub.TopicFilter {
						rec.Subs[i] = sub
						replaced = true
					}
				}
				if !replaced {
					rec.Subs = append(rec.Subs, sub)
				}
			}
			return rec
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *SessionStore) DelSubscriptions(ctx context.Context, subs []stores.MqttSubscription) error {
	for client, del := range groupByClient(subs) {
		drop := map[string]bool{}
		for _, sub := range del {
			drop[sub.TopicFilter] = true
		}
		err := s.change(ctx, client, func(rec *sessionRecord) *sessionRecord {
			if rec == nil {
				return nil
			}
			kept := make([]stores.MqttSubscription, 0, len(rec.Subs))
			for _, sub := range rec.Subs {
				if !drop[sub.TopicFilter] {
					kept = append(kept, sub)
				}
			}
			if len(kept) == len(rec.Subs) {
				return nil
			}
			rec.Subs = kept
			return rec
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *SessionStore) DelClient(ctx context.Context, clientID string) error {
	return s.r.del(ctx, kindSession, clientID)
}

// PurgeSessions removes disconnected clean sessions, as the SQL stores do.
func (s *SessionStore) PurgeSessions(ctx context.Context) error {
	var purge []string
	if err := s.each(ctx, func(r *sessionRecord) bool {
		if r.Info.CleanSession && !r.Info.Connected {
			purge = append(purge, r.Info.ClientID)
		}
		return true
	}); err != nil {
		return err
	}
	for _, c := range purge {
		if err := s.DelClient(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
