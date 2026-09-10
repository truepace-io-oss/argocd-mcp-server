package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// notThisServer is appended to tool descriptions so the model routes
// non-Argo-CD questions to the kubernetes-mcp server instead.
const notThisServer = " For pod logs, events, nodes or any non-Argo-CD object use the kubernetes-mcp server instead."

func (s *Server) registerReadTools(m *mcp.Server) {
	addTool(m, s, "instances_list",
		"List the Argo CD instances this MCP manages, with their namespace, read-only flag and reachability."+notThisServer,
		s.instancesList)
	addTool(m, s, "apps_list",
		"List Argo CD Applications with sync status, health, project, revision and destination. Supports filtering by project, sync/health status, label selector and name substring."+notThisServer,
		s.appsList)
	addTool(m, s, "app_get",
		"Get one Argo CD Application: sources, destination, sync policy, sync/health state, current operation, conditions and recent sync history."+notThisServer,
		s.appGet)
	addTool(m, s, "app_resources",
		"List the Kubernetes resources an Argo CD Application manages with their per-resource sync and health state — the drift summary. This is Argo CD's own comparison result, not a manifest diff."+notThisServer,
		s.appResources)
	addTool(m, s, "app_history",
		"List an Application's sync history (id, revision, deployment time). The id is the input for app_rollback.",
		s.appHistory)
	addTool(m, s, "projects_list",
		"List Argo CD AppProjects with their allowed source repositories and destinations.",
		s.projectsList)
	addTool(m, s, "appsets_list",
		"List Argo CD ApplicationSets with their generator kinds, conditions and the number of Applications they own.",
		s.appSetsList)
	addTool(m, s, "app_wait",
		"Wait until an Application reaches Synced (and optionally Healthy) or its running operation finishes. Read-only; polls until the timeout.",
		s.appWait)
}

func (s *Server) instancesList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	var b strings.Builder
	b.WriteString("Managed Argo CD instances:\n")
	for _, in := range s.reg.All() {
		marker := ""
		if in.Name == s.reg.DefaultName() {
			marker = " (default)"
		}
		status, err := in.Ping(ctx)
		if err != nil {
			status = "UNREACHABLE: " + err.Error()
		}
		guard, reason, _ := in.CheckWriteGuard(ctx)
		fmt.Fprintf(&b, "- %s%s namespaces=%s readOnly=%t writeGuard=%s — %s\n",
			in.Name, marker, strings.Join(in.Namespaces, ","), in.ReadOnly, guard, status)
		if guard == argocd.GuardMissing {
			fmt.Fprintf(&b, "    WARNING: %s — these credentials can rewrite an Application's source. "+
				"Install the argocd-mcp write guard (ValidatingAdmissionPolicy) in that cluster before enabling writes.\n", reason)
		}
	}
	return textResult(b.String()), nil, nil
}

type appsListParam struct {
	namespaceParam
	Project       string `json:"project,omitempty" jsonschema:"only applications belonging to this AppProject"`
	SyncStatus    string `json:"syncStatus,omitempty" jsonschema:"only applications with this sync status: Synced, OutOfSync or Unknown"`
	HealthStatus  string `json:"healthStatus,omitempty" jsonschema:"only applications with this health status: Healthy, Progressing, Degraded, Suspended, Missing or Unknown"`
	LabelSelector string `json:"labelSelector,omitempty" jsonschema:"optional Kubernetes label selector applied server-side, e.g. 'env=prod'"`
	NameContains  string `json:"nameContains,omitempty" jsonschema:"only applications whose name contains this substring (case-insensitive)"`
}

func (s *Server) appsList(ctx context.Context, _ *mcp.CallToolRequest, in appsListParam) (*mcp.CallToolResult, any, error) {
	inst, err := s.resolveInstance(in.Instance)
	if err != nil {
		return errorResult(err), nil, nil
	}
	namespaces := listNamespaces(inst, in.Namespace)

	var apps []*argocd.App
	for _, ns := range namespaces {
		list, err := argocd.ListApps(ctx, inst.Dynamic, ns, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return errorResult(err), nil, nil
		}
		for i := range list.Items {
			a := argocd.AppFromUnstructured(&list.Items[i])
			if !matchesFilters(a, in) {
				continue
			}
			apps = append(apps, a)
		}
	}
	return textResult(appsTable(inst.Name, strings.Join(namespaces, ","), apps)), nil, nil
}

func matchesFilters(a *argocd.App, in appsListParam) bool {
	if in.Project != "" && !strings.EqualFold(a.Project, in.Project) {
		return false
	}
	if in.SyncStatus != "" && !strings.EqualFold(a.SyncStatus, in.SyncStatus) {
		return false
	}
	if in.HealthStatus != "" && !strings.EqualFold(a.HealthStatus, in.HealthStatus) {
		return false
	}
	if in.NameContains != "" && !strings.Contains(strings.ToLower(a.Name), strings.ToLower(in.NameContains)) {
		return false
	}
	return true
}

// listNamespaces returns the namespaces a list tool should search: the explicit
// one when given, otherwise every namespace configured for the instance
// (Argo CD's "apps in any namespace" support).
func listNamespaces(in *instances.Instance, explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	if len(in.Namespaces) == 0 {
		return []string{in.Namespace}
	}
	return in.Namespaces
}

// getApp is the shared "resolve instance, fetch and project one Application"
// helper used by most tools.
func (s *Server) getApp(ctx context.Context, instance, namespace, name string) (*instances.Instance, *argocd.App, error) {
	inst, err := s.resolveInstance(instance)
	if err != nil {
		return nil, nil, err
	}
	if name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	ns := resolveNamespace(inst, namespace)
	obj, err := argocd.GetApp(ctx, inst.Dynamic, ns, name)
	if err != nil {
		return nil, nil, err
	}
	return inst, argocd.AppFromUnstructured(obj), nil
}

func (s *Server) appGet(ctx context.Context, _ *mcp.CallToolRequest, in appRef) (*mcp.CallToolResult, any, error) {
	inst, app, err := s.getApp(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	return textResult(appDetail(inst.Name, app, 5)), nil, nil
}

type appResourcesParam struct {
	appRef
	OnlyDrifted *bool `json:"onlyDrifted,omitempty" jsonschema:"when true (the default) list only resources that are OutOfSync, unhealthy or awaiting pruning"`
}

func (s *Server) appResources(ctx context.Context, _ *mcp.CallToolRequest, in appResourcesParam) (*mcp.CallToolResult, any, error) {
	_, app, err := s.getApp(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	onlyDrifted := true
	if in.OnlyDrifted != nil {
		onlyDrifted = *in.OnlyDrifted
	}
	return textResult(resourcesTable(app, onlyDrifted)), nil, nil
}

type appHistoryParam struct {
	appRef
	Limit int `json:"limit,omitempty" jsonschema:"how many of the most recent history entries to return (default 10)"`
}

func (s *Server) appHistory(ctx context.Context, _ *mcp.CallToolRequest, in appHistoryParam) (*mcp.CallToolResult, any, error) {
	_, app, err := s.getApp(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	return textResult(historyTable(app, limit)), nil, nil
}

func (s *Server) projectsList(ctx context.Context, _ *mcp.CallToolRequest, in namespaceParam) (*mcp.CallToolResult, any, error) {
	inst, err := s.resolveInstance(in.Instance)
	if err != nil {
		return errorResult(err), nil, nil
	}
	var out []*argocd.Project
	for _, ns := range listNamespaces(inst, in.Namespace) {
		list, err := argocd.ListProjects(ctx, inst.Dynamic, ns, metav1.ListOptions{})
		if err != nil {
			return errorResult(err), nil, nil
		}
		for i := range list.Items {
			out = append(out, argocd.ProjectFromUnstructured(&list.Items[i]))
		}
	}
	return textResult(projectsTable(inst.Name, out)), nil, nil
}

func (s *Server) appSetsList(ctx context.Context, _ *mcp.CallToolRequest, in namespaceParam) (*mcp.CallToolResult, any, error) {
	inst, err := s.resolveInstance(in.Instance)
	if err != nil {
		return errorResult(err), nil, nil
	}
	namespaces := listNamespaces(inst, in.Namespace)

	var sets []*argocd.AppSet
	for _, ns := range namespaces {
		list, err := argocd.ListAppSets(ctx, inst.Dynamic, ns, metav1.ListOptions{})
		if err != nil {
			return errorResult(err), nil, nil
		}
		for i := range list.Items {
			sets = append(sets, argocd.AppSetFromUnstructured(&list.Items[i]))
		}
	}

	// Count owned Applications via the ownerReference the AppSet controller sets.
	owned := map[string]int{}
	for _, ns := range namespaces {
		list, err := argocd.ListApps(ctx, inst.Dynamic, ns, metav1.ListOptions{})
		if err != nil {
			continue // owner counts are a nicety; never fail the listing for them
		}
		for i := range list.Items {
			for _, ref := range list.Items[i].GetOwnerReferences() {
				if ref.Kind == "ApplicationSet" {
					owned[list.Items[i].GetNamespace()+"/"+ref.Name]++
				}
			}
		}
	}
	return textResult(appSetsTable(inst.Name, sets, owned)), nil, nil
}

type appWaitParam struct {
	appRef
	TimeoutSeconds int   `json:"timeoutSeconds,omitempty" jsonschema:"how long to wait, 1-900 seconds (default 300)"`
	PollSeconds    int   `json:"pollSeconds,omitempty" jsonschema:"poll interval in seconds, 1-30 (default 3)"`
	ForHealthy     *bool `json:"forHealthy,omitempty" jsonschema:"also wait for health=Healthy, not just sync=Synced (default true)"`
}

func (s *Server) appWait(ctx context.Context, _ *mcp.CallToolRequest, in appWaitParam) (*mcp.CallToolResult, any, error) {
	timeout := clamp(in.TimeoutSeconds, 1, 900, 300)
	poll := clamp(in.PollSeconds, 1, 30, 3)
	forHealthy := true
	if in.ForHealthy != nil {
		forHealthy = *in.ForHealthy
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	var last *argocd.App
	for {
		inst, app, err := s.getApp(ctx, in.Instance, in.Namespace, in.Name)
		if err != nil {
			return errorResult(err), nil, nil
		}
		last = app

		if app.Operation != nil && (app.Operation.Phase == argocd.PhaseFailed || app.Operation.Phase == argocd.PhaseError) {
			return errorResult(fmt.Errorf("operation on %s/%s ended in phase %s: %s",
				app.Namespace, app.Name, app.Operation.Phase, truncate(app.Operation.Message, maxMessageLen))), nil, nil
		}
		if synced(app, forHealthy) {
			return textResult(fmt.Sprintf("%s/%s reached sync=%s health=%s (revision %s) @ %s",
				app.Namespace, app.Name, dash(app.SyncStatus), dash(app.HealthStatus),
				shortRev(app.SyncRevision), inst.Name)), nil, nil
		}
		if time.Now().Add(time.Duration(poll) * time.Second).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return errorResult(ctx.Err()), nil, nil
		case <-time.After(time.Duration(poll) * time.Second):
		}
	}

	phase := "-"
	if last != nil && last.Operation != nil {
		phase = dash(last.Operation.Phase)
	}
	return errorResult(fmt.Errorf("timed out after %ds waiting for %s/%s; last state sync=%s health=%s operation=%s",
		timeout, last.Namespace, last.Name, dash(last.SyncStatus), dash(last.HealthStatus), phase)), nil, nil
}

// synced reports whether the application reached the desired steady state.
func synced(a *argocd.App, forHealthy bool) bool {
	if a.SyncStatus != argocd.SyncSynced {
		return false
	}
	if forHealthy && a.HealthStatus != argocd.HealthHealthy {
		return false
	}
	return !a.HasPendingOp && !a.Operation.Running()
}

func clamp(v, min, max, def int) int {
	if v <= 0 {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
