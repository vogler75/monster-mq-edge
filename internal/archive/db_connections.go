package archive

import (
	"net/url"
	"regexp"
	"strings"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

const DefaultDatabaseConnectionName = "Default"

var authorityCredentialsPattern = regexp.MustCompile(`(://[^:@/?#]+):([^@/?#]+)@`)

func SanitizeDatabaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" && u.User != nil {
		if username := u.User.Username(); username != "" {
			u.User = url.User(username)
		} else {
			u.User = nil
		}
		return u.String()
	}
	return authorityCredentialsPattern.ReplaceAllString(raw, "$1@")
}

func MongoURLWithCredentials(raw, username, password string) string {
	if username == "" && password == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	u.User = url.UserPassword(username, password)
	return u.String()
}

func BuiltInDatabaseConnections(cfg *config.Config) []stores.DatabaseConnectionConfig {
	out := []stores.DatabaseConnectionConfig{}
	if cfg.Postgres.URL != "" {
		out = append(out, stores.DatabaseConnectionConfig{
			Name:     DefaultDatabaseConnectionName,
			Type:     stores.DatabaseConnectionPostgres,
			URL:      cfg.Postgres.URL,
			Username: cfg.Postgres.User,
			Password: cfg.Postgres.Pass,
			ReadOnly: true,
		})
	}
	if cfg.MongoDB.URL != "" {
		out = append(out, stores.DatabaseConnectionConfig{
			Name:     DefaultDatabaseConnectionName,
			Type:     stores.DatabaseConnectionMongoDB,
			URL:      cfg.MongoDB.URL,
			Database: cfg.MongoDB.Database,
			ReadOnly: true,
		})
	}
	return out
}

func RequiredDatabaseConnectionTypes(lastVal stores.MessageStoreType, archiveType stores.MessageArchiveType) []stores.DatabaseConnectionType {
	seen := map[stores.DatabaseConnectionType]struct{}{}
	add := func(t stores.DatabaseConnectionType) {
		if t != "" {
			seen[t] = struct{}{}
		}
	}
	switch lastVal {
	case stores.MessageStorePostgres:
		add(stores.DatabaseConnectionPostgres)
	case stores.MessageStoreMongoDB:
		add(stores.DatabaseConnectionMongoDB)
	}
	switch archiveType {
	case stores.ArchivePostgres:
		add(stores.DatabaseConnectionPostgres)
	case stores.ArchiveMongoDB:
		add(stores.DatabaseConnectionMongoDB)
	}
	out := make([]stores.DatabaseConnectionType, 0, len(seen))
	for _, t := range []stores.DatabaseConnectionType{stores.DatabaseConnectionPostgres, stores.DatabaseConnectionMongoDB} {
		if _, ok := seen[t]; ok {
			out = append(out, t)
		}
	}
	return out
}

func IsDefaultDatabaseConnectionName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), DefaultDatabaseConnectionName)
}
