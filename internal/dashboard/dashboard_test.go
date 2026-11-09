package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedHandler(t *testing.T) {
	h := Handler("", nil, nil)

	t.Run("serves placeholder or index", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/placeholder.html", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		if !strings.Contains(string(body), "MonsterMQ") {
			t.Fatalf("expected MonsterMQ in body, got: %s", string(body))
		}
	})

	t.Run("missing static asset returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/missing.js", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
}

func TestFilesystemHandler(t *testing.T) {
	tempDir := t.TempDir()
	indexContent := "<html><body>Custom Dashboard Index</body></html>"
	assetContent := "body { color: red; }"

	if err := os.WriteFile(filepath.Join(tempDir, "index.html"), []byte(indexContent), 0644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(tempDir, "css")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "style.css"), []byte(assetContent), 0644); err != nil {
		t.Fatal(err)
	}

	h := Handler(tempDir, nil, nil)

	t.Run("serves root index.html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		if string(body) != indexContent {
			t.Fatalf("unexpected content: %s", string(body))
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Fatalf("expected no-cache header, got %s", cc)
		}
	})

	t.Run("serves sub-asset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/css/style.css", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		if string(body) != assetContent {
			t.Fatalf("unexpected content: %s", string(body))
		}
	})

	t.Run("directory without index returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/css", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for directory listing, got %d", rec.Code)
		}
	})

	t.Run("spa route fallback to index.html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/routes/view-a", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for SPA fallback, got %d", rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		if string(body) != indexContent {
			t.Fatalf("unexpected content: %s", string(body))
		}
	})

	t.Run("api paths return 404", func(t *testing.T) {
		for _, p := range []string{"/api", "/api/v1/docs", "/graphql", "/hmi/screen", "/redfish/v1"} {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for %s, got %d", p, rec.Code)
			}
		}
	})
}

func TestBrandingJSON(t *testing.T) {
	get := func(h http.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BrandingPath, nil))
		return rec
	}

	t.Run("served from config", func(t *testing.T) {
		rec := get(Handler(t.TempDir(), &Branding{Brand: " NexusMQ ", ProductName: "WinCCMQ"}, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %q", ct)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"brand": "nexusmq", "productName": "WinCCMQ"}
		if len(got) != len(want) || got["brand"] != want["brand"] || got["productName"] != want["productName"] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	})

	t.Run("config wins over a bundled file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "config"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config", "branding.json"), []byte(`{"productName":"Bundled"}`), 0644); err != nil {
			t.Fatal(err)
		}
		if body := get(Handler(dir, &Branding{ProductName: "WinCCMQ"}, nil)).Body.String(); !strings.Contains(body, "WinCCMQ") {
			t.Fatalf("expected configured branding, got %s", body)
		}
		if body := get(Handler(dir, nil, nil)).Body.String(); !strings.Contains(body, "Bundled") {
			t.Fatalf("expected bundled branding without config, got %s", body)
		}
	})

	t.Run("not configured returns 404", func(t *testing.T) {
		if rec := get(Handler(t.TempDir(), nil, nil)); rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
}
