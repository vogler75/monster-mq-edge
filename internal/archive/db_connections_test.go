package archive

import (
	"testing"

	"monstermq.io/edge/internal/stores"
)

func TestMongoURLWithCredentialsReplacesUserinfo(t *testing.T) {
	got := MongoURLWithCredentials("mongodb://old@localhost:27017", "user", "pass")
	if got != "mongodb://user:pass@localhost:27017" {
		t.Fatalf("unexpected URL: %s", got)
	}
	got = MongoURLWithCredentials("mongodb://old:stale@localhost:27017/db", "new", "secret")
	if got != "mongodb://new:secret@localhost:27017/db" {
		t.Fatalf("unexpected replacement URL: %s", got)
	}
}

func TestSanitizeDatabaseURL(t *testing.T) {
	got := SanitizeDatabaseURL("postgres://user:secret@localhost/db")
	if got != "postgres://user@localhost/db" {
		t.Fatalf("unexpected sanitized URL: %s", got)
	}
}

func TestRequiredDatabaseConnectionTypes(t *testing.T) {
	got := RequiredDatabaseConnectionTypes(stores.MessageStorePostgres, stores.ArchivePostgres)
	if len(got) != 1 || got[0] != stores.DatabaseConnectionPostgres {
		t.Fatalf("postgres required types: %+v", got)
	}
	got = RequiredDatabaseConnectionTypes(stores.MessageStorePostgres, stores.ArchiveMongoDB)
	if len(got) != 2 || got[0] != stores.DatabaseConnectionPostgres || got[1] != stores.DatabaseConnectionMongoDB {
		t.Fatalf("mixed required types: %+v", got)
	}
	got = RequiredDatabaseConnectionTypes(stores.MessageStoreSQLite, stores.ArchiveNone)
	if len(got) != 0 {
		t.Fatalf("sqlite/none required types: %+v", got)
	}
}
