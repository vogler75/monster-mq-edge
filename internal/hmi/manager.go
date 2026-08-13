package hmi

import (
	"archive/zip"
	"bytes"
	"context"
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
	"monstermq.io/edge/internal/stores"
)

type HmiConfig struct {
	UrlPath     string `json:"urlPath"`
	IsMain      bool   `json:"isMain"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	EntryPoint  string `json:"entryPoint,omitempty"`
}

type HmiDevice struct {
	Name            string    `json:"name"`
	NodeID          string    `json:"nodeId"`
	Enabled         bool      `json:"enabled"`
	Config          HmiConfig `json:"config"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	IsOnCurrentNode bool      `json:"isOnCurrentNode"`
	FileCount       int       `json:"fileCount"`
	SizeBytes       int64     `json:"sizeBytes"`
}

type DashboardFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

type Metadata struct {
	MainDashboard string `json:"mainDashboard"`
}

type Manager struct {
	mu          sync.RWMutex
	baseDir     string
	nodeID      string
	deviceStore stores.DeviceConfigStore
}

func NewManager(cfg *config.Config, deviceStore stores.DeviceConfigStore) *Manager {
	dir := cfg.HMI.Path
	if dir == "" {
		dir = "./data/hmi"
	}
	m := &Manager{
		baseDir:     dir,
		nodeID:      cfg.NodeID,
		deviceStore: deviceStore,
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
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>MonsterMQ HMI Dashboard</title>
    <style>
        * { box-sizing: border-box; }
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; padding: 2rem; background: #0f172a; color: #f8fafc; margin: 0; }
        .card { background: #1e293b; padding: 1.5rem; border-radius: 8px; max-width: 640px; margin: 0 auto; box-shadow: 0 4px 6px rgba(0,0,0,0.3); border: 1px solid #334155; }
        h1 { margin-top: 0; color: #38bdf8; font-size: 1.5rem; }
        p { color: #94a3b8; font-size: 0.95rem; line-height: 1.5; }
        pre { background: #090d16; padding: 1rem; border-radius: 6px; overflow-x: auto; color: #a7f3d0; border: 1px solid #1e293b; font-size: 0.875rem; }
        button { background: #0284c7; color: white; border: none; padding: 0.6rem 1.2rem; border-radius: 6px; cursor: pointer; font-size: 0.95rem; font-weight: 5rem; transition: background 0.2s; }
        button:hover { background: #0369a1; }
    </style>
</head>
<body>
    <div class="card">
        <h1>MonsterMQ HMI Dashboard</h1>
        <p>This is the default HMI application served directly by MonsterMQ.</p>
        <button onclick="checkStatus()">Check Broker Status</button>
        <pre id="output">Click button to test GraphQL connection...</pre>
    </div>
    <script>
        async function checkStatus() {
            try {
                const res = await fetch('/graphql', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ query: '{ broker { nodeId version userManagementEnabled isLeader isCurrent enabledFeatures } }' })
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

	// Ensure DB entry for 'main' device if deviceStore is available
	if m.deviceStore != nil {
		ctx := context.Background()
		dc, _ := m.deviceStore.Get(ctx, "main")
		if dc == nil {
			cfgJSON, _ := json.Marshal(HmiConfig{
				UrlPath:    "",
				IsMain:     true,
				Title:      "Main Dashboard",
				EntryPoint: "index.html",
			})
			_ = m.deviceStore.Save(ctx, stores.DeviceConfig{
				Name:      "main",
				Namespace: "main",
				NodeID:    "local",
				Type:      "HMI",
				Enabled:   true,
				Config:    string(cfgJSON),
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			})
		} else if dc.Type == "HMI" && (dc.NodeID != "local" || dc.Namespace == "") {
			dc.NodeID = "local"
			if dc.Namespace == "" {
				dc.Namespace = dc.Name
			}
			dc.UpdatedAt = time.Now()
			_ = m.deviceStore.Save(ctx, *dc)
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

func (m *Manager) IsHmiEnabled(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.deviceStore != nil {
		ctx := context.Background()
		dc, err := m.deviceStore.Get(ctx, name)
		if err == nil && dc != nil && dc.Type == "HMI" {
			return dc.Enabled
		}
	}
	// Default to enabled if directory exists
	dashDir := filepath.Join(m.baseDir, name)
	info, err := os.Stat(dashDir)
	return err == nil && info.IsDir()
}

func (m *Manager) ListHmis() ([]*HmiDevice, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta := m.getMetadataLocked()

	// Fetch from deviceStore if available
	var dbConfigs map[string]stores.DeviceConfig
	if m.deviceStore != nil {
		ctx := context.Background()
		all, err := m.deviceStore.GetAll(ctx)
		if err == nil {
			dbConfigs = make(map[string]stores.DeviceConfig)
			for _, dc := range all {
				if dc.Type == "HMI" {
					dbConfigs[dc.Name] = dc
				}
			}
		}
	}

	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return nil, err
	}

	var hmis []*HmiDevice
	seen := make(map[string]bool)

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		name := entry.Name()
		seen[name] = true
		hmi, err := m.getHmiStatsLocked(name, meta.MainDashboard, dbConfigs[name])
		if err == nil {
			hmis = append(hmis, hmi)
		}
	}

	// Also list deviceStore entries that may not have directory created yet
	for name, dc := range dbConfigs {
		if !seen[name] {
			hmi, err := m.getHmiStatsLocked(name, meta.MainDashboard, dc)
			if err == nil {
				hmis = append(hmis, hmi)
			}
		}
	}

	return hmis, nil
}

func (m *Manager) GetHmi(name string) (*HmiDevice, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta := m.getMetadataLocked()

	var dc stores.DeviceConfig
	if m.deviceStore != nil {
		ctx := context.Background()
		if found, err := m.deviceStore.Get(ctx, name); err == nil && found != nil {
			dc = *found
		}
	}

	return m.getHmiStatsLocked(name, meta.MainDashboard, dc)
}

func (m *Manager) getHmiStatsLocked(name, mainDashName string, dc stores.DeviceConfig) (*HmiDevice, error) {
	dashDir := filepath.Join(m.baseDir, name)
	info, err := os.Stat(dashDir)
	var fileCount int
	var totalSize int64
	var latestMod time.Time

	if err == nil && info.IsDir() {
		latestMod = info.ModTime()
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
	}

	isMain := name == mainDashName
	urlPath := name
	if isMain {
		urlPath = ""
	}

	cfg := HmiConfig{
		UrlPath: urlPath,
		IsMain:  isMain,
	}

	nodeID := m.nodeID
	enabled := true
	createdAt := latestMod
	updatedAt := latestMod

	if dc.Name != "" {
		nodeID = dc.NodeID
		enabled = dc.Enabled
		if !dc.CreatedAt.IsZero() {
			createdAt = dc.CreatedAt
		}
		if !dc.UpdatedAt.IsZero() {
			updatedAt = dc.UpdatedAt
		}
		if dc.Config != "" {
			_ = json.Unmarshal([]byte(dc.Config), &cfg)
		}
	}

	return &HmiDevice{
		Name:            name,
		NodeID:          nodeID,
		Enabled:         enabled,
		Config:          cfg,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		IsOnCurrentNode: nodeID == m.nodeID || nodeID == "local" || nodeID == "*",
		FileCount:       fileCount,
		SizeBytes:       totalSize,
	}, nil
}

func (m *Manager) SaveHmiDevice(name string, nodeID string, enabled *bool, cfg HmiConfig) (*HmiDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("invalid HMI name %q", name)
	}

	dashDir := filepath.Join(m.baseDir, name)
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		return nil, err
	}

	indexPath := filepath.Join(dashDir, "index.html")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		defaultHTML := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>%s</title>
    <style>body { font-family: sans-serif; padding: 2rem; background: #0f172a; color: #f8fafc; }</style>
</head>
<body>
    <h1>Dashboard: %s</h1>
    <p>Created on %s</p>
</body>
</html>`, name, name, time.Now().Format(time.RFC3339))
		_ = os.WriteFile(indexPath, []byte(defaultHTML), 0644)
	}

	if nodeID == "" {
		nodeID = m.nodeID
	}

	isEnabled := true
	if enabled != nil {
		isEnabled = *enabled
	}

	meta := m.getMetadataLocked()
	if cfg.IsMain {
		meta.MainDashboard = name
		_ = m.saveMetadataLocked(meta)
	}

	if m.deviceStore != nil {
		ctx := context.Background()
		cfgJSON, _ := json.Marshal(cfg)
		if nodeID == "" {
			nodeID = "local"
		}
		dc := stores.DeviceConfig{
			Name:      name,
			Namespace: name,
			NodeID:    nodeID,
			Type:      "HMI",
			Enabled:   isEnabled,
			Config:    string(cfgJSON),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		_ = m.deviceStore.Save(ctx, dc)
	}

	return m.getHmiStatsLocked(name, meta.MainDashboard, stores.DeviceConfig{})
}

func (m *Manager) DeleteHmiDevice(name string) error {
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

	if m.deviceStore != nil {
		ctx := context.Background()
		_ = m.deviceStore.Delete(ctx, name)
	}

	dashDir := filepath.Join(m.baseDir, name)
	return os.RemoveAll(dashDir)
}

func (m *Manager) ToggleHmiDevice(name string, enabled bool) (*HmiDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta := m.getMetadataLocked()

	if m.deviceStore != nil {
		ctx := context.Background()
		dc, err := m.deviceStore.Toggle(ctx, name, enabled)
		if err == nil && dc != nil {
			return m.getHmiStatsLocked(name, meta.MainDashboard, *dc)
		}
	}

	return m.getHmiStatsLocked(name, meta.MainDashboard, stores.DeviceConfig{Name: name, Enabled: enabled})
}

func (m *Manager) ReassignHmiDevice(name string, nodeID string) (*HmiDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta := m.getMetadataLocked()

	if m.deviceStore != nil {
		ctx := context.Background()
		dc, err := m.deviceStore.Reassign(ctx, name, nodeID)
		if err == nil && dc != nil {
			return m.getHmiStatsLocked(name, meta.MainDashboard, *dc)
		}
	}

	return m.getHmiStatsLocked(name, meta.MainDashboard, stores.DeviceConfig{Name: name, NodeID: nodeID})
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

func (m *Manager) UploadDashboardZip(name string, zipBase64 string, setAsMain bool) (*HmiDevice, error) {
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

	return m.getHmiStatsLocked(name, meta.MainDashboard, stores.DeviceConfig{})
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
