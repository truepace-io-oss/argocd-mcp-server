package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/metrics"
)

// governed is appended to every mutating tool description.
const governed = " Blocked when this MCP instance or the target Argo CD instance is read-only; ultimately governed by Kubernetes RBAC on argoproj.io."

func (s *Server) registerWriteTools(m *mcp.Server) {
	addTool(m, s, "app_sync",
		"Sync an Argo CD Application: requests the same operation as `argocd app sync`, with optional revision, prune, dry-run, sync options, partial resource selection and retry."+governed,
		s.appSync)
	addTool(m, s, "app_refresh",
		"Refresh an Argo CD Application so the controller re-compares it against its source (normal or hard refresh). Does not deploy anything."+governed,
		s.appRefresh)
	addTool(m, s, "app_rollback",
		"Roll an Argo CD Application back to a previous sync history entry (see app_history for the id)."+governed,
		s.appRollback)
	addTool(m, s, "app_terminate_operation",
		"Terminate the operation currently running on an Argo CD Application."+governed,
		s.appTerminateOperation)
}

// prepareWrite resolves the instance, enforces the read-only guards and loads
// the current application state that the guards and defaults need.
func (s *Server) prepareWrite(ctx context.Context, instance, namespace, name string) (*instances.Instance, *argocd.App, error) {
	inst, err := s.resolveInstance(instance)
	if err != nil {
		return nil, nil, err
	}
	if err := s.assertWritable(inst); err != nil {
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

type appSyncParam struct {
	appRef
	Revision    string               `json:"revision,omitempty" jsonschema:"git revision or chart version to sync to; defaults to the application's own targetRevision"`
	Prune       bool                 `json:"prune,omitempty" jsonschema:"delete resources that are no longer in the source"`
	DryRun      bool                 `json:"dryRun,omitempty" jsonschema:"perform a server-side dry run without changing anything"`
	SyncOptions []string             `json:"syncOptions,omitempty" jsonschema:"per-sync options, e.g. 'ApplyOutOfSyncOnly=true' or 'Validate=false'"`
	Resources   []argocd.ResourceRef `json:"resources,omitempty" jsonschema:"sync only these resources; each needs kind and name, optionally group and namespace"`
	Strategy    string               `json:"strategy,omitempty" jsonschema:"sync strategy: 'apply' or 'hook' (default: Argo CD's own default)"`
	Force       bool                 `json:"force,omitempty" jsonschema:"use kubectl apply --force; only valid with the 'apply' strategy"`
	RetryLimit  int                  `json:"retryLimit,omitempty" jsonschema:"how often Argo CD retries a failed sync"`
	Reason      string               `json:"reason,omitempty" jsonschema:"short note recorded on the operation, e.g. why the sync was requested"`
}

func (s *Server) appSync(ctx context.Context, _ *mcp.CallToolRequest, in appSyncParam) (*mcp.CallToolResult, any, error) {
	inst, app, err := s.prepareWrite(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	if err := argocd.CheckNoOperationInProgress(app); err != nil {
		metrics.RecordOperation(inst.Name, "sync", "conflict")
		return errorResult(err), nil, nil
	}

	revision := in.Revision
	if revision == "" {
		revision = argocd.DefaultRevision(app)
	}
	reason := in.Reason
	if reason == "" {
		reason = "requested via argocd-mcp"
	}
	patch, err := argocd.SyncPatch(argocd.SyncRequest{
		Revision:    revision,
		Prune:       in.Prune,
		DryRun:      in.DryRun,
		SyncOptions: in.SyncOptions,
		Resources:   in.Resources,
		Strategy:    in.Strategy,
		Force:       in.Force,
		RetryLimit:  in.RetryLimit,
		InitiatedBy: initiator(ctx),
		Reason:      reason,
	})
	if err != nil {
		metrics.RecordOperation(inst.Name, "sync", "error")
		return errorResult(err), nil, nil
	}
	if _, err := argocd.PatchApp(ctx, inst.Dynamic, app.Namespace, app.Name, argocd.PatchType, patch); err != nil {
		metrics.RecordOperation(inst.Name, "sync", "error")
		return errorResult(err), nil, nil
	}
	metrics.RecordOperation(inst.Name, "sync", "ok")

	var b strings.Builder
	fmt.Fprintf(&b, "sync requested for %s/%s @ %s (revision %s", app.Namespace, app.Name, inst.Name, shortRev(revision))
	if in.DryRun {
		b.WriteString(", dryRun")
	}
	if in.Prune {
		b.WriteString(", prune")
	}
	if len(in.Resources) > 0 {
		fmt.Fprintf(&b, ", %d selected resource(s)", len(in.Resources))
	}
	b.WriteString("). The application controller executes it asynchronously — use app_wait or app_get to follow it.")
	return textResult(b.String()), nil, nil
}

type appRefreshParam struct {
	appRef
	Hard           bool  `json:"hard,omitempty" jsonschema:"hard refresh: also invalidate the repo-server manifest cache"`
	Wait           *bool `json:"wait,omitempty" jsonschema:"wait until the controller has reconciled (default true)"`
	TimeoutSeconds int   `json:"timeoutSeconds,omitempty" jsonschema:"how long to wait for the reconcile, 1-300 seconds (default 60)"`
}

func (s *Server) appRefresh(ctx context.Context, _ *mcp.CallToolRequest, in appRefreshParam) (*mcp.CallToolResult, any, error) {
	inst, app, err := s.prepareWrite(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	before := app.ReconciledAt

	if _, err := argocd.PatchApp(ctx, inst.Dynamic, app.Namespace, app.Name, argocd.PatchType, argocd.RefreshPatch(in.Hard)); err != nil {
		metrics.RecordOperation(inst.Name, "refresh", "error")
		return errorResult(err), nil, nil
	}
	metrics.RecordOperation(inst.Name, "refresh", "ok")

	kind := argocd.RefreshNormal
	if in.Hard {
		kind = argocd.RefreshHard
	}
	wait := true
	if in.Wait != nil {
		wait = *in.Wait
	}
	if !wait {
		return textResult(fmt.Sprintf("%s refresh requested for %s/%s @ %s", kind, app.Namespace, app.Name, inst.Name)), nil, nil
	}

	timeout := clamp(in.TimeoutSeconds, 1, 300, 60)
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		obj, err := argocd.GetApp(ctx, inst.Dynamic, app.Namespace, app.Name)
		if err != nil {
			return errorResult(err), nil, nil
		}
		cur := argocd.AppFromUnstructured(obj)
		if cur.RefreshRequested == "" || reconciledAfter(cur.ReconciledAt, before) {
			return textResult(fmt.Sprintf("%s refresh of %s/%s @ %s completed: sync=%s health=%s (revision %s)",
				kind, cur.Namespace, cur.Name, inst.Name, dash(cur.SyncStatus), dash(cur.HealthStatus), shortRev(cur.SyncRevision))), nil, nil
		}
		if time.Now().Add(time.Second).After(deadline) {
			// Not an error: the refresh *was* requested, the controller just has
			// not caught up yet.
			return textResult(fmt.Sprintf("%s refresh requested for %s/%s @ %s, but the application controller had not reconciled within %ds; last state sync=%s health=%s",
				kind, cur.Namespace, cur.Name, inst.Name, timeout, dash(cur.SyncStatus), dash(cur.HealthStatus))), nil, nil
		}
		select {
		case <-ctx.Done():
			return errorResult(ctx.Err()), nil, nil
		case <-time.After(2 * time.Second):
		}
	}
}

func reconciledAfter(cur, before *time.Time) bool {
	if cur == nil {
		return false
	}
	if before == nil {
		return true
	}
	return cur.After(*before)
}

type appRollbackParam struct {
	appRef
	ID     int64 `json:"id" jsonschema:"history id to roll back to (see app_history)"`
	Prune  bool  `json:"prune,omitempty" jsonschema:"delete resources that are not part of the rolled-back revision"`
	DryRun bool  `json:"dryRun,omitempty" jsonschema:"perform a server-side dry run without changing anything"`
	Force  bool  `json:"force,omitempty" jsonschema:"roll back even though automated sync is enabled (auto-sync would otherwise immediately undo it)"`
}

func (s *Server) appRollback(ctx context.Context, _ *mcp.CallToolRequest, in appRollbackParam) (*mcp.CallToolResult, any, error) {
	inst, app, err := s.prepareWrite(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	if err := argocd.CheckNoOperationInProgress(app); err != nil {
		metrics.RecordOperation(inst.Name, "rollback", "conflict")
		return errorResult(err), nil, nil
	}
	entry, err := argocd.FindHistoryEntry(app, in.ID)
	if err != nil {
		metrics.RecordOperation(inst.Name, "rollback", "error")
		return errorResult(err), nil, nil
	}
	if app.AutoSync && !in.Force {
		metrics.RecordOperation(inst.Name, "rollback", "error")
		return errorResult(fmt.Errorf(
			"%s/%s has automated sync enabled (%s) — a rollback would be reverted by the controller; disable auto-sync or pass force=true",
			app.Namespace, app.Name, app.AutoSyncLabel())), nil, nil
	}

	patch, err := argocd.SyncPatch(argocd.SyncRequest{
		Revision:    entry.Revision,
		Prune:       in.Prune,
		DryRun:      in.DryRun,
		Source:      entry.Source,
		Sources:     entry.Sources,
		InitiatedBy: initiator(ctx),
		Reason:      fmt.Sprintf("rollback to history id %d via argocd-mcp", entry.ID),
	})
	if err != nil {
		metrics.RecordOperation(inst.Name, "rollback", "error")
		return errorResult(err), nil, nil
	}
	if _, err := argocd.PatchApp(ctx, inst.Dynamic, app.Namespace, app.Name, argocd.PatchType, patch); err != nil {
		metrics.RecordOperation(inst.Name, "rollback", "error")
		return errorResult(err), nil, nil
	}
	metrics.RecordOperation(inst.Name, "rollback", "ok")
	return textResult(fmt.Sprintf("rollback requested for %s/%s @ %s to history id %d (revision %s). Follow it with app_wait or app_get.",
		app.Namespace, app.Name, inst.Name, entry.ID, shortRev(entry.Revision))), nil, nil
}

func (s *Server) appTerminateOperation(ctx context.Context, _ *mcp.CallToolRequest, in appRef) (*mcp.CallToolResult, any, error) {
	inst, app, err := s.prepareWrite(ctx, in.Instance, in.Namespace, in.Name)
	if err != nil {
		return errorResult(err), nil, nil
	}
	if app.Operation == nil || !app.Operation.Running() {
		phase := "none"
		if app.Operation != nil {
			phase = dash(app.Operation.Phase)
		}
		metrics.RecordOperation(inst.Name, "terminate", "error")
		return errorResult(fmt.Errorf("no operation is running on %s/%s (phase=%s)", app.Namespace, app.Name, phase)), nil, nil
	}
	if _, err := argocd.PatchApp(ctx, inst.Dynamic, app.Namespace, app.Name, argocd.PatchType, argocd.TerminatePatch()); err != nil {
		metrics.RecordOperation(inst.Name, "terminate", "error")
		return errorResult(err), nil, nil
	}
	metrics.RecordOperation(inst.Name, "terminate", "ok")
	return textResult(fmt.Sprintf("termination requested for the running operation on %s/%s @ %s", app.Namespace, app.Name, inst.Name)), nil, nil
}
