package questdb

import (
	"testing"
)

func TestNormalizeQWPConf(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		user     string
		pass     string
		expected string
	}{
		{
			name:     "already qwp formatted",
			rawURL:   "ws::addr=localhost:9000;",
			expected: "ws::addr=localhost:9000;",
		},
		{
			name:     "plain host:port",
			rawURL:   "localhost:9000",
			expected: "ws::addr=localhost:9000;",
		},
		{
			name:     "http url",
			rawURL:   "http://qdb.local:9000",
			expected: "ws::addr=qdb.local:9000;",
		},
		{
			name:     "pgwire url mapped to 9000",
			rawURL:   "postgres://localhost:8812/qdb",
			expected: "ws::addr=localhost:9000;",
		},
		{
			name:     "with credentials",
			rawURL:   "localhost:9000",
			user:     "admin",
			pass:     "secret",
			expected: "ws::addr=localhost:9000;username=admin;password=secret;",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeQWPConf(tc.rawURL, tc.user, tc.pass)
			if got != tc.expected {
				t.Errorf("NormalizeQWPConf(%q, %q, %q) = %q; want %q", tc.rawURL, tc.user, tc.pass, got, tc.expected)
			}
		})
	}
}

func TestNormalizePGDSN(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		user     string
		pass     string
		expected string
	}{
		{
			name:     "qwp conf mapped to pgwire 8812",
			rawURL:   "ws::addr=localhost:9000;",
			expected: "postgres://admin:quest@localhost:8812/qdb?sslmode=disable",
		},
		{
			name:     "plain host:port mapped to 8812",
			rawURL:   "localhost:9000",
			expected: "postgres://admin:quest@localhost:8812/qdb?sslmode=disable",
		},
		{
			name:     "custom user and pass",
			rawURL:   "localhost:8812",
			user:     "myuser",
			pass:     "mypass",
			expected: "postgres://myuser:mypass@localhost:8812/qdb?sslmode=disable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizePGDSN(tc.rawURL, tc.user, tc.pass)
			if got != tc.expected {
				t.Errorf("NormalizePGDSN(%q, %q, %q) = %q; want %q", tc.rawURL, tc.user, tc.pass, got, tc.expected)
			}
		})
	}
}
