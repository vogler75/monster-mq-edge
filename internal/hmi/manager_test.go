package hmi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	tempDir := t.TempDir()
	hmiDir := filepath.Join(tempDir, "hmi_root")
	if err := os.MkdirAll(hmiDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.NodeID = "test-node"
	cfg.HMI.Enabled = true
	cfg.HMI.Path = hmiDir

	mgr := NewManager(cfg, nil)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
	return mgr, hmiDir
}

func TestValidateDashboardName(t *testing.T) {
	valid := []string{"main", "dash1", "My_Dashboard", "dashboard-v2", "D123"}
	for _, v := range valid {
		if err := ValidateDashboardName(v); err != nil {
			t.Errorf("expected %q to be valid, got: %v", v, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		"..",
		"../escape",
		"dash/board",
		"dash\\board",
		".hidden",
		"-leading-dash",
		"_leading_underscore",
		"name with spaces",
		"name@symbol",
		"name;injection",
	}
	for _, inv := range invalid {
		if err := ValidateDashboardName(inv); err == nil {
			t.Errorf("expected %q to be rejected, but it passed", inv)
		}
	}
}

func TestResolveDashboardPath_Containment(t *testing.T) {
	mgr, hmiDir := newTestManager(t)

	// Create test dashboard
	dashName := "plant1"
	dashDir := filepath.Join(hmiDir, dashName)
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Valid paths inside dashboard
	p, err := mgr.ResolveDashboardPath(dashName, "index.html")
	if err != nil {
		t.Fatalf("expected valid path, got err: %v", err)
	}
	if p != filepath.Join(dashDir, "index.html") {
		t.Fatalf("expected %q, got %q", filepath.Join(dashDir, "index.html"), p)
	}

	pSub, err := mgr.ResolveDashboardPath(dashName, "assets/app.js")
	if err != nil {
		t.Fatalf("expected valid subpath, got err: %v", err)
	}
	if pSub != filepath.Join(dashDir, "assets", "app.js") {
		t.Fatalf("expected %q, got %q", filepath.Join(dashDir, "assets", "app.js"), pSub)
	}

	// 2. Traversal attempts via relative path
	traversalPaths := []string{
		"../outside.txt",
		"../../outside.txt",
		"assets/../../outside.txt",
		"/../outside.txt",
		"..\\outside.txt",
	}
	for _, tp := range traversalPaths {
		_, err := mgr.ResolveDashboardPath(dashName, tp)
		if err == nil {
			t.Errorf("expected error for traversal path %q, but got nil", tp)
		}
	}

	// 3. Traversal attempts via dashboard name
	_, err = mgr.ResolveDashboardPath("../outside-fixture", "index.html")
	if err == nil {
		t.Errorf("expected error for traversal dashboard name, but got nil")
	}

	// 4. Prefix confusion: e.g. "plant1" vs "plant1-ext"
	siblingDir := filepath.Join(hmiDir, "plant1-ext")
	_ = os.MkdirAll(siblingDir, 0755)
	_ = os.WriteFile(filepath.Join(siblingDir, "secret.txt"), []byte("secret"), 0644)

	_, err = mgr.ResolveDashboardPath(dashName, "../plant1-ext/secret.txt")
	if err == nil {
		t.Errorf("expected prefix-confusion traversal to be rejected, but got nil")
	}
}

func TestResolveDashboardPath_Symlinks(t *testing.T) {
	mgr, hmiDir := newTestManager(t)

	// Create a secret file outside HMI root
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "passwd")
	if err := os.WriteFile(secretFile, []byte("secret_data"), 0644); err != nil {
		t.Fatal(err)
	}

	dashName := "symdash"
	dashDir := filepath.Join(hmiDir, dashName)
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create symlink inside dashboard pointing to outside file
	symlinkPath := filepath.Join(dashDir, "leak_symlink")
	if err := os.Symlink(secretFile, symlinkPath); err != nil {
		t.Skipf("skipping symlink test: symlink creation failed: %v", err)
	}

	// ResolveDashboardPath must reject accessing the symlink pointing outside
	_, err := mgr.ResolveDashboardPath(dashName, "leak_symlink")
	if err == nil {
		t.Errorf("expected error resolving symlink escaping dashboard, got nil")
	}

	// ReadDashboardFile must also reject
	_, err = mgr.ReadDashboardFile(dashName, "leak_symlink")
	if err == nil {
		t.Errorf("expected error reading symlink escaping dashboard, got nil")
	}
}

func TestExportDashboardZip_Security(t *testing.T) {
	mgr, hmiDir := newTestManager(t)

	// 1. Outside directory fixture next to HMI root
	siblingDir := filepath.Join(filepath.Dir(hmiDir), "outside-fixture")
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(siblingDir, "secret.txt"), []byte("confidential"), 0644)

	// Attempt export with directory traversal in name (Issue #14 reproduction)
	_, err := mgr.ExportDashboardZip("../outside-fixture")
	if err == nil {
		t.Fatal("expected error exporting ../outside-fixture, but got nil")
	}

	_, err = mgr.ExportDashboardZip("..")
	if err == nil {
		t.Fatal("expected error exporting .., but got nil")
	}

	// 2. Normal export
	dashName := "validpanel"
	dashDir := filepath.Join(hmiDir, dashName)
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dashDir, "index.html"), []byte("<h1>Panel</h1>"), 0644)

	// Also add an external symlink inside the dashboard
	_ = os.Symlink(filepath.Join(siblingDir, "secret.txt"), filepath.Join(dashDir, "leaked.txt"))

	zipB64, err := mgr.ExportDashboardZip(dashName)
	if err != nil {
		t.Fatalf("expected success exporting valid dashboard, got: %v", err)
	}

	// Decode zip and verify leaked.txt was NOT included
	zipBytes, err := base64.StdEncoding.DecodeString(zipB64)
	if err != nil {
		t.Fatal(err)
	}
	r, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range r.File {
		if f.Name == "leaked.txt" {
			t.Fatalf("security violation: export included symlink pointing outside dashboard: %q", f.Name)
		}
	}
}

func TestUploadDashboardZip_ZipSlip(t *testing.T) {
	mgr, hmiDir := newTestManager(t)

	dashName := "uploadtest"

	// Create zip containing a ZipSlip attack entry
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	// 1. Normal file
	f1, _ := w.Create("index.html")
	_, _ = f1.Write([]byte("<h1>Hello</h1>"))

	// 2. Malicious traversal entry
	f2, _ := w.Create("../evil.txt")
	_, _ = f2.Write([]byte("evil payload"))

	// 3. Absolute path entry
	f3, _ := w.Create("/root.txt")
	_, _ = f3.Write([]byte("root payload"))

	_ = w.Close()

	zipB64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	_, err := mgr.UploadDashboardZip(dashName, zipB64, false)
	if err != nil {
		t.Fatalf("UploadDashboardZip failed: %v", err)
	}

	// Verify evil.txt was NOT written outside the dashboard directory
	evilFile := filepath.Join(hmiDir, "evil.txt")
	if _, err := os.Stat(evilFile); err == nil {
		t.Fatalf("security violation: ZipSlip wrote file outside dashboard root: %s", evilFile)
	}

	// Verify root.txt was NOT written outside
	dashDir := filepath.Join(hmiDir, dashName)
	indexFile := filepath.Join(dashDir, "index.html")
	if _, err := os.Stat(indexFile); err != nil {
		t.Fatalf("expected index.html to exist in dashboard: %v", err)
	}
}

type fakeDeviceConfigStore struct {
	devices map[string]stores.DeviceConfig
}

func newFakeDeviceConfigStore() *fakeDeviceConfigStore {
	return &fakeDeviceConfigStore{devices: make(map[string]stores.DeviceConfig)}
}

func (f *fakeDeviceConfigStore) GetAll(ctx context.Context) ([]stores.DeviceConfig, error) {
	var res []stores.DeviceConfig
	for _, d := range f.devices {
		res = append(res, d)
	}
	return res, nil
}
func (f *fakeDeviceConfigStore) GetByType(ctx context.Context, dt string) ([]stores.DeviceConfig, error) {
	var res []stores.DeviceConfig
	for _, d := range f.devices {
		if d.Type == dt {
			res = append(res, d)
		}
	}
	return res, nil
}
func (f *fakeDeviceConfigStore) GetByNode(ctx context.Context, nodeID string) ([]stores.DeviceConfig, error) {
	return nil, nil
}
func (f *fakeDeviceConfigStore) GetEnabledByNode(ctx context.Context, nodeID string) ([]stores.DeviceConfig, error) {
	return nil, nil
}
func (f *fakeDeviceConfigStore) Get(ctx context.Context, name string) (*stores.DeviceConfig, error) {
	d, ok := f.devices[name]
	if !ok {
		return nil, nil
	}
	return &d, nil
}
func (f *fakeDeviceConfigStore) Save(ctx context.Context, d stores.DeviceConfig) error {
	f.devices[d.Name] = d
	return nil
}
func (f *fakeDeviceConfigStore) Delete(ctx context.Context, name string) error {
	delete(f.devices, name)
	return nil
}
func (f *fakeDeviceConfigStore) Toggle(ctx context.Context, name string, enabled bool) (*stores.DeviceConfig, error) {
	d, ok := f.devices[name]
	if !ok {
		return nil, nil
	}
	d.Enabled = enabled
	f.devices[name] = d
	return &d, nil
}
func (f *fakeDeviceConfigStore) Reassign(ctx context.Context, name string, nodeID string) (*stores.DeviceConfig, error) {
	return nil, nil
}
func (f *fakeDeviceConfigStore) Close() error { return nil }

func TestEnsureInit_MainDashboardInDB(t *testing.T) {
	tempDir := t.TempDir()
	hmiDir := filepath.Join(tempDir, "hmi_root")
	_ = os.MkdirAll(hmiDir, 0755)

	// Pre-create metadata.json pointing to "smarthome"
	metaContent := `{"mainDashboard": "smarthome"}`
	_ = os.WriteFile(filepath.Join(hmiDir, "metadata.json"), []byte(metaContent), 0644)

	// Create an external folder and symlink it to "smarthome" inside hmiDir
	extDir := filepath.Join(tempDir, "external_smarthome")
	_ = os.MkdirAll(extDir, 0755)
	_ = os.WriteFile(filepath.Join(extDir, "index.html"), []byte("<h1>Smarthome</h1>"), 0644)
	_ = os.Symlink(extDir, filepath.Join(hmiDir, "smarthome"))

	cfg := config.Default()
	cfg.NodeID = "test-node"
	cfg.HMI.Enabled = true
	cfg.HMI.Path = hmiDir

	fakeStore := newFakeDeviceConfigStore()
	mgr := NewManager(cfg, fakeStore)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}

	// 1. EnsureInit should have registered "smarthome" in fakeStore
	dc, err := fakeStore.Get(context.Background(), "smarthome")
	if err != nil || dc == nil {
		t.Fatalf("expected smarthome to be registered in deviceStore, got: %v, err: %v", dc, err)
	}
	if !dc.Enabled || dc.Type != "HMI" {
		t.Errorf("expected enabled HMI device, got enabled=%v type=%s", dc.Enabled, dc.Type)
	}

	// 2. ListHmis should return smarthome from the database
	hmis, err := mgr.ListHmis()
	if err != nil {
		t.Fatalf("ListHmis failed: %v", err)
	}

	var found *HmiDevice
	for _, h := range hmis {
		if h.Name == "smarthome" {
			found = h
			break
		}
	}
	if found == nil {
		t.Fatalf("expected smarthome in ListHmis results: %#v", hmis)
	}
	if !found.Config.IsMain {
		t.Errorf("expected smarthome to have isMain=true")
	}
	// Since smarthome is a symlink, local physical directory stats report 0 files & 0 bytes
	if found.FileCount != 0 || found.SizeBytes != 0 {
		t.Errorf("expected 0 files and 0 bytes for symlinked dashboard, got files=%d bytes=%d", found.FileCount, found.SizeBytes)
	}
}
