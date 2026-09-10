package e2e

import (
	"strings"
	"testing"
)

func TestE2EReadFlow(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-read"
	ensureNamespace(t, cs, ns)
	seedProject(t, ns, "default")

	seedApp(t, ns, "healthy-app", seedOpts{sync: "Synced", health: "Healthy", revision: "1111111111111111111111111111111111111111"})
	seedApp(t, ns, "drifted-app", seedOpts{
		sync: "OutOfSync", health: "Degraded", revision: "2222222222222222222222222222222222222222",
		resources: []any{
			map[string]any{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": ns, "name": "api",
				"status": "OutOfSync", "health": map[string]any{"status": "Degraded", "message": "CrashLoopBackOff"}},
			map[string]any{"version": "v1", "kind": "Service", "namespace": ns, "name": "api",
				"status": "Synced", "health": map[string]any{"status": "Healthy"}},
		},
		history: []any{
			map[string]any{"id": int64(1), "revision": "aaa", "deployedAt": "2026-09-01T10:00:00Z"},
			map[string]any{"id": int64(2), "revision": "bbb", "deployedAt": "2026-09-05T10:00:00Z"},
		},
	})

	token := mintToken(t, cs, ns, "mcp-reader")
	grantArgoRole(t, cs, ns, "mcp-reader", "argocd-mcp-read", []string{"get", "list", "watch"})

	sess := startMCP(t, "envtest", ns, token, false, false)

	// instances_list proves the Argo CD API surface is reachable, not just the cluster.
	out, isErr := callText(t, sess, "instances_list", map[string]any{})
	if isErr {
		t.Fatalf("instances_list failed: %s", out)
	}
	mustContain(t, out, "envtest (default)", "instances_list")
	mustContain(t, out, "reachable", "instances_list")

	// apps_list shows both applications with their buckets.
	out, isErr = callText(t, sess, "apps_list", map[string]any{})
	if isErr {
		t.Fatalf("apps_list failed: %s", out)
	}
	mustContain(t, out, "2 application(s)", "apps_list")
	mustContain(t, out, ns+"/healthy-app", "apps_list")
	mustContain(t, out, ns+"/drifted-app", "apps_list")
	mustContain(t, out, "OutOfSync 1, Synced 1", "apps_list histogram")

	// Filters narrow it down.
	out, _ = callText(t, sess, "apps_list", map[string]any{"syncStatus": "OutOfSync"})
	mustContain(t, out, "1 application(s)", "apps_list filtered")
	if strings.Contains(out, "healthy-app") {
		t.Fatalf("filter leaked a synced app: %s", out)
	}

	// app_get renders the full detail.
	out, isErr = callText(t, sess, "app_get", map[string]any{"name": "drifted-app"})
	if isErr {
		t.Fatalf("app_get failed: %s", out)
	}
	mustContain(t, out, "sync:        OutOfSync", "app_get")
	mustContain(t, out, "destination: in-cluster/drifted-app", "app_get")
	mustContain(t, out, "resources:   2 managed, 1 drifted", "app_get")

	// app_resources is the drift summary.
	out, isErr = callText(t, sess, "app_resources", map[string]any{"name": "drifted-app"})
	if isErr {
		t.Fatalf("app_resources failed: %s", out)
	}
	mustContain(t, out, "1 drifted resource(s) of 2 managed", "app_resources")
	mustContain(t, out, "apps/Deployment "+ns+"/api", "app_resources")
	mustContain(t, out, "CrashLoopBackOff", "app_resources")

	// app_history feeds app_rollback.
	out, isErr = callText(t, sess, "app_history", map[string]any{"name": "drifted-app"})
	if isErr {
		t.Fatalf("app_history failed: %s", out)
	}
	mustContain(t, out, "id=1 revision=aaa", "app_history")
	mustContain(t, out, "id=2 revision=bbb", "app_history")

	// projects_list reads AppProjects through the same credentials.
	out, isErr = callText(t, sess, "projects_list", map[string]any{})
	if isErr {
		t.Fatalf("projects_list failed: %s", out)
	}
	mustContain(t, out, ns+"/default", "projects_list")

	// appsets_list works even when there are none.
	out, isErr = callText(t, sess, "appsets_list", map[string]any{})
	if isErr {
		t.Fatalf("appsets_list failed: %s", out)
	}
	mustContain(t, out, "0 ApplicationSet(s)", "appsets_list")

	// app_wait returns immediately for an already-healthy application.
	out, isErr = callText(t, sess, "app_wait", map[string]any{"name": "healthy-app", "timeoutSeconds": 5})
	if isErr {
		t.Fatalf("app_wait failed: %s", out)
	}
	mustContain(t, out, "reached sync=Synced health=Healthy", "app_wait")
}

func TestE2EUnknownInstanceAndApp(t *testing.T) {
	cs := adminClient(t)
	ns := "e2e-notfound"
	ensureNamespace(t, cs, ns)
	token := mintToken(t, cs, ns, "mcp-reader-nf")
	grantArgoRole(t, cs, ns, "mcp-reader-nf", "argocd-mcp-read-nf", []string{"get", "list", "watch"})
	sess := startMCP(t, "envtest", ns, token, false, false)

	out, isErr := callText(t, sess, "apps_list", map[string]any{"instance": "ghost"})
	if !isErr || !strings.Contains(out, "unknown Argo CD instance") {
		t.Fatalf("expected an unknown-instance tool error, got isErr=%v: %s", isErr, out)
	}
	out, isErr = callText(t, sess, "app_get", map[string]any{"name": "ghost"})
	if !isErr || !strings.Contains(out, "not found") {
		t.Fatalf("expected a not-found tool error, got isErr=%v: %s", isErr, out)
	}
}
