// Package mcpserver builds the MCP server: it registers the Argo CD tools and
// routes each call to the right instance in the registry. It contains no
// authorization logic — every request is executed with the target cluster's
// ServiceAccount credentials and authorized by Kubernetes RBAC on argoproj.io.
//
// The tool surface is deliberately limited to Argo CD domain operations.
// Pod logs, events, nodes and generic resource access belong to the separate
// kubernetes-mcp server and are intentionally not re-implemented here.
package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
)

// serverVersion is injected by main via SetVersion.
var serverVersion = "dev"

// SetVersion sets the version advertised by the MCP server.
func SetVersion(v string) {
	if v != "" {
		serverVersion = v
	}
}

// Server holds the shared dependencies for all tool handlers.
type Server struct {
	reg      *instances.Registry
	readOnly bool // global kill-switch
}

// New builds a Server from the registry and config.
func New(reg *instances.Registry, cfg *config.Config) *Server {
	return &Server{reg: reg, readOnly: cfg.ReadOnly}
}

// MCPServer constructs an *mcp.Server with all tools registered. The streamable
// HTTP handler calls this (via a closure) per session.
func (s *Server) MCPServer() *mcp.Server {
	m := mcp.NewServer(&mcp.Implementation{
		Name:    "argocd-mcp",
		Version: serverVersion,
	}, nil)

	s.registerReadTools(m)
	s.registerWriteTools(m)
	return m
}
