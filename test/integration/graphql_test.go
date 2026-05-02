package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

func startWithGraphQL(t *testing.T, mqttPort, gqlPort int, cfgFns ...func(*config.Config)) (*broker.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("g-%d", gqlPort)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "g.db")
	for _, fn := range cfgFns {
		fn(cfg)
	}
	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	go func() { _ = srv.Serve() }()
	// wait for graphql to start
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", gqlPort))
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return srv, fmt.Sprintf("http://localhost:%d/graphql", gqlPort)
}

func gqlQuery(t *testing.T, url, query string, vars map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if errs, ok := result["errors"]; ok {
		t.Fatalf("graphql errors: %v\nresponse=%s", errs, out)
	}
	return result["data"].(map[string]any)
}

func TestGraphQLBrokerConfig(t *testing.T) {
	srv, url := startWithGraphQL(t, 23001, 28001)
	defer srv.Close()

	data := gqlQuery(t, url, `{ brokerConfig { nodeId tcpPort sqlitePath sessionStoreType } }`, nil)
	cfg := data["brokerConfig"].(map[string]any)
	if cfg["nodeId"] != "g-28001" {
		t.Fatalf("nodeId %v", cfg["nodeId"])
	}
	if int(cfg["tcpPort"].(float64)) != 23001 {
		t.Fatalf("tcpPort %v", cfg["tcpPort"])
	}
	if cfg["sessionStoreType"] != "SQLITE" {
		t.Fatalf("sessionStoreType %v", cfg["sessionStoreType"])
	}
}

func TestGraphQLPublishAndCurrentValue(t *testing.T) {
	srv, url := startWithGraphQL(t, 23002, 28002)
	defer srv.Close()

	// Publish via GraphQL.
	gqlQuery(t, url, `mutation { publish(input: { topic: "g/temp", payload: "23.5", qos: 0, retain: true }) { success topic } }`, nil)

	// Wait for archive group flush.
	time.Sleep(500 * time.Millisecond)

	data := gqlQuery(t, url, `{ currentValue(topic: "g/temp") { topic payload qos } }`, nil)
	cv := data["currentValue"].(map[string]any)
	if cv["topic"] != "g/temp" {
		t.Fatalf("topic %v", cv["topic"])
	}
	if !strings.Contains(fmt.Sprintf("%v", cv["payload"]), "23.5") {
		t.Fatalf("payload %v", cv["payload"])
	}

	// Retained message should also be queryable.
	data = gqlQuery(t, url, `{ retainedMessage(topic: "g/temp") { topic payload } }`, nil)
	rm := data["retainedMessage"].(map[string]any)
	if !strings.Contains(fmt.Sprintf("%v", rm["payload"]), "23.5") {
		t.Fatalf("retained payload %v", rm["payload"])
	}
}

func TestGraphQLSessionsLifecycle(t *testing.T) {
	srv, url := startWithGraphQL(t, 23003, 28003)
	defer srv.Close()

	// Connect an MQTT client so a session row appears.
	cl := mqtt.NewClient(mqttOpts(23003, "gql-sess"))
	if tok := cl.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cl.Disconnect(100)

	if tok := cl.Subscribe("g/+/x", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	time.Sleep(200 * time.Millisecond)

	data := gqlQuery(t, url, `{ sessions { clientId connected subscriptions { topicFilter qos } } }`, nil)
	sessions := data["sessions"].([]any)
	found := false
	for _, s := range sessions {
		m := s.(map[string]any)
		if m["clientId"] == "gql-sess" {
			found = true
			subs := m["subscriptions"].([]any)
			if len(subs) != 1 || subs[0].(map[string]any)["topicFilter"] != "g/+/x" {
				t.Fatalf("unexpected subscriptions: %v", subs)
			}
		}
	}
	if !found {
		t.Fatalf("session 'gql-sess' missing in %v", sessions)
	}
}

func TestGraphQLArchiveGroupCRUD(t *testing.T) {
	srv, url := startWithGraphQL(t, 23004, 28004)
	defer srv.Close()

	// Default group should exist.
	data := gqlQuery(t, url, `{ archiveGroups { name enabled lastValType archiveType } }`, nil)
	groups := data["archiveGroups"].([]any)
	if len(groups) < 1 {
		t.Fatal("expected default group")
	}
	hasDefault := false
	for _, g := range groups {
		group := g.(map[string]any)
		if group["name"] == "Default" {
			hasDefault = true
			if group["lastValType"] != "MEMORY" || group["archiveType"] != "NONE" {
				t.Fatalf("unexpected Default group storage: %v", group)
			}
		}
	}
	if !hasDefault {
		t.Fatal("Default group missing")
	}

	// Create a new group.
	gqlQuery(t, url, `mutation Create($input: CreateArchiveGroupInput!) {
        archiveGroup { create(input: $input) { success archiveGroup { name enabled } } }
    }`, map[string]any{
		"input": map[string]any{
			"name":        "Sensors",
			"topicFilter": []string{"sensor/#"},
			"lastValType": "SQLITE",
			"archiveType": "SQLITE",
		},
	})

	// Read it back.
	data = gqlQuery(t, url, `{ archiveGroup(name: "Sensors") { name topicFilter } }`, nil)
	if data["archiveGroup"].(map[string]any)["name"] != "Sensors" {
		t.Fatalf("created group not found: %v", data)
	}
}

func TestGraphQLDatabaseConnectionCRUDAndValidation(t *testing.T) {
	srv, url := startWithGraphQL(t, 23021, 28021, func(c *config.Config) {
		c.Postgres.URL = "postgres://default:secret@localhost/defaultdb"
		c.MongoDB.URL = "mongodb://mongo-default:secret@localhost:27017"
		c.MongoDB.Database = "monstermq"
	})
	defer srv.Close()

	data := gqlQuery(t, url, `{ databaseConnectionNames(type: POSTGRES) }`, nil)
	names := data["databaseConnectionNames"].([]any)
	if len(names) != 1 || names[0] != "Default" {
		t.Fatalf("postgres names: %v", names)
	}
	data = gqlQuery(t, url, `{ databaseConnections(type: POSTGRES) { name type url readOnly } }`, nil)
	conns := data["databaseConnections"].([]any)
	def := conns[0].(map[string]any)
	if def["name"] != "Default" || def["readOnly"] != true || strings.Contains(def["url"].(string), "secret") {
		t.Fatalf("unexpected default connection: %v", def)
	}

	data = gqlQuery(t, url, `mutation {
        archiveGroup {
            createDatabaseConnection(input: {
                name: "pgEdge",
                type: POSTGRES,
                url: "postgres://user:topsecret@127.0.0.1:1/db",
                username: "user",
                password: "stored-secret",
                schema: "public"
            }) { success connection { name type url username schema readOnly } message }
        }
    }`, nil)
	create := data["archiveGroup"].(map[string]any)["createDatabaseConnection"].(map[string]any)
	if create["success"] != true {
		t.Fatalf("create connection failed: %v", create)
	}
	conn := create["connection"].(map[string]any)
	if strings.Contains(conn["url"].(string), "topsecret") || conn["readOnly"] != false {
		t.Fatalf("connection not sanitized: %v", conn)
	}

	data = gqlQuery(t, url, `mutation {
        archiveGroup {
            updateDatabaseConnection(input: { name: "pgEdge", url: "postgres://user:newsecret@127.0.0.1:1/db2" }) {
                success connection { name url }
            }
        }
    }`, nil)
	update := data["archiveGroup"].(map[string]any)["updateDatabaseConnection"].(map[string]any)
	if update["success"] != true || strings.Contains(update["connection"].(map[string]any)["url"].(string), "newsecret") {
		t.Fatalf("update connection failed/sanitization failed: %v", update)
	}
	stored, err := srv.Storage().ArchiveConfig.GetDatabaseConnection(context.Background(), "pgEdge")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Password != "stored-secret" {
		t.Fatalf("password was not preserved on update: %+v", stored)
	}

	data = gqlQuery(t, url, `mutation {
        archiveGroup { createDatabaseConnection(input: { name: "Default", type: POSTGRES, url: "postgres://x" }) { success message } }
    }`, nil)
	if data["archiveGroup"].(map[string]any)["createDatabaseConnection"].(map[string]any)["success"] != false {
		t.Fatal("Default connection should be read-only")
	}

	if err := srv.Storage().ArchiveConfig.Save(context.Background(), stores.ArchiveGroupConfig{
		Name:                   "PgNamed",
		Enabled:                false,
		TopicFilters:           []string{"pg/#"},
		LastValType:            stores.MessageStorePostgres,
		ArchiveType:            stores.ArchiveNone,
		DatabaseConnectionName: "pgEdge",
		PayloadFormat:          stores.PayloadDefault,
	}); err != nil {
		t.Fatal(err)
	}

	data = gqlQuery(t, url, `mutation { archiveGroup { deleteDatabaseConnection(name: "pgEdge") { success message } } }`, nil)
	del := data["archiveGroup"].(map[string]any)["deleteDatabaseConnection"].(map[string]any)
	if del["success"] != false {
		t.Fatalf("delete in-use connection should fail: %v", del)
	}

	data = gqlQuery(t, url, `mutation { archiveGroup { enable(name: "PgNamed") { success message } } }`, nil)
	enable := data["archiveGroup"].(map[string]any)["enable"].(map[string]any)
	if enable["success"] != false {
		t.Fatalf("enable with undeployable named connection should report failure: %v", enable)
	}

	data = gqlQuery(t, url, `mutation Create($input: CreateArchiveGroupInput!) {
        archiveGroup { create(input: $input) { success message } }
    }`, map[string]any{"input": map[string]any{
		"name":                   "WrongType",
		"topicFilter":            []string{"bad/#"},
		"lastValType":            "MONGODB",
		"archiveType":            "NONE",
		"databaseConnectionName": "pgEdge",
	}})
	wrong := data["archiveGroup"].(map[string]any)["create"].(map[string]any)
	if wrong["success"] != false {
		t.Fatalf("wrong connection type should fail: %v", wrong)
	}
}

func TestGraphQLUserManagement(t *testing.T) {
	srv, url := startWithGraphQL(t, 23005, 28005, func(c *config.Config) {
		// Login validation only kicks in with user management on; with it off,
		// we deliberately accept any credentials so the dashboard works.
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
	})
	defer srv.Close()

	// Create
	gqlQuery(t, url, `mutation { user { createUser(input: { username: "bob", password: "pw", isAdmin: true }) { success user { username isAdmin } } } }`, nil)

	// Login
	data := gqlQuery(t, url, `mutation { login(username: "bob", password: "pw") { success username isAdmin token } }`, nil)
	login := data["login"].(map[string]any)
	if !login["success"].(bool) {
		t.Fatalf("login failed: %v", login)
	}
	if login["username"] != "bob" {
		t.Fatalf("login username %v", login["username"])
	}

	// Wrong password
	data = gqlQuery(t, url, `mutation { login(username: "bob", password: "wrong") { success message } }`, nil)
	if data["login"].(map[string]any)["success"].(bool) {
		t.Fatal("wrong password should fail")
	}

	// List users
	data = gqlQuery(t, url, `{ users { username isAdmin } }`, nil)
	users := data["users"].([]any)
	seen := map[string]bool{}
	for _, raw := range users {
		seen[raw.(map[string]any)["username"].(string)] = true
	}
	if !seen["Admin"] || !seen["bob"] {
		t.Fatalf("users %v", users)
	}
}
