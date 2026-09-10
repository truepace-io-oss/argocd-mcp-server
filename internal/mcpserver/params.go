package mcpserver

import (
	"context"
	"fmt"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/metrics"
)

// Shared input structs for the tools. Field descriptions (jsonschema tag) are
// surfaced to the LLM as the tool's parameter documentation.

type instanceParam struct {
	Instance string `json:"instance,omitempty" jsonschema:"the configured Argo CD instance to target; defaults to the server's default instance when omitted"`
}

// metricInstance reports the requested instance for metric labels; promoted to
// every input struct that embeds instanceParam.
func (p instanceParam) metricInstance() string { return p.Instance }

type namespaceParam struct {
	instanceParam
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace holding the Argo CD objects; defaults to the instance's Argo CD namespace (usually 'argocd')"`
}

type appRef struct {
	instanceParam
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace holding the Argo CD Application; defaults to the instance's Argo CD namespace (usually 'argocd')"`
	Name      string `json:"name" jsonschema:"Argo CD Application name"`
}

// resolveInstance picks the target instance from the argument or the default.
func (s *Server) resolveInstance(name string) (*instances.Instance, error) {
	return s.reg.Get(name)
}

// resolveNamespace applies the instance's Argo CD namespace when the caller left
// it empty.
func resolveNamespace(in *instances.Instance, ns string) string {
	if ns == "" {
		return in.Namespace
	}
	return ns
}

// assertWritable blocks mutating operations when writes are disabled globally or
// for the target instance. Kubernetes RBAC is still the ultimate gate; this is
// defense-in-depth so an over-privileged token cannot be used to write through
// an instance an operator intends to be read-only.
func (s *Server) assertWritable(in *instances.Instance) error {
	if s.readOnly {
		metrics.RecordWriteBlocked(in.Name, "global_readonly")
		return fmt.Errorf("this MCP instance is configured read-only (writes disabled globally)")
	}
	if in.ReadOnly {
		metrics.RecordWriteBlocked(in.Name, "instance_readonly")
		return fmt.Errorf("writes are disabled for Argo CD instance %q (readOnly)", in.Name)
	}
	return nil
}

// initiator returns the value written to operation.initiatedBy.username, so
// every sync/rollback is attributable in the Argo CD UI. It uses the
// authenticated caller when agent auth is enabled, and falls back to the plain
// server name otherwise.
func initiator(ctx context.Context) string {
	if info := sdkauth.TokenInfoFromContext(ctx); info != nil && info.UserID != "" {
		return "argocd-mcp:" + info.UserID
	}
	return "argocd-mcp"
}
