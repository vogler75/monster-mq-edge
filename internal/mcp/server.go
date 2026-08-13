package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/hmi"
)

type Server struct {
	cfg    *config.Config
	hmiMgr *hmi.Manager
	logger *slog.Logger
	mcpSrv *mcp.Server
}

type CreateDashboardParams struct {
	Name      string `json:"name"`
	SetAsMain bool   `json:"set_as_main"`
}

type DeleteDashboardParams struct {
	Name string `json:"name"`
}

type SetMainDashboardParams struct {
	Name string `json:"name"`
}

type ListHMIFilesParams struct {
	Dashboard string `json:"dashboard"`
}

type ReadHMIFileParams struct {
	Dashboard string `json:"dashboard"`
	Path      string `json:"path"`
}

type WriteHMIFileParams struct {
	Dashboard string `json:"dashboard"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

func NewServer(cfg *config.Config, hmiMgr *hmi.Manager, logger *slog.Logger) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "monstermq-edge-hmi",
		Version: "1.0.0",
	}, nil)

	s := &Server{
		cfg:    cfg,
		hmiMgr: hmiMgr,
		logger: logger,
		mcpSrv: mcpSrv,
	}

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "list_dashboards",
		Description: "List all HMI dashboard apps and show which dashboard is set as main.",
	}, s.handleListDashboards)

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "create_dashboard",
		Description: "Create a new HMI dashboard app directory.",
	}, s.handleCreateDashboard)

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "delete_dashboard",
		Description: "Delete an HMI dashboard app directory.",
	}, s.handleDeleteDashboard)

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "set_main_dashboard",
		Description: "Set an HMI dashboard app as the main dashboard (served at /hmi/).",
	}, s.handleSetMainDashboard)

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "list_hmi_files",
		Description: "List all files in a dashboard app directory.",
	}, s.handleListFiles)

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "read_hmi_file",
		Description: "Read the contents of an HMI file from a dashboard app.",
	}, s.handleReadFile)

	mcp.AddTool(mcpSrv, &mcp.Tool{
		Name:        "write_hmi_file",
		Description: "Write content to an HMI file in a dashboard app, creating it if it doesn't exist.",
	}, s.handleWriteFile)

	return s
}

func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.cfg.MCP.Port)
	s.logger.Info("mcp server listening", "port", s.cfg.MCP.Port)

	if s.hmiMgr != nil {
		_ = s.hmiMgr.EnsureInit()
	}

	handler := mcp.NewSSEHandler(func(request *http.Request) *mcp.Server {
		return s.mcpSrv
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/", handler)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			s.logger.Error("mcp server error", "error", err)
		}
	}()

	return nil
}

func (s *Server) targetDashboard(specified string) string {
	if specified != "" {
		return specified
	}
	if s.hmiMgr != nil {
		return s.hmiMgr.GetMainDashboardName()
	}
	return "main"
}

func (s *Server) handleListDashboards(ctx context.Context, req *mcp.CallToolRequest, args any) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	dashboards, err := s.hmiMgr.ListDashboards()
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	var sb strings.Builder
	for _, d := range dashboards {
		isMainStr := ""
		if d.IsMain {
			isMainStr = " (MAIN)"
		}
		sb.WriteString(fmt.Sprintf("- %s%s: path=%s, files=%d, size=%d bytes\n", d.Name, isMainStr, d.Path, d.FileCount, d.SizeBytes))
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: sb.String()},
		},
	}, nil, nil
}

func (s *Server) handleCreateDashboard(ctx context.Context, req *mcp.CallToolRequest, args CreateDashboardParams) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	d, err := s.hmiMgr.CreateDashboard(args.Name, args.SetAsMain)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Successfully created dashboard %q at %s", d.Name, d.Path)},
		},
	}, nil, nil
}

func (s *Server) handleDeleteDashboard(ctx context.Context, req *mcp.CallToolRequest, args DeleteDashboardParams) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	err := s.hmiMgr.DeleteDashboard(args.Name)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Successfully deleted dashboard %q", args.Name)},
		},
	}, nil, nil
}

func (s *Server) handleSetMainDashboard(ctx context.Context, req *mcp.CallToolRequest, args SetMainDashboardParams) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	err := s.hmiMgr.SetMainDashboard(args.Name)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Successfully set %q as the main dashboard", args.Name)},
		},
	}, nil, nil
}

func (s *Server) handleListFiles(ctx context.Context, req *mcp.CallToolRequest, args ListHMIFilesParams) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	dashName := s.targetDashboard(args.Dashboard)
	files, err := s.hmiMgr.ListDashboardFiles(dashName)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	var filePaths []string
	for _, f := range files {
		filePaths = append(filePaths, f.Path)
	}

	res := strings.Join(filePaths, "\n")
	if res == "" {
		res = "(empty directory)"
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Dashboard: %s\nFiles:\n%s", dashName, res)},
		},
	}, nil, nil
}

func (s *Server) handleReadFile(ctx context.Context, req *mcp.CallToolRequest, args ReadHMIFileParams) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	dashName := s.targetDashboard(args.Dashboard)
	data, err := s.hmiMgr.ReadDashboardFile(dashName, args.Path)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(data)},
		},
	}, nil, nil
}

func (s *Server) handleWriteFile(ctx context.Context, req *mcp.CallToolRequest, args WriteHMIFileParams) (*mcp.CallToolResult, any, error) {
	if s.hmiMgr == nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "HMI is not enabled"}}}, nil, nil
	}

	dashName := s.targetDashboard(args.Dashboard)
	err := s.hmiMgr.WriteDashboardFile(dashName, args.Path, []byte(args.Content))
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Successfully wrote %d bytes to %s in dashboard %s", len(args.Content), args.Path, dashName)},
		},
	}, nil, nil
}
