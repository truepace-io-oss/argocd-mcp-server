package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// patches returns every patch action recorded on the fake client, decoded.
func patches(t *testing.T, actions []k8stesting.Action) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, a := range actions {
		p, ok := a.(k8stesting.PatchAction)
		if !ok || a.GetVerb() != "patch" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(p.GetPatch(), &m); err != nil {
			t.Fatalf("recorded patch is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestAppSyncHappyPath(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{
		appRef: appRef{Name: "nexus"}, Prune: true, Revision: "abc123",
		SyncOptions: []string{"ApplyOutOfSyncOnly=true"},
	})
	if res.IsError {
		t.Fatalf("sync should succeed: %s", text(t, res))
	}
	got := text(t, res)
	if !strings.Contains(got, "sync requested for argocd/nexus @ tools") || !strings.Contains(got, "prune") {
		t.Fatalf("summary wrong: %s", got)
	}

	ps := patches(t, dyn.Actions())
	if len(ps) != 1 {
		t.Fatalf("expected exactly one patch, got %d", len(ps))
	}
	op := ps[0]["operation"].(map[string]any)
	sync := op["sync"].(map[string]any)
	if sync["revision"] != "abc123" || sync["prune"] != true {
		t.Fatalf("sync payload wrong: %#v", sync)
	}
	if opts := sync["syncOptions"].([]any); opts[0] != "ApplyOutOfSyncOnly=true" {
		t.Fatalf("syncOptions wrong: %#v", opts)
	}
	if user := op["initiatedBy"].(map[string]any)["username"]; user != "argocd-mcp" {
		t.Fatalf("initiatedBy without auth should be the plain server name, got %v", user)
	}
}

func TestAppSyncDefaultsToTargetRevision(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	if res, _, _ := s.appSync(ctx(), nil, appSyncParam{appRef: appRef{Name: "nexus"}}); res.IsError {
		t.Fatalf("sync failed: %s", text(t, res))
	}
	sync := patches(t, dyn.Actions())[0]["operation"].(map[string]any)["sync"].(map[string]any)
	if sync["revision"] != "main" {
		t.Fatalf("revision should default to the app's targetRevision, got %v", sync["revision"])
	}
}

func TestAppSyncBlockedGlobalReadOnly(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	s.readOnly = true
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{appRef: appRef{Name: "nexus"}})
	if !res.IsError || !strings.Contains(text(t, res), "read-only") {
		t.Fatalf("expected the global read-only block, got: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("no patch must reach the API server when writes are disabled globally")
	}
	if got := classifyResult(res, nil); got != "blocked" {
		t.Fatalf("classifyResult = %q, want blocked", got)
	}
}

func TestAppSyncBlockedInstanceReadOnly(t *testing.T) {
	s, dyn := buildTestServer(t, true) // instance readOnly
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{appRef: appRef{Name: "nexus"}})
	if !res.IsError || !strings.Contains(text(t, res), "readOnly") {
		t.Fatalf("expected the per-instance read-only block, got: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("no patch must reach the API server for a read-only instance")
	}
}

func TestAppSyncRefusesConcurrentOperation(t *testing.T) {
	s, dyn := buildTestServer(t, false,
		newApp(appOpts{name: "busy", sync: "OutOfSync", health: "Progressing", opPhase: argocd.PhaseRunning}))
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{appRef: appRef{Name: "busy"}})
	if !res.IsError || !strings.Contains(text(t, res), "already in progress") {
		t.Fatalf("expected the in-progress guard, got: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "app_terminate_operation") {
		t.Fatalf("error should suggest the remedy: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("no patch must be sent while another operation runs")
	}
	if got := classifyResult(res, nil); got != "conflict" {
		t.Fatalf("classifyResult = %q, want conflict", got)
	}
}

func TestAppSyncRefusesQueuedOperation(t *testing.T) {
	s, _ := buildTestServer(t, false,
		newApp(appOpts{name: "queued", sync: "OutOfSync", health: "Healthy", pendingOp: true}))
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{appRef: appRef{Name: "queued"}})
	if !res.IsError || !strings.Contains(text(t, res), "queued but not started") {
		t.Fatalf("expected the queued-operation guard, got: %s", text(t, res))
	}
}

func TestAppSyncRejectsBadStrategy(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{appRef: appRef{Name: "nexus"}, Strategy: "magic"})
	if !res.IsError || !strings.Contains(text(t, res), "invalid sync strategy") {
		t.Fatalf("expected a strategy validation error, got: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("an invalid request must not be sent")
	}
}

func TestAppSyncPartialResources(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	res, _, _ := s.appSync(ctx(), nil, appSyncParam{
		appRef:    appRef{Name: "nexus"},
		Resources: []argocd.ResourceRef{{Group: "apps", Kind: "Deployment", Namespace: "nexus", Name: "core"}},
	})
	if res.IsError {
		t.Fatalf("partial sync failed: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "1 selected resource(s)") {
		t.Fatalf("summary should mention the selection: %s", text(t, res))
	}
	sync := patches(t, dyn.Actions())[0]["operation"].(map[string]any)["sync"].(map[string]any)
	r := sync["resources"].([]any)[0].(map[string]any)
	if r["kind"] != "Deployment" || r["name"] != "core" {
		t.Fatalf("resource selection wrong: %#v", r)
	}
}

func TestAppRefreshNoWait(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	no := false
	res, _, _ := s.appRefresh(ctx(), nil, appRefreshParam{appRef: appRef{Name: "nexus"}, Hard: true, Wait: &no})
	if res.IsError {
		t.Fatalf("refresh failed: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "hard refresh requested") {
		t.Fatalf("summary wrong: %s", text(t, res))
	}
	ann := patches(t, dyn.Actions())[0]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[argocd.AnnotationRefresh] != argocd.RefreshHard {
		t.Fatalf("refresh annotation wrong: %#v", ann)
	}
}

func TestAppRefreshWaitsForController(t *testing.T) {
	// The fake client keeps the annotation, but the controller is simulated by
	// clearing it on the second read.
	s, dyn := buildTestServer(t, false)
	reads := 0
	dyn.PrependReactor("get", "applications", func(action k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads < 3 { // 1 = prepareWrite, 2 = first poll (still annotated)
			return false, nil, nil
		}
		obj := newApp(appOpts{name: "nexus", project: "prod", sync: "Synced", health: "Healthy", revision: "abc"})
		return true, obj, nil
	})
	res, _, _ := s.appRefresh(ctx(), nil, appRefreshParam{appRef: appRef{Name: "nexus"}, TimeoutSeconds: 10})
	if res.IsError {
		t.Fatalf("refresh failed: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "completed") || !strings.Contains(text(t, res), "sync=Synced") {
		t.Fatalf("wait result wrong: %s", text(t, res))
	}
}

func TestAppRefreshBlockedReadOnly(t *testing.T) {
	s, dyn := buildTestServer(t, true)
	res, _, _ := s.appRefresh(ctx(), nil, appRefreshParam{appRef: appRef{Name: "nexus"}})
	if !res.IsError || !strings.Contains(text(t, res), "readOnly") {
		t.Fatalf("refresh must honour the read-only guard: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("no patch must reach the API server")
	}
}

func TestAppRollback(t *testing.T) {
	// nexus has auto-sync -> needs force; use a plain app instead.
	app := newApp(appOpts{name: "plain", sync: "Synced", health: "Healthy",
		history: []map[string]any{
			{"id": int64(1), "revision": "r1", "deployedAt": "2026-09-01T10:00:00Z",
				"source": map[string]any{"repoURL": "https://git/x.git", "path": "a"}},
			{"id": int64(2), "revision": "r2", "deployedAt": "2026-09-05T10:00:00Z"},
		}})
	s, dyn := buildTestServer(t, false, app)
	res, _, _ := s.appRollback(ctx(), nil, appRollbackParam{appRef: appRef{Name: "plain"}, ID: 1})
	if res.IsError {
		t.Fatalf("rollback failed: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "history id 1") {
		t.Fatalf("summary wrong: %s", text(t, res))
	}
	sync := patches(t, dyn.Actions())[0]["operation"].(map[string]any)["sync"].(map[string]any)
	if sync["revision"] != "r1" {
		t.Fatalf("rollback revision wrong: %#v", sync)
	}
	if src := sync["source"].(map[string]any); src["path"] != "a" {
		t.Fatalf("rollback source not taken from history: %#v", src)
	}
}

func TestAppRollbackUnknownID(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appRollback(ctx(), nil, appRollbackParam{appRef: appRef{Name: "nexus"}, ID: 99})
	if !res.IsError || !strings.Contains(text(t, res), "available ids: 1, 2") {
		t.Fatalf("expected the available-ids hint, got: %s", text(t, res))
	}
}

func TestAppRollbackAutoSyncGuard(t *testing.T) {
	s, dyn := buildTestServer(t, false) // nexus has automated sync with selfHeal
	res, _, _ := s.appRollback(ctx(), nil, appRollbackParam{appRef: appRef{Name: "nexus"}, ID: 1})
	if !res.IsError || !strings.Contains(text(t, res), "automated sync") {
		t.Fatalf("expected the auto-sync guard, got: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("guarded rollback must not patch")
	}

	res, _, _ = s.appRollback(ctx(), nil, appRollbackParam{appRef: appRef{Name: "nexus"}, ID: 1, Force: true})
	if res.IsError {
		t.Fatalf("force=true should override the guard: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 1 {
		t.Fatal("forced rollback should patch exactly once")
	}
}

func TestAppTerminateOperation(t *testing.T) {
	s, dyn := buildTestServer(t, false,
		newApp(appOpts{name: "busy", sync: "OutOfSync", health: "Progressing", opPhase: argocd.PhaseRunning}))
	res, _, _ := s.appTerminateOperation(ctx(), nil, appRef{Name: "busy"})
	if res.IsError {
		t.Fatalf("terminate failed: %s", text(t, res))
	}
	phase := patches(t, dyn.Actions())[0]["status"].(map[string]any)["operationState"].(map[string]any)["phase"]
	if phase != argocd.PhaseTerminating {
		t.Fatalf("terminate patch wrong: %v", phase)
	}
}

func TestAppTerminateWithoutRunningOperation(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	res, _, _ := s.appTerminateOperation(ctx(), nil, appRef{Name: "nexus"})
	if !res.IsError || !strings.Contains(text(t, res), "no operation is running") {
		t.Fatalf("expected a no-operation error, got: %s", text(t, res))
	}
	if len(patches(t, dyn.Actions())) != 0 {
		t.Fatal("nothing to terminate must not patch")
	}
}
