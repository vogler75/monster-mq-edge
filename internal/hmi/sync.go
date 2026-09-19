package hmi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/version"
)

// SyncRequest is the JSON envelope sent upstream by the client.
type SyncRequest struct {
	Action        string `json:"action"`
	ReqID         string `json:"reqId"`
	Dashboard     string `json:"dashboard,omitempty"`
	Path          string `json:"path,omitempty"`
	ContentBase64 string `json:"contentBase64,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	ZipBase64     string `json:"zipBase64,omitempty"`
	SetAsMain     bool   `json:"setAsMain,omitempty"`
}

// SyncFileEntry represents a file in a dashboard listing.
type SyncFileEntry struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256,omitempty"`
	ModTime   int64  `json:"modTime,omitempty"`
}

// SyncResponse is the JSON envelope sent downstream by the broker.
type SyncResponse struct {
	Action        string          `json:"action"`
	ReqID         string          `json:"reqId"`
	Success       bool            `json:"success"`
	Error         string          `json:"error,omitempty"`
	Dashboard     string          `json:"dashboard,omitempty"`
	Path          string          `json:"path,omitempty"`
	BytesWritten  int64           `json:"bytesWritten,omitempty"`
	ZipBase64     string          `json:"zipBase64,omitempty"`
	ContentBase64 string          `json:"contentBase64,omitempty"`
	SHA256        string          `json:"sha256,omitempty"`
	Files         []SyncFileEntry `json:"files,omitempty"`
	FileCount     int             `json:"fileCount,omitempty"`
	SizeBytes     int64           `json:"sizeBytes,omitempty"`
	NodeID        string          `json:"nodeId,omitempty"`
	BrokerVersion string          `json:"brokerVersion,omitempty"`
	MainDashboard string          `json:"mainDashboard,omitempty"`
	Dashboards    []string        `json:"dashboards,omitempty"`
}

type incomingMsg struct {
	sessionUUID string
	req         SyncRequest
}

// SyncService handles bidirectional HMI file synchronization over MQTT.
type SyncService struct {
	mgr       *Manager
	server    *mqtt.Server
	publishFn func(topic string, payload []byte, retain bool, qos byte) error
	nodeID    string
	baseTopic string
	logger    *slog.Logger

	mu      sync.Mutex
	started bool
	stopped bool
	queue   chan incomingMsg
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewSyncService creates a new SyncService.
func NewSyncService(
	mgr *Manager,
	server *mqtt.Server,
	publishFn func(topic string, payload []byte, retain bool, qos byte) error,
	nodeID string,
	baseTopic string,
	logger *slog.Logger,
) *SyncService {
	base := strings.TrimRight(baseTopic, "/")
	if base == "" {
		base = "monstermq/hmi/sync"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SyncService{
		mgr:       mgr,
		server:    server,
		publishFn: publishFn,
		nodeID:    nodeID,
		baseTopic: base,
		logger:    logger,
		queue:     make(chan incomingMsg, 256),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Start begins processing synchronization requests.
func (s *SyncService) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started || s.stopped {
		return nil
	}
	s.started = true

	// Worker to process incoming sync requests off the hot path
	s.wg.Add(1)
	go s.workerLoop()

	// Subscribe to: <baseTopic>/+/upstream
	filter := s.baseTopic + "/+/upstream"
	err := s.server.Subscribe(filter, 0, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		s.handlePacket(pk)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to HMI sync filter", "filter", filter, "err", err)
		return err
	}

	s.logger.Info("HMI MQTT file sync service started", "filter", filter)
	return nil
}

// Stop terminates the sync service.
func (s *SyncService) Stop() {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.cancel()
	s.mu.Unlock()

	s.wg.Wait()
	s.logger.Info("HMI MQTT file sync service stopped")
}

func (s *SyncService) handlePacket(pk packets.Packet) {
	// Parse session UUID from topic: <baseTopic>/<sessionUUID>/upstream
	topic := pk.TopicName
	prefix := s.baseTopic + "/"
	if !strings.HasPrefix(topic, prefix) || !strings.HasSuffix(topic, "/upstream") {
		return
	}

	trimmed := strings.TrimPrefix(topic, prefix)
	trimmed = strings.TrimSuffix(trimmed, "/upstream")
	sessionUUID := strings.TrimSpace(trimmed)
	if sessionUUID == "" || strings.Contains(sessionUUID, "/") {
		return
	}

	var req SyncRequest
	if err := json.Unmarshal(pk.Payload, &req); err != nil {
		s.logger.Warn("invalid HMI sync JSON payload", "session", sessionUUID, "err", err)
		s.sendDownstream(sessionUUID, SyncResponse{
			Action:  "error",
			Success: false,
			Error:   "invalid JSON payload: " + err.Error(),
		})
		return
	}

	select {
	case s.queue <- incomingMsg{sessionUUID: sessionUUID, req: req}:
	default:
		s.logger.Warn("HMI sync queue full, dropping request", "session", sessionUUID, "action", req.Action)
		s.sendDownstream(sessionUUID, SyncResponse{
			Action:  req.Action,
			ReqID:   req.ReqID,
			Success: false,
			Error:   "server busy: sync queue full",
		})
	}
}

func (s *SyncService) workerLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		case msg := <-s.queue:
			s.processRequest(msg.sessionUUID, msg.req)
		}
	}
}

func (s *SyncService) processRequest(sessionUUID string, req SyncRequest) {
	resp := SyncResponse{
		Action: req.Action,
		ReqID:  req.ReqID,
	}

	switch strings.ToLower(req.Action) {
	case "ping":
		s.handlePing(&resp)
	case "list":
		s.handleList(req, &resp)
	case "export":
		s.handleExport(req, &resp)
	case "read":
		s.handleRead(req, &resp)
	case "write":
		s.handleWrite(req, &resp)
	case "delete":
		s.handleDelete(req, &resp)
	case "import":
		s.handleImport(req, &resp)
	default:
		resp.Success = false
		resp.Error = fmt.Sprintf("unsupported action %q", req.Action)
	}

	s.sendDownstream(sessionUUID, resp)
}

func (s *SyncService) handlePing(resp *SyncResponse) {
	resp.Success = true
	resp.NodeID = s.nodeID
	resp.BrokerVersion = version.Version
	resp.MainDashboard = s.mgr.GetMainDashboardName()

	hmis, err := s.mgr.ListHmis()
	if err == nil {
		dashboards := make([]string, 0, len(hmis))
		for _, h := range hmis {
			dashboards = append(dashboards, h.Name)
		}
		resp.Dashboards = dashboards
	}
}

func (s *SyncService) handleList(req SyncRequest, resp *SyncResponse) {
	dashName := strings.TrimSpace(req.Dashboard)
	if dashName == "" {
		dashName = s.mgr.GetMainDashboardName()
	}
	resp.Dashboard = dashName

	files, err := s.mgr.ListDashboardFiles(dashName)
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
		return
	}

	entries := make([]SyncFileEntry, 0, len(files))
	for _, f := range files {
		entry := SyncFileEntry{
			Path:      f.Path,
			SizeBytes: f.SizeBytes,
		}

		fullPath, err := s.mgr.ResolveDashboardPath(dashName, f.Path)
		if err == nil {
			if info, err := os.Stat(fullPath); err == nil {
				entry.ModTime = info.ModTime().Unix()
			}
			if data, err := os.ReadFile(fullPath); err == nil {
				h := sha256.Sum256(data)
				entry.SHA256 = hex.EncodeToString(h[:])
			}
		}

		entries = append(entries, entry)
	}

	resp.Success = true
	resp.Files = entries
	resp.FileCount = len(entries)
}

func (s *SyncService) handleExport(req SyncRequest, resp *SyncResponse) {
	dashName := strings.TrimSpace(req.Dashboard)
	if dashName == "" {
		dashName = s.mgr.GetMainDashboardName()
	}
	resp.Dashboard = dashName

	zipB64, err := s.mgr.ExportDashboardZip(dashName)
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
		return
	}

	hmiDev, _ := s.mgr.GetHmi(dashName)
	var fileCount int
	var sizeBytes int64
	if hmiDev != nil {
		fileCount = hmiDev.FileCount
		sizeBytes = hmiDev.SizeBytes
	}

	resp.Success = true
	resp.ZipBase64 = zipB64
	resp.FileCount = fileCount
	resp.SizeBytes = sizeBytes
}

func (s *SyncService) handleRead(req SyncRequest, resp *SyncResponse) {
	dashName := strings.TrimSpace(req.Dashboard)
	if dashName == "" {
		dashName = s.mgr.GetMainDashboardName()
	}
	resp.Dashboard = dashName
	resp.Path = req.Path

	data, err := s.mgr.ReadDashboardFile(dashName, req.Path)
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
		return
	}

	h := sha256.Sum256(data)
	resp.Success = true
	resp.ContentBase64 = base64.StdEncoding.EncodeToString(data)
	resp.SHA256 = hex.EncodeToString(h[:])
	resp.SizeBytes = int64(len(data))
}

func (s *SyncService) handleWrite(req SyncRequest, resp *SyncResponse) {
	dashName := strings.TrimSpace(req.Dashboard)
	if dashName == "" {
		dashName = s.mgr.GetMainDashboardName()
	}
	resp.Dashboard = dashName
	resp.Path = req.Path

	if strings.TrimSpace(req.Path) == "" {
		resp.Success = false
		resp.Error = "path cannot be empty"
		return
	}

	data, err := base64.StdEncoding.DecodeString(req.ContentBase64)
	if err != nil {
		resp.Success = false
		resp.Error = "invalid base64 content: " + err.Error()
		return
	}

	// Verify SHA256 if provided
	if req.SHA256 != "" {
		h := sha256.Sum256(data)
		expected := strings.ToLower(strings.TrimSpace(req.SHA256))
		actual := hex.EncodeToString(h[:])
		if actual != expected {
			resp.Success = false
			resp.Error = fmt.Sprintf("sha256 mismatch: expected %s, got %s", expected, actual)
			return
		}
	}

	if err := s.mgr.WriteDashboardFile(dashName, req.Path, data); err != nil {
		resp.Success = false
		resp.Error = err.Error()
		return
	}

	resp.Success = true
	resp.BytesWritten = int64(len(data))
}

func (s *SyncService) handleDelete(req SyncRequest, resp *SyncResponse) {
	dashName := strings.TrimSpace(req.Dashboard)
	if dashName == "" {
		dashName = s.mgr.GetMainDashboardName()
	}
	resp.Dashboard = dashName
	resp.Path = req.Path

	if strings.TrimSpace(req.Path) == "" {
		resp.Success = false
		resp.Error = "path cannot be empty"
		return
	}

	if err := s.mgr.DeleteDashboardFile(dashName, req.Path); err != nil {
		resp.Success = false
		resp.Error = err.Error()
		return
	}

	resp.Success = true
}

func (s *SyncService) handleImport(req SyncRequest, resp *SyncResponse) {
	dashName := strings.TrimSpace(req.Dashboard)
	if dashName == "" {
		dashName = s.mgr.GetMainDashboardName()
	}
	resp.Dashboard = dashName

	if strings.TrimSpace(req.ZipBase64) == "" {
		resp.Success = false
		resp.Error = "zipBase64 cannot be empty"
		return
	}

	_, err := s.mgr.UploadDashboardZip(dashName, req.ZipBase64, req.SetAsMain)
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
		return
	}

	resp.Success = true
}

func (s *SyncService) sendDownstream(sessionUUID string, resp SyncResponse) {
	topic := fmt.Sprintf("%s/%s/downstream", s.baseTopic, sessionUUID)
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("failed to marshal sync response", "err", err)
		return
	}

	if err := s.publishFn(topic, data, false, 1); err != nil {
		s.logger.Warn("failed to publish sync downstream", "topic", topic, "err", err)
	}
}
