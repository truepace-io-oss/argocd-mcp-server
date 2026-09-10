package argocd

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// PatchType is the patch type used for every mutation (see PatchApp).
const PatchType = types.MergePatchType

// ResourceRef selects a single resource for a partial sync.
type ResourceRef struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// SyncRequest is the input for SyncPatch. It maps 1:1 onto Argo CD's
// Operation/SyncOperation types.
type SyncRequest struct {
	Revision    string
	Prune       bool
	DryRun      bool
	SyncOptions []string
	Resources   []ResourceRef
	Strategy    string // "" | apply | hook
	Force       bool   // only meaningful with the apply strategy
	RetryLimit  int
	InitiatedBy string
	Reason      string

	// Source / Sources override the application's own source; set by rollback.
	Source  map[string]any
	Sources []any
}

// SyncPatch builds the JSON merge patch that requests a sync — exactly the
// mutation the Argo CD API server performs for `argocd app sync`.
func SyncPatch(r SyncRequest) ([]byte, error) {
	switch r.Strategy {
	case "", StrategyApply, StrategyHook:
	default:
		return nil, fmt.Errorf("invalid sync strategy %q (want %q or %q)", r.Strategy, StrategyApply, StrategyHook)
	}
	if r.Force && r.Strategy != StrategyApply {
		return nil, fmt.Errorf("force is only supported with the %q sync strategy", StrategyApply)
	}
	if r.Revision == "" {
		return nil, fmt.Errorf("sync revision must not be empty")
	}
	for i, res := range r.Resources {
		if res.Kind == "" || res.Name == "" {
			return nil, fmt.Errorf("resources[%d]: kind and name are required", i)
		}
	}

	sync := map[string]any{"revision": r.Revision}
	if r.Prune {
		sync["prune"] = true
	}
	if r.DryRun {
		sync["dryRun"] = true
	}
	if len(r.SyncOptions) > 0 {
		sync["syncOptions"] = toAnySlice(r.SyncOptions)
	}
	if len(r.Resources) > 0 {
		list := make([]any, 0, len(r.Resources))
		for _, res := range r.Resources {
			m := map[string]any{"kind": res.Kind, "name": res.Name}
			if res.Group != "" {
				m["group"] = res.Group
			}
			if res.Namespace != "" {
				m["namespace"] = res.Namespace
			}
			list = append(list, m)
		}
		sync["resources"] = list
	}
	switch r.Strategy {
	case StrategyApply:
		apply := map[string]any{}
		if r.Force {
			apply["force"] = true
		}
		sync["syncStrategy"] = map[string]any{"apply": apply}
	case StrategyHook:
		sync["syncStrategy"] = map[string]any{"hook": map[string]any{}}
	}
	if r.Source != nil {
		sync["source"] = r.Source
	}
	if len(r.Sources) > 0 {
		sync["sources"] = r.Sources
	}

	initiator := r.InitiatedBy
	if initiator == "" {
		initiator = "argocd-mcp"
	}
	op := map[string]any{
		"sync":        sync,
		"initiatedBy": map[string]any{"username": initiator},
	}
	if r.Reason != "" {
		op["info"] = []any{map[string]any{"name": "Reason", "value": r.Reason}}
	}
	if r.RetryLimit > 0 {
		op["retry"] = map[string]any{
			"limit":   int64(r.RetryLimit),
			"backoff": map[string]any{"duration": "5s", "factor": int64(2), "maxDuration": "3m"},
		}
	}

	return json.Marshal(map[string]any{"operation": op})
}

// RefreshPatch builds the merge patch that sets the refresh annotation. The
// application controller removes the annotation again once it has reconciled.
func RefreshPatch(hard bool) []byte {
	v := RefreshNormal
	if hard {
		v = RefreshHard
	}
	// Hand-built: the value set is fixed and known-safe, and this keeps the
	// output byte-stable for the golden tests.
	return []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationRefresh, v))
}

// TerminatePatch builds the merge patch that asks the controller to terminate a
// running operation. The Application CRD has no status subresource, so this is a
// plain patch on the main resource.
func TerminatePatch() []byte {
	return []byte(`{"status":{"operationState":{"phase":"Terminating"}}}`)
}

// ErrOperationInProgress is returned by CheckNoOperationInProgress.
var ErrOperationInProgress = fmt.Errorf("another operation is already in progress")

// CheckNoOperationInProgress refuses to queue a second operation on an
// application that is already busy — the same guard the Argo CD API server
// applies, surfaced early with an actionable message.
func CheckNoOperationInProgress(a *App) error {
	if a.Operation.Running() {
		return fmt.Errorf("%w on %s/%s (phase=%s); wait for it or call app_terminate_operation first",
			ErrOperationInProgress, a.Namespace, a.Name, a.Operation.Phase)
	}
	if a.HasPendingOp {
		return fmt.Errorf("%w on %s/%s (an operation is queued but not started yet); wait for it or call app_terminate_operation first",
			ErrOperationInProgress, a.Namespace, a.Name)
	}
	return nil
}

// FindHistoryEntry returns the history entry with the given id, or an error
// listing the ids that do exist.
func FindHistoryEntry(a *App, id int64) (*HistoryEntry, error) {
	for i := range a.History {
		if a.History[i].ID == id {
			return &a.History[i], nil
		}
	}
	if len(a.History) == 0 {
		return nil, fmt.Errorf("application %s/%s has no sync history to roll back to", a.Namespace, a.Name)
	}
	ids := make([]string, 0, len(a.History))
	for _, h := range a.History {
		ids = append(ids, fmt.Sprintf("%d", h.ID))
	}
	return nil, fmt.Errorf("no history entry with id %d for %s/%s; available ids: %s",
		id, a.Namespace, a.Name, strings.Join(ids, ", "))
}

// DefaultRevision picks the revision to sync to when the caller did not give
// one: the application's own targetRevision, falling back to HEAD.
func DefaultRevision(a *App) string {
	if len(a.Sources) > 0 && a.Sources[0].TargetRevision != "" {
		return a.Sources[0].TargetRevision
	}
	return "HEAD"
}

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
