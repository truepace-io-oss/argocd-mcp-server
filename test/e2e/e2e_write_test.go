package e2e

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// syncVerbs is the RBAC the argocd-mcp-agent prototype grants when allowSync is
// true: read plus `patch` on applications. `update` is deliberately absent —
// every mutation is a JSON merge patch.
var syncVerbs = []string{"get", "list", "watch", "patch"}

func TestE2EWriteFlow(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-write"
	ensureNamespace(t, cs, ns)
	seedApp(t, ns, "app", seedOpts{sync: "OutOfSync", health: "Healthy", revision: "old",
		history: []any{
			map[string]any{"id": int64(1), "revision": "rev-one", "deployedAt": "2026-09-01T10:00:00Z",
				"source": map[string]any{"repoURL": "https://git/x.git", "path": "a"}},
		}})

	token := mintToken(t, cs, ns, "mcp-writer")
	grantArgoRole(t, cs, ns, "mcp-writer", "argocd-mcp-sync", syncVerbs)
	sess := startMCP(t, "envtest", ns, token, false, false)

	// --- app_sync writes the operation Argo CD's own API server would write ---
	out, isErr := callText(t, sess, "app_sync", map[string]any{
		"name": "app", "revision": "abc123", "prune": true,
		"syncOptions": []any{"ApplyOutOfSyncOnly=true"},
	})
	if isErr {
		t.Fatalf("app_sync failed: %s", out)
	}
	mustContain(t, out, "sync requested for "+ns+"/app", "app_sync")

	obj := getApp(t, ns, "app")
	rev, found, _ := unstructured.NestedString(obj.Object, "operation", "sync", "revision")
	if !found || rev != "abc123" {
		t.Fatalf("operation.sync.revision = %q (found=%v)", rev, found)
	}
	if prune, _, _ := unstructured.NestedBool(obj.Object, "operation", "sync", "prune"); !prune {
		t.Fatal("operation.sync.prune was not written")
	}
	opts, _, _ := unstructured.NestedStringSlice(obj.Object, "operation", "sync", "syncOptions")
	if len(opts) != 1 || opts[0] != "ApplyOutOfSyncOnly=true" {
		t.Fatalf("operation.sync.syncOptions = %v", opts)
	}
	user, _, _ := unstructured.NestedString(obj.Object, "operation", "initiatedBy", "username")
	if !strings.HasPrefix(user, "argocd-mcp") {
		t.Fatalf("operation.initiatedBy.username = %q, want an argocd-mcp identity", user)
	}

	// --- a second sync is refused while an operation is queued ---
	out, isErr = callText(t, sess, "app_sync", map[string]any{"name": "app"})
	if !isErr || !strings.Contains(out, "already in progress") {
		t.Fatalf("expected the in-progress guard, got isErr=%v: %s", isErr, out)
	}

	// Clear the queued operation (in production the controller does this).
	clearOperation(t, ns, "app")

	// --- app_refresh writes the annotation the controller watches ---
	out, isErr = callText(t, sess, "app_refresh", map[string]any{"name": "app", "hard": true, "wait": false})
	if isErr {
		t.Fatalf("app_refresh failed: %s", out)
	}
	mustContain(t, out, "hard refresh requested", "app_refresh")
	obj = getApp(t, ns, "app")
	if got := obj.GetAnnotations()["argocd.argoproj.io/refresh"]; got != "hard" {
		t.Fatalf("refresh annotation = %q, want hard", got)
	}

	// --- app_rollback uses the history entry's revision and source ---
	out, isErr = callText(t, sess, "app_rollback", map[string]any{"name": "app", "id": 1})
	if isErr {
		t.Fatalf("app_rollback failed: %s", out)
	}
	mustContain(t, out, "history id 1", "app_rollback")
	obj = getApp(t, ns, "app")
	rev, _, _ = unstructured.NestedString(obj.Object, "operation", "sync", "revision")
	if rev != "rev-one" {
		t.Fatalf("rollback revision = %q, want rev-one", rev)
	}
	path, _, _ := unstructured.NestedString(obj.Object, "operation", "sync", "source", "path")
	if path != "a" {
		t.Fatalf("rollback did not carry the historical source: %q", path)
	}
}

func TestE2ETerminateOperation(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-terminate"
	ensureNamespace(t, cs, ns)
	seedApp(t, ns, "busy", seedOpts{sync: "OutOfSync", health: "Progressing"})
	setOperationPhase(t, ns, "busy", "Running")

	token := mintToken(t, cs, ns, "mcp-writer-t")
	grantArgoRole(t, cs, ns, "mcp-writer-t", "argocd-mcp-sync-t", syncVerbs)
	sess := startMCP(t, "envtest", ns, token, false, false)

	out, isErr := callText(t, sess, "app_terminate_operation", map[string]any{"name": "busy"})
	if isErr {
		t.Fatalf("terminate failed: %s", out)
	}
	obj := getApp(t, ns, "busy")
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "operationState", "phase")
	if phase != "Terminating" {
		t.Fatalf("status.operationState.phase = %q, want Terminating", phase)
	}
}

func TestE2ERollbackAutoSyncGuard(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-autosync"
	ensureNamespace(t, cs, ns)
	seedApp(t, ns, "auto", seedOpts{sync: "Synced", health: "Healthy", autoSync: true,
		history: []any{map[string]any{"id": int64(1), "revision": "rev-one", "deployedAt": "2026-09-01T10:00:00Z"}}})

	token := mintToken(t, cs, ns, "mcp-writer-a")
	grantArgoRole(t, cs, ns, "mcp-writer-a", "argocd-mcp-sync-a", syncVerbs)
	sess := startMCP(t, "envtest", ns, token, false, false)

	out, isErr := callText(t, sess, "app_rollback", map[string]any{"name": "auto", "id": 1})
	if !isErr || !strings.Contains(out, "automated sync") {
		t.Fatalf("expected the auto-sync guard, got isErr=%v: %s", isErr, out)
	}
	if _, found, _ := unstructured.NestedMap(getApp(t, ns, "auto").Object, "operation"); found {
		t.Fatal("guarded rollback must not have written an operation")
	}

	out, isErr = callText(t, sess, "app_rollback", map[string]any{"name": "auto", "id": 1, "force": true})
	if isErr {
		t.Fatalf("force=true should override the guard: %s", out)
	}
	if _, found, _ := unstructured.NestedMap(getApp(t, ns, "auto").Object, "operation"); !found {
		t.Fatal("forced rollback should have written an operation")
	}
}

// clearOperation removes the top-level operation field, simulating the
// application controller picking the operation up.
func clearOperation(t *testing.T, ns, name string) {
	t.Helper()
	obj := getApp(t, ns, name)
	unstructured.RemoveNestedField(obj.Object, "operation")
	updateApp(t, ns, obj)
}

// setOperationPhase writes a complete status.operationState — the CRD requires
// `operation` and `startedAt` alongside `phase`.
func setOperationPhase(t *testing.T, ns, name, phase string) {
	t.Helper()
	obj := getApp(t, ns, name)
	state := map[string]any{
		"phase":     phase,
		"startedAt": "2026-09-08T07:00:00Z",
		"operation": map[string]any{
			"sync":        map[string]any{"revision": "HEAD"},
			"initiatedBy": map[string]any{"username": "admin"},
		},
	}
	if err := unstructured.SetNestedMap(obj.Object, state, "status", "operationState"); err != nil {
		t.Fatalf("set operationState: %v", err)
	}
	updateApp(t, ns, obj)
}
