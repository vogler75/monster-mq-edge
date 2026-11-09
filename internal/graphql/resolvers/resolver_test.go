package resolvers

import (
	"context"
	"path/filepath"
	"testing"

	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

func TestMqttClientConfigUpdatePreservesAddressesWhenOmitted(t *testing.T) {
	existing := `{"brokerUrl":"tcp://old","password":"secret","addresses":[{"mode":"PUBLISH","remoteTopic":"remote/#","localTopic":"local/#","removePath":true}]}`
	clientID := "client"
	cfg := mqttClientConfigInputToMergedMap(&generated.MqttClientConnectionConfigInput{
		BrokerURL: "tcp://new",
		ClientID:  &clientID,
	}, existing)

	addrs, ok := cfg["addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("addresses = %#v, want one preserved address", cfg["addresses"])
	}
	if cfg["password"] != "secret" {
		t.Fatalf("password = %#v, want preserved secret", cfg["password"])
	}
}

func TestMqttClientConfigUpdatePreservesRedundancy(t *testing.T) {
	cfg := mqttClientConfigInputToMergedMap(&generated.MqttClientConnectionConfigInput{BrokerURL: "tcp://new"},
		`{"brokerUrl":"tcp://old","redundancy":"HOT_STANDBY"}`)
	if cfg["redundancy"] != "HOT_STANDBY" {
		t.Fatalf("redundancy = %#v, want preserved HOT_STANDBY", cfg["redundancy"])
	}
}

func TestMqttClientConfigUpdateCanClearAddresses(t *testing.T) {
	existing := `{"addresses":[{"mode":"PUBLISH","remoteTopic":"remote/#","localTopic":"local/#","removePath":true}]}`
	cfg := mqttClientConfigInputToMergedMap(&generated.MqttClientConnectionConfigInput{
		BrokerURL: "tcp://new",
		Addresses: []*generated.MqttClientAddressInput{},
	}, existing)

	addrs, ok := cfg["addresses"].([]map[string]any)
	if !ok || len(addrs) != 0 {
		t.Fatalf("addresses = %#v, want explicit empty address list", cfg["addresses"])
	}
}

func TestDatabaseConnectionTypeSQLiteMapping(t *testing.T) {
	gt := toDatabaseConnectionType("SQLITE")
	if gt != generated.DatabaseConnectionTypeSQLIte {
		t.Fatalf("toDatabaseConnectionType(SQLITE) = %v, want SQLITE", gt)
	}

	st := fromDatabaseConnectionType(generated.DatabaseConnectionTypeSQLIte)
	if st != "SQLITE" {
		t.Fatalf("fromDatabaseConnectionType(SQLITE) = %v, want SQLITE", st)
	}
}

func TestScriptDocumentationAndSkill(t *testing.T) {
	r := &Resolver{
		Cfg: &config.Config{
			Features: config.FeaturesConfig{
				PythonScripts: true,
			},
		},
	}
	qr := &queryResolver{r}
	ctx := context.Background()

	langs, err := qr.ScriptLanguages(ctx)
	if err != nil {
		t.Fatalf("ScriptLanguages error: %v", err)
	}
	if len(langs) == 0 {
		t.Fatalf("expected at least 1 script language")
	}
	star := langs[0]
	if star.Name != "starlark" {
		t.Errorf("expected language starlark, got %s", star.Name)
	}
	if len(star.Documentation) == 0 {
		t.Errorf("expected non-empty documentation for starlark")
	}
	if len(star.Skill) == 0 {
		t.Errorf("expected non-empty skill for starlark")
	}

	doc, err := qr.ScriptDocumentation(ctx, nil)
	if err != nil {
		t.Fatalf("ScriptDocumentation error: %v", err)
	}
	if len(doc) == 0 {
		t.Errorf("expected non-empty default ScriptDocumentation")
	}

	skill, err := qr.ScriptSkill(ctx, nil)
	if err != nil {
		t.Fatalf("ScriptSkill error: %v", err)
	}
	if len(skill) == 0 {
		t.Errorf("expected non-empty default ScriptSkill")
	}
}

func TestSessionToGraphQLProtocolSessions(t *testing.T) {
	for _, id := range []string{"nats-123", "redis-1", "mcp-abc"} {
		gql := sessionToGraphQL(stores.SessionInfo{
			ClientID:    id,
			Connected:   true,
			Information: `{"RemoteAddress":"127.0.0.1:1234","ProtocolVersion":"NATS","SSL":false}`,
		})
		if gql.ProtocolVersion != nil || gql.ReceiveMaximum != nil || gql.MaximumPacketSize != nil || gql.TopicAliasMaximum != nil {
			t.Errorf("%s: expected nil MQTT protocol fields, got pv=%v rm=%v mps=%v tam=%v",
				id, gql.ProtocolVersion, gql.ReceiveMaximum, gql.MaximumPacketSize, gql.TopicAliasMaximum)
		}
	}

	mqttGQL := sessionToGraphQL(stores.SessionInfo{
		ClientID:        "sensor-1",
		ProtocolVersion: 5,
		ReceiveMaximum:  65535,
	})
	if mqttGQL.ProtocolVersion == nil || *mqttGQL.ProtocolVersion != 5 {
		t.Errorf("mqtt session ProtocolVersion = %v, want 5", mqttGQL.ProtocolVersion)
	}
}

func TestAnonymousAndAdminAuth(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.UserManagement.Enabled = true
	cfg.UserManagement.AnonymousEnabled = true
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "monstermq.db")

	storage, db, err := storesqlite.Build(ctx, cfg)
	if err != nil {
		t.Fatalf("build storage: %v", err)
	}
	defer db.Close()

	// Create default users (Admin and Anonymous)
	adminHash, err := storesqlite.HashPassword("Admin")
	if err != nil {
		t.Fatalf("hash admin: %v", err)
	}
	if err := storage.Users.CreateUser(ctx, stores.User{
		Username:     "Admin",
		PasswordHash: adminHash,
		Enabled:      true,
		CanSubscribe: true,
		CanPublish:   true,
		IsAdmin:      true,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := storage.Users.CreateUser(ctx, stores.User{
		Username:     "Anonymous",
		PasswordHash: "",
		Enabled:      true,
		CanSubscribe: false,
		CanPublish:   false,
		IsAdmin:      false,
	}); err != nil {
		t.Fatalf("create anonymous: %v", err)
	}

	authCache := auth.NewCache(storage.Users, true, true)
	if err := authCache.Refresh(ctx); err != nil {
		t.Fatalf("refresh authCache: %v", err)
	}

	r := &Resolver{
		Cfg:       cfg,
		Storage:   storage,
		AuthCache: authCache,
	}
	mr := &mutationResolver{r}
	qr := &queryResolver{r}

	// 1. Broker.anonymousEnabled should be true
	broker := r.brokerObj()
	if !broker.AnonymousEnabled {
		t.Errorf("expected Broker.anonymousEnabled to be true")
	}

	// 2. Login as Anonymous without password succeeds and is not admin
	anonLogin, err := mr.Login(ctx, "Anonymous", "")
	if err != nil {
		t.Fatalf("mr.Login(Anonymous) error: %v", err)
	}
	if !anonLogin.Success || anonLogin.IsAdmin || anonLogin.Token == nil || *anonLogin.Username != "Anonymous" {
		t.Fatalf("unexpected anonLogin result: %+v", anonLogin)
	}

	// 3. Login as Admin succeeds and is admin
	adminLogin, err := mr.Login(ctx, "Admin", "Admin")
	if err != nil {
		t.Fatalf("mr.Login(Admin) error: %v", err)
	}
	if !adminLogin.Success || !adminLogin.IsAdmin || adminLogin.Token == nil || *adminLogin.Username != "Admin" {
		t.Fatalf("unexpected adminLogin result: %+v", adminLogin)
	}

	// 4. Anonymous cannot query Users (requires admin)
	anonCtx := auth.WithPrincipal(ctx, stores.User{Username: "Anonymous", IsAdmin: false})
	if _, err := qr.Users(anonCtx, nil); err == nil {
		t.Errorf("expected Users query to fail for Anonymous user")
	}

	// 5. Admin can query Users
	adminCtx := auth.WithPrincipal(ctx, stores.User{Username: "Admin", IsAdmin: true})
	users, err := qr.Users(adminCtx, nil)
	if err != nil {
		t.Fatalf("expected Users query to succeed for Admin: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	// 6. Disable Anonymous user: anonymousEnabled becomes false, login fails
	anonUser, err := storage.Users.GetUser(ctx, "Anonymous")
	if err != nil {
		t.Fatalf("get anonymous: %v", err)
	}
	anonUser.Enabled = false
	if err := storage.Users.UpdateUser(ctx, *anonUser); err != nil {
		t.Fatalf("update anonymous: %v", err)
	}
	if err := authCache.Refresh(ctx); err != nil {
		t.Fatalf("refresh auth: %v", err)
	}

	if r.isAnonymousEnabled() {
		t.Errorf("expected isAnonymousEnabled to be false after disabling Anonymous user")
	}
	anonLoginDisabled, err := mr.Login(ctx, "Anonymous", "")
	if err != nil {
		t.Fatalf("mr.Login error: %v", err)
	}
	if anonLoginDisabled.Success {
		t.Errorf("expected login to fail for disabled Anonymous user")
	}
}

func TestDeviceToGenericGraphQLVirtualTopicTreeStripping(t *testing.T) {
	d := stores.DeviceConfig{
		Name:      "test-tree",
		Namespace: "enterprise",
		NodeID:    "*",
		Type:      "VirtualTopicTree",
		Config:    `{"schemaVersion":1,"id":"test-tree","name":"Test","rootPath":"enterprise","revision":"secret-rev-123","nodes":[]}`,
	}
	dev := deviceToGenericGraphQL(d)
	cfg := dev.Config
	if rev, exists := cfg["revision"]; exists && rev != "" {
		t.Errorf("expected revision to be stripped from exported virtual topic tree, got %v", rev)
	}
}

// The synthetic localhost administrator (AllowAnonymousLocalhost) is not in the
// user store: topic checks must allow it, but not a principal that is merely
// named "localhost", and not when the option is off.
func TestAllowTopicLocalhostPrincipal(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.UserManagement.Enabled = true
	cfg.UserManagement.AnonymousEnabled = false
	cfg.UserManagement.AllowAnonymousLocalhost = true
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "monstermq.db")

	storage, db, err := storesqlite.Build(ctx, cfg)
	if err != nil {
		t.Fatalf("build storage: %v", err)
	}
	defer db.Close()
	authCache := auth.NewCache(storage.Users, false, true)
	if err := authCache.Refresh(ctx); err != nil {
		t.Fatalf("refresh authCache: %v", err)
	}
	r := &Resolver{Cfg: cfg, Storage: storage, AuthCache: authCache}

	local := auth.WithLocalhostPrincipal(ctx)
	if !r.allowTopic(local, "test/x", true) || !r.allowTopic(local, "test/#", false) {
		t.Fatalf("expected the localhost principal to publish and subscribe")
	}
	named := auth.WithPrincipal(ctx, auth.LocalhostUser)
	if r.allowTopic(named, "test/x", true) {
		t.Errorf("expected a principal only named localhost to be checked against the user store")
	}
	if r.allowTopic(ctx, "test/x", false) {
		t.Errorf("expected an unauthenticated caller to be refused")
	}
	cfg.UserManagement.AllowAnonymousLocalhost = false
	if r.allowTopic(local, "test/x", true) {
		t.Errorf("expected the localhost principal to be refused with AllowAnonymousLocalhost off")
	}
}
