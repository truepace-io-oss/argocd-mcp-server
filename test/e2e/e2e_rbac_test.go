package e2e

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestE2ERBACReadOnlyTokenCannotSync proves the central claim: the same MCP
// code, given a read-only ServiceAccount, surfaces a Kubernetes 403 on a sync.
// There is no policy engine in the MCP — Kubernetes RBAC is the gate.
func TestE2ERBACReadOnlyTokenCannotSync(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-rbac-ro"
	ensureNamespace(t, cs, ns)
	seedApp(t, ns, "app", seedOpts{sync: "OutOfSync", health: "Healthy", revision: "old"})

	token := mintToken(t, cs, ns, "ro-sa")
	grantArgoRole(t, cs, ns, "ro-sa", "argocd-mcp-read-only", []string{"get", "list", "watch"})

	// The MCP instance itself is NOT read-only, so the guard is not what blocks
	// the write — RBAC is.
	sess := startMCP(t, "envtest", ns, token, false, false)

	if out, isErr := callText(t, sess, "apps_list", map[string]any{}); isErr {
		t.Fatalf("read should succeed with a read-only role: %s", out)
	}

	out, isErr := callText(t, sess, "app_sync", map[string]any{"name": "app"})
	if !isErr {
		t.Fatalf("expected an RBAC-denied sync, got success: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "forbidden") {
		t.Fatalf("expected Forbidden in the error, got: %s", out)
	}
	if _, found, _ := unstructured.NestedMap(getApp(t, ns, "app").Object, "operation"); found {
		t.Fatal("RBAC-denied sync must not have written an operation")
	}
}

// TestE2ERBACNoArgoAccessAtAll proves the reachability probe reports a useful
// error when the ServiceAccount has no argoproj.io permissions at all.
func TestE2ERBACWithoutAnyRole(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-rbac-none"
	ensureNamespace(t, cs, ns)
	token := mintToken(t, cs, ns, "no-role-sa") // deliberately no Role/RoleBinding

	sess := startMCP(t, "envtest", ns, token, false, false)

	out, isErr := callText(t, sess, "instances_list", map[string]any{})
	if isErr {
		t.Fatalf("instances_list must not fail hard on an unreachable instance: %s", out)
	}
	mustContain(t, out, "UNREACHABLE", "instances_list")
	mustContain(t, out, "forbidden", "instances_list")
}

// TestE2EPerInstanceReadOnlyGuard proves the defense-in-depth guard: even with a
// sync-capable ServiceAccount, an instance flagged readOnly refuses writes
// *before* hitting the API server.
func TestE2EPerInstanceReadOnlyGuard(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-guard"
	ensureNamespace(t, cs, ns)
	seedApp(t, ns, "app", seedOpts{sync: "OutOfSync", health: "Healthy", revision: "old"})

	token := mintToken(t, cs, ns, "guard-sa")
	grantArgoRole(t, cs, ns, "guard-sa", "argocd-mcp-sync-guard", syncVerbs)

	sess := startMCP(t, "envtest", ns, token, true /* instance readOnly */, false)

	out, isErr := callText(t, sess, "app_sync", map[string]any{"name": "app"})
	if !isErr || !strings.Contains(out, "readOnly") {
		t.Fatalf("expected the per-instance readOnly guard, got isErr=%v: %s", isErr, out)
	}
	if _, found, _ := unstructured.NestedMap(getApp(t, ns, "app").Object, "operation"); found {
		t.Fatal("guard failed: an operation was written despite readOnly")
	}
	// Reads still work.
	if out, isErr := callText(t, sess, "apps_list", map[string]any{}); isErr {
		t.Fatalf("reads must still work on a read-only instance: %s", out)
	}
}

// TestE2EGlobalReadOnlyGuard proves the global kill-switch.
func TestE2EGlobalReadOnlyGuard(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-guard-global"
	ensureNamespace(t, cs, ns)
	seedApp(t, ns, "app", seedOpts{sync: "OutOfSync", health: "Healthy", revision: "old"})

	token := mintToken(t, cs, ns, "guard-global-sa")
	grantArgoRole(t, cs, ns, "guard-global-sa", "argocd-mcp-sync-global", syncVerbs)

	sess := startMCP(t, "envtest", ns, token, false, true /* global readOnly */)

	out, isErr := callText(t, sess, "app_refresh", map[string]any{"name": "app"})
	if !isErr || !strings.Contains(out, "read-only") {
		t.Fatalf("expected the global read-only block, got isErr=%v: %s", isErr, out)
	}
	if ann := getApp(t, ns, "app").GetAnnotations()["argocd.argoproj.io/refresh"]; ann != "" {
		t.Fatalf("global guard failed: refresh annotation %q was written", ann)
	}
}
