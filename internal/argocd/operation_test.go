package argocd

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustPatch(t *testing.T, r SyncRequest) map[string]any {
	t.Helper()
	raw, err := SyncPatch(r)
	if err != nil {
		t.Fatalf("SyncPatch: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("patch is not valid JSON: %v (%s)", err, raw)
	}
	return got
}

func TestSyncPatchMinimal(t *testing.T) {
	got := mustPatch(t, SyncRequest{Revision: "HEAD"})
	want := map[string]any{
		"operation": map[string]any{
			"sync":        map[string]any{"revision": "HEAD"},
			"initiatedBy": map[string]any{"username": "argocd-mcp"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("minimal patch =\n%#v\nwant\n%#v", got, want)
	}
}

func TestSyncPatchFullOptions(t *testing.T) {
	got := mustPatch(t, SyncRequest{
		Revision:    "abc123",
		Prune:       true,
		DryRun:      true,
		SyncOptions: []string{"ApplyOutOfSyncOnly=true", "Validate=false"},
		Resources:   []ResourceRef{{Group: "apps", Kind: "Deployment", Namespace: "web", Name: "api"}},
		Strategy:    StrategyApply,
		Force:       true,
		RetryLimit:  3,
		InitiatedBy: "argocd-mcp:daniel",
		Reason:      "requested via argocd-mcp",
	})
	op := got["operation"].(map[string]any)
	if op["initiatedBy"].(map[string]any)["username"] != "argocd-mcp:daniel" {
		t.Fatalf("initiatedBy not propagated: %#v", op)
	}
	sync := op["sync"].(map[string]any)
	if sync["revision"] != "abc123" || sync["prune"] != true || sync["dryRun"] != true {
		t.Fatalf("sync flags wrong: %#v", sync)
	}
	if opts := sync["syncOptions"].([]any); len(opts) != 2 || opts[0] != "ApplyOutOfSyncOnly=true" {
		t.Fatalf("syncOptions wrong: %#v", opts)
	}
	res := sync["resources"].([]any)[0].(map[string]any)
	if res["group"] != "apps" || res["kind"] != "Deployment" || res["namespace"] != "web" || res["name"] != "api" {
		t.Fatalf("resources wrong: %#v", res)
	}
	if force := sync["syncStrategy"].(map[string]any)["apply"].(map[string]any)["force"]; force != true {
		t.Fatalf("force not set: %#v", sync["syncStrategy"])
	}
	if limit := op["retry"].(map[string]any)["limit"]; limit != float64(3) {
		t.Fatalf("retry limit wrong: %#v", op["retry"])
	}
	if info := op["info"].([]any)[0].(map[string]any); info["name"] != "Reason" {
		t.Fatalf("info wrong: %#v", info)
	}
}

func TestSyncPatchOmitsFalseFlags(t *testing.T) {
	sync := mustPatch(t, SyncRequest{Revision: "HEAD"})["operation"].(map[string]any)["sync"].(map[string]any)
	for _, k := range []string{"prune", "dryRun", "syncOptions", "resources", "syncStrategy", "source", "sources"} {
		if _, ok := sync[k]; ok {
			t.Fatalf("key %q should be omitted when unset: %#v", k, sync)
		}
	}
}

func TestSyncPatchHookStrategy(t *testing.T) {
	sync := mustPatch(t, SyncRequest{Revision: "HEAD", Strategy: StrategyHook})["operation"].(map[string]any)["sync"].(map[string]any)
	if _, ok := sync["syncStrategy"].(map[string]any)["hook"]; !ok {
		t.Fatalf("hook strategy missing: %#v", sync["syncStrategy"])
	}
}

func TestSyncPatchRollbackSource(t *testing.T) {
	src := map[string]any{"repoURL": "https://git/x.git", "path": "app", "targetRevision": "v1"}
	sync := mustPatch(t, SyncRequest{Revision: "deadbeef", Source: src})["operation"].(map[string]any)["sync"].(map[string]any)
	if !reflect.DeepEqual(sync["source"], src) {
		t.Fatalf("rollback source not propagated: %#v", sync["source"])
	}
}

func TestSyncPatchErrors(t *testing.T) {
	cases := []struct {
		name string
		req  SyncRequest
		want string
	}{
		{"empty revision", SyncRequest{}, "revision must not be empty"},
		{"bad strategy", SyncRequest{Revision: "HEAD", Strategy: "magic"}, "invalid sync strategy"},
		{"force without apply", SyncRequest{Revision: "HEAD", Force: true}, "force is only supported"},
		{"force with hook", SyncRequest{Revision: "HEAD", Strategy: StrategyHook, Force: true}, "force is only supported"},
		{"resource without kind", SyncRequest{Revision: "HEAD", Resources: []ResourceRef{{Name: "x"}}}, "kind and name are required"},
		{"resource without name", SyncRequest{Revision: "HEAD", Resources: []ResourceRef{{Kind: "Service"}}}, "kind and name are required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SyncPatch(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("SyncPatch error = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestRefreshPatch(t *testing.T) {
	for _, tc := range []struct {
		hard bool
		want string
	}{{false, RefreshNormal}, {true, RefreshHard}} {
		var m map[string]any
		if err := json.Unmarshal(RefreshPatch(tc.hard), &m); err != nil {
			t.Fatalf("refresh patch invalid: %v", err)
		}
		ann := m["metadata"].(map[string]any)["annotations"].(map[string]any)
		if ann[AnnotationRefresh] != tc.want {
			t.Fatalf("refresh annotation = %v, want %q", ann[AnnotationRefresh], tc.want)
		}
	}
}

func TestTerminatePatch(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(TerminatePatch(), &m); err != nil {
		t.Fatalf("terminate patch invalid: %v", err)
	}
	phase := m["status"].(map[string]any)["operationState"].(map[string]any)["phase"]
	if phase != PhaseTerminating {
		t.Fatalf("phase = %v, want %q", phase, PhaseTerminating)
	}
}

func TestCheckNoOperationInProgress(t *testing.T) {
	cases := []struct {
		name    string
		app     App
		wantErr bool
	}{
		{"idle", App{Namespace: "argocd", Name: "a"}, false},
		{"succeeded", App{Namespace: "argocd", Name: "a", Operation: &OperationState{Phase: PhaseSucceeded}}, false},
		{"failed", App{Namespace: "argocd", Name: "a", Operation: &OperationState{Phase: PhaseFailed}}, false},
		{"running", App{Namespace: "argocd", Name: "a", Operation: &OperationState{Phase: PhaseRunning}}, true},
		{"terminating", App{Namespace: "argocd", Name: "a", Operation: &OperationState{Phase: PhaseTerminating}}, true},
		{"queued", App{Namespace: "argocd", Name: "a", HasPendingOp: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckNoOperationInProgress(&c.app)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err != nil && !errors.Is(err, ErrOperationInProgress) {
				t.Fatalf("error should wrap ErrOperationInProgress: %v", err)
			}
		})
	}
}

func TestFindHistoryEntry(t *testing.T) {
	app := &App{Namespace: "argocd", Name: "a", History: []HistoryEntry{{ID: 1, Revision: "r1"}, {ID: 3, Revision: "r3"}}}
	e, err := FindHistoryEntry(app, 3)
	if err != nil || e.Revision != "r3" {
		t.Fatalf("FindHistoryEntry(3) = %v, %v", e, err)
	}
	_, err = FindHistoryEntry(app, 2)
	if err == nil || !strings.Contains(err.Error(), "available ids: 1, 3") {
		t.Fatalf("expected an error listing available ids, got %v", err)
	}
	_, err = FindHistoryEntry(&App{Namespace: "argocd", Name: "b"}, 1)
	if err == nil || !strings.Contains(err.Error(), "no sync history") {
		t.Fatalf("expected a no-history error, got %v", err)
	}
}

func TestDefaultRevision(t *testing.T) {
	if got := DefaultRevision(&App{}); got != "HEAD" {
		t.Fatalf("DefaultRevision(no source) = %q, want HEAD", got)
	}
	app := &App{Sources: []Source{{TargetRevision: "main"}}}
	if got := DefaultRevision(app); got != "main" {
		t.Fatalf("DefaultRevision = %q, want main", got)
	}
}
