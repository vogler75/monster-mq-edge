package dashboard

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

//go:embed all:dist
var embeddedFS embed.FS

// BrandingPath is where the dashboard (js/branding.js) looks for its branding.
const BrandingPath = "/config/branding.json"

// Branding is the dashboard's /config/branding.json document. Field names
// follow js/branding.js in the dashboard repo.
type Branding struct {
	Brand         string `json:"brand,omitempty"`
	ProductName   string `json:"productName,omitempty"`
	DashboardName string `json:"dashboardName,omitempty"`
	Tagline       string `json:"tagline,omitempty"`
}

// Handler returns an http.Handler that serves the dashboard.
// If customPath is non-empty, files are served live from that filesystem path.
// Otherwise, files are served from the embedded bundle.
// A non-nil branding is served at BrandingPath, taking precedence over any
// branding.json file in the bundle; nil leaves the dashboard's own branding.
func Handler(customPath string, branding *Branding, logger *slog.Logger) http.Handler {
	var brandingJSON []byte
	if branding != nil {
		b := *branding
		b.Brand = strings.ToLower(strings.TrimSpace(b.Brand))
		brandingJSON, _ = json.Marshal(b)
		if logger != nil {
			logger.Info("dashboard branding enabled", "brand", b.Brand, "productName", b.ProductName)
		}
	}

	var fileSystem http.FileSystem
	var fsOpen func(name string) (fs.File, error)

	if customPath != "" {
		if logger != nil {
			logger.Info("dashboard serving from filesystem", "path", customPath)
		}
		fileSystem = http.Dir(customPath)
		fsOpen = func(name string) (fs.File, error) {
			return os.Open(path.Join(customPath, name))
		}
	} else {
		if logger != nil {
			logger.Info("dashboard serving from embedded bundle")
		}
		sub, err := fs.Sub(embeddedFS, "dist")
		if err != nil && logger != nil {
			logger.Error("dashboard sub fs error", "err", err)
		}
		fileSystem = http.FS(sub)
		fsOpen = func(name string) (fs.File, error) {
			if sub == nil {
				return nil, os.ErrNotExist
			}
			return sub.Open(strings.TrimPrefix(name, "/"))
		}
	}

	fileServer := http.FileServer(fileSystem)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanPath := path.Clean(r.URL.Path)
		if cleanPath == "" || cleanPath == "." {
			cleanPath = "/"
		}

		if brandingJSON != nil && cleanPath == BrandingPath {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(brandingJSON)
			return
		}

		if strings.HasPrefix(cleanPath, "/api/") || cleanPath == "/api" ||
			strings.HasPrefix(cleanPath, "/graphql") ||
			strings.HasPrefix(cleanPath, "/mcp/") || cleanPath == "/mcp" ||
			strings.HasPrefix(cleanPath, "/hmi/") || cleanPath == "/hmi" ||
			strings.HasPrefix(cleanPath, "/redfish/") || cleanPath == "/redfish" {
			http.NotFound(w, r)
			return
		}

		target := strings.TrimPrefix(cleanPath, "/")
		if target == "" {
			target = "index.html"
		}

		if customPath != "" {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		} else if target == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}

		f, err := fsOpen(target)
		if err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && stat.IsDir() {
				idx, idxErr := fsOpen(path.Join(target, "index.html"))
				if idxErr != nil {
					http.NotFound(w, r)
					return
				}
				_ = idx.Close()
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		if path.Ext(cleanPath) != "" {
			http.NotFound(w, r)
			return
		}

		idxFile, err := fsOpen("index.html")
		if err == nil {
			_ = idxFile.Close()
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}

		http.NotFound(w, r)
	})
}
