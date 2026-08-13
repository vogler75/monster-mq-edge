package hmi

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/config"
)

type DashboardApp struct {
	Name      string    `json:"name"`
	IsMain    bool      `json:"isMain"`
	Path      string    `json:"path"`
	FileCount int       `json:"fileCount"`
	SizeBytes int64     `json:"sizeBytes"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type DashboardFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

type Metadata struct {
	MainDashboard string `json:"mainDashboard"`
}

type Manager struct {
	mu      sync.RWMutex
	baseDir string
}

func NewManager(cfg *config.Config) *Manager {
	dir := cfg.HMI.Path
	if dir == "" {
		dir = "./data/hmi"
	}
	m := &Manager{
		baseDir: dir,
	}
	_ = m.EnsureInit()
	return m
}

func (m *Manager) EnsureInit() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.baseDir, 0755); err != nil {
		return err
	}

	metaPath := filepath.Join(m.baseDir, "metadata.json")
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		meta := Metadata{MainDashboard: "main"}
		data, _ := json.MarshalIndent(meta, "", "  ")
		_ = os.WriteFile(metaPath, data, 0644)
	}

	mainDir := filepath.Join(m.baseDir, "main")
	if _, err := os.Stat(mainDir); os.IsNotExist(err) {
		_ = os.MkdirAll(mainDir, 0755)
		indexPath := filepath.Join(mainDir, "index.html")
		if _, err := os.Stat(indexPath); os.IsNotExist(err) {
			defaultHTML := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Main Dashboard - MonsterMQ</title>
    <style>
        body { font-family: sans-serif; padding: 2rem; background: #0f172a; color: #f8fafc; }
        .card { background: #1e293b; padding: 1.5rem; border-radius: 8px; max-width: 600px; margin: 0 auto; box-shadow: 0 4px 6px rgba(0,0,0,0.3); }
        h1 { margin-top: 0; color: #38bdf8; }
        pre { background: #090d16; padding: 1rem; border-radius: 4px; overflow-x: auto; color: #a7f3d0; }
        button { background: #0284c7; color: white; border: none; padding: 0.5rem 1rem; border-radius: 4px; cursor: pointer; font-size: 1rem; }
        button:hover { background: #0369a1; }
    </style>
</head>
<body>
    <div class="card">
        <h1>MonsterMQ Edge Main Dashboard</h1>
        <p>This is the default HMI application hosted by the broker.</p>
        <button onclick="checkStatus()">Check Broker Status</button>
        <pre id="output">Click button to test GraphQL connection...</pre>
    </div>
    <script>
        async function checkStatus() {
            try {
                const res = await fetch('/graphql', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ query: '{ brokerConfig { nodeId version } }' })
                });
                const data = await res.json();
                document.getElementById('output').textContent = JSON.stringify(data, null, 2);
            } catch (err) {
                document.getElementById('output').textContent = 'Error: ' + err.message;
            }
        }
    </script>
</body>
</html>`
			_ = os.WriteFile(indexPath, []byte(defaultHTML), 0644)
		}
	}

	return nil
}

func (m *Manager) getMetadataLocked() Metadata {
	metaPath := filepath.Join(m.baseDir, "metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return Metadata{MainDashboard: "main"}
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil || meta.MainDashboard == "" {
		return Metadata{MainDashboard: "main"}
	}
	return meta
}

func (m *Manager) saveMetadataLocked(meta Metadata) error {
	metaPath := filepath.Join(m.baseDir, "metadata.json")
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0644)
}

func (m *Manager) GetMainDashboardName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getMetadataLocked().MainDashboard
}

func (m *Manager) SetMainDashboard(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dashDir := filepath.Join(m.baseDir, name)
	info, err := os.Stat(dashDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("dashboard %q does not exist", name)
	}

	meta := m.getMetadataLocked()
	meta.MainDashboard = name
	return m.saveMetadataLocked(meta)
}

func (m *Manager) ListDashboards() ([]*DashboardApp, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta := m.getMetadataLocked()

	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return nil, err
	}

	var dashboards []*DashboardApp
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		name := entry.Name()
		dash, err := m.getDashboardStatsLocked(name, meta.MainDashboard)
		if err == nil {
			dashboards = append(dashboards, dash)
		}
	}

	return dashboards, nil
}

func (m *Manager) GetDashboard(name string) (*DashboardApp, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta := m.getMetadataLocked()
	return m.getDashboardStatsLocked(name, meta.MainDashboard)
}

func (m *Manager) getDashboardStatsLocked(name, mainDashName string) (*DashboardApp, error) {
	dashDir := filepath.Join(m.baseDir, name)
	info, err := os.Stat(dashDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("dashboard %q not found", name)
	}

	var fileCount int
	var totalSize int64
	var latestMod time.Time = info.ModTime()

	_ = filepath.Walk(dashDir, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !f.IsDir() {
			fileCount++
			totalSize += f.Size()
			if f.ModTime().After(latestMod) {
				latestMod = f.ModTime()
			}
		}
		return nil
	})

	return &DashboardApp{
		Name:      name,
		IsMain:    name == mainDashName,
		Path:      "/hmi/" + name,
		FileCount: fileCount,
		SizeBytes: totalSize,
		UpdatedAt: latestMod,
	}, nil
}

func (m *Manager) CreateDashboard(name string, setAsMain bool) (*DashboardApp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("invalid dashboard name %q", name)
	}

	dashDir := filepath.Join(m.baseDir, name)
	if _, err := os.Stat(dashDir); err == nil {
		return nil, fmt.Errorf("dashboard %q already exists", name)
	}

	if err := os.MkdirAll(dashDir, 0755); err != nil {
		return nil, err
	}

	indexPath := filepath.Join(dashDir, "index.html")
	defaultHTML := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>%s Dashboard</title>
    <style>body { font-family: sans-serif; padding: 2rem; background: #0f172a; color: #f8fafc; }</style>
</head>
<body>
    <h1>Dashboard: %s</h1>
    <p>Created on %s</p>
</body>
</html>`, name, name, time.Now().Format(time.RFC3339))
	_ = os.WriteFile(indexPath, []byte(defaultHTML), 0644)

	meta := m.getMetadataLocked()
	if setAsMain {
		meta.MainDashboard = name
		_ = m.saveMetadataLocked(meta)
	}

	return m.getDashboardStatsLocked(name, meta.MainDashboard)
}

func (m *Manager) DeleteDashboard(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if name == "main" {
		return fmt.Errorf("cannot delete default 'main' dashboard")
	}

	meta := m.getMetadataLocked()
	if meta.MainDashboard == name {
		meta.MainDashboard = "main"
		_ = m.saveMetadataLocked(meta)
	}

	dashDir := filepath.Join(m.baseDir, name)
	return os.RemoveAll(dashDir)
}

func (m *Manager) resolveDashboardPathLocked(dashName, relPath string) (string, error) {
	basePath, err := filepath.Abs(filepath.Join(m.baseDir, dashName))
	if err != nil {
		return "", err
	}

	targetPath, err := filepath.Abs(filepath.Join(basePath, relPath))
	if err != nil {
		return "", err
	}

	if !strings.HasPrefix(targetPath, basePath) {
		return "", fmt.Errorf("invalid path: access outside of dashboard directory forbidden")
	}

	return targetPath, nil
}

func (m *Manager) ListDashboardFiles(dashName string) ([]DashboardFile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	dashDir := filepath.Join(m.baseDir, dashName)
	if _, err := os.Stat(dashDir); err != nil {
		return nil, fmt.Errorf("dashboard %q not found", dashName)
	}

	var files []DashboardFile
	err := filepath.Walk(dashDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dashDir, path)
		files = append(files, DashboardFile{
			Path:      rel,
			SizeBytes: info.Size(),
		})
		return nil
	})

	return files, err
}

func (m *Manager) ReadDashboardFile(dashName, relPath string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	targetPath, err := m.resolveDashboardPathLocked(dashName, relPath)
	if err != nil {
		return nil, err
	}

	return os.ReadFile(targetPath)
}

func (m *Manager) WriteDashboardFile(dashName, relPath string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	targetPath, err := m.resolveDashboardPathLocked(dashName, relPath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(targetPath, content, 0644)
}

func (m *Manager) DeleteDashboardFile(dashName, relPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	targetPath, err := m.resolveDashboardPathLocked(dashName, relPath)
	if err != nil {
		return err
	}

	return os.Remove(targetPath)
}

func (m *Manager) UploadDashboardZip(name string, zipBase64 string, setAsMain bool) (*DashboardApp, error) {
	zipData, err := base64.StdEncoding.DecodeString(zipBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 encoding: %w", err)
	}

	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip archive: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("invalid dashboard name %q", name)
	}

	dashDir := filepath.Join(m.baseDir, name)
	_ = os.RemoveAll(dashDir)
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		return nil, err
	}

	for _, f := range r.File {
		targetPath, err := m.resolveDashboardPathLocked(name, f.Name)
		if err != nil {
			continue
		}

		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			continue
		}

		outFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			continue
		}

		_, _ = io.Copy(outFile, rc)
		rc.Close()
		outFile.Close()
	}

	meta := m.getMetadataLocked()
	if setAsMain {
		meta.MainDashboard = name
		_ = m.saveMetadataLocked(meta)
	}

	return m.getDashboardStatsLocked(name, meta.MainDashboard)
}

func (m *Manager) ExportDashboardZip(name string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	dashDir := filepath.Join(m.baseDir, name)
	if _, err := os.Stat(dashDir); err != nil {
		return "", fmt.Errorf("dashboard %q not found", name)
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	err := filepath.Walk(dashDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(dashDir, path)
		if err != nil {
			return nil
		}

		f, err := w.Create(rel)
		if err != nil {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		_, err = f.Write(data)
		return err
	})

	if err != nil {
		return "", err
	}

	if err := w.Close(); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
