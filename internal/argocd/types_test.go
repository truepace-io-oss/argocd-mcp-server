package argocd

import (
	"encoding/json"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func loadFixture(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

func TestAppFromUnstructured(t *testing.T) {
	a := AppFromUnstructured(loadFixture(t, "testdata/app.json"))

	if a.Namespace != "argocd" || a.Name != "web-api" {
		t.Fatalf("identity wrong: %s/%s", a.Namespace, a.Name)
	}
	if a.Project != "tools" {
		t.Fatalf("project = %q", a.Project)
	}
	if len(a.Sources) != 1 || a.Sources[0].TargetRevision != "main" {
		t.Fatalf("sources wrong: %#v", a.Sources)
	}
	if got := a.Destination(); got != "in-cluster/web-api" {
		t.Fatalf("destination = %q", got)
	}
	if got := a.AutoSyncLabel(); got != "true(selfHeal,prune)" {
		t.Fatalf("autoSyncLabel = %q", got)
	}
	if a.SyncStatus != "OutOfSync" || a.SyncRevision != "8f3c1ad" {
		t.Fatalf("sync wrong: %s %s", a.SyncStatus, a.SyncRevision)
	}
	if a.HealthStatus != "Degraded" || a.HealthMessage != "pod crash-looping" {
		t.Fatalf("health wrong: %s %s", a.HealthStatus, a.HealthMessage)
	}
	if a.ReconciledAt == nil || a.ReconciledAt.Year() != 2026 {
		t.Fatalf("reconciledAt = %v", a.ReconciledAt)
	}
	if !a.HasPendingOp {
		t.Fatal("HasPendingOp should be true — the object has a top-level operation")
	}
	if a.Operation == nil || a.Operation.Phase != PhaseRunning || !a.Operation.Running() {
		t.Fatalf("operationState wrong: %#v", a.Operation)
	}
	if a.Operation.InitiatedBy != "argocd-mcp:daniel" || a.Operation.Revision != "8f3c1ad" {
		t.Fatalf("operationState details wrong: %#v", a.Operation)
	}
	if a.RefreshRequested != RefreshNormal {
		t.Fatalf("refresh annotation = %q", a.RefreshRequested)
	}
	if len(a.Resources) != 3 {
		t.Fatalf("resources = %d, want 3", len(a.Resources))
	}
	if !a.Resources[0].Drifted() || a.Resources[1].Drifted() || !a.Resources[2].Drifted() {
		t.Fatalf("drift detection wrong: %#v", a.Resources)
	}
	if a.Resources[0].SyncWave != 1 {
		t.Fatalf("syncWave = %d", a.Resources[0].SyncWave)
	}
	// History is normalised to oldest-first.
	if len(a.History) != 2 || a.History[0].ID != 1 || a.History[1].ID != 3 {
		t.Fatalf("history not sorted by id: %#v", a.History)
	}
	if a.History[1].Source["path"] != "a" || a.History[1].InitiatedBy != "admin" {
		t.Fatalf("history details wrong: %#v", a.History[1])
	}
	if len(a.Conditions) != 1 || a.Conditions[0].Type != "SyncError" {
		t.Fatalf("conditions wrong: %#v", a.Conditions)
	}
	if a.Labels["env"] != "prod" {
		t.Fatalf("labels wrong: %#v", a.Labels)
	}
}

// A freshly created Application has no status at all — projection must not panic.
func TestAppFromUnstructuredBare(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "new", "namespace": "argocd"},
		"spec":       map[string]any{"project": "default"},
	}}
	a := AppFromUnstructured(u)
	if a.Name != "new" || a.SyncStatus != "" || a.Operation != nil || a.HasPendingOp {
		t.Fatalf("bare app projected wrong: %#v", a)
	}
	if a.Operation.Running() {
		t.Fatal("nil operation must not be reported as running")
	}
	if got := a.AutoSyncLabel(); got != "false" {
		t.Fatalf("autoSyncLabel = %q, want false", got)
	}
	if got := a.Destination(); got != "?/" {
		t.Fatalf("destination = %q", got)
	}
	if got := DefaultRevision(a); got != "HEAD" {
		t.Fatalf("DefaultRevision = %q", got)
	}
}

func TestAppFromUnstructuredMultiSource(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "multi", "namespace": "argocd"},
		"spec": map[string]any{
			"sources": []any{
				map[string]any{"repoURL": "https://git/a.git", "path": "a", "targetRevision": "v1"},
				map[string]any{"repoURL": "https://charts", "chart": "nginx", "targetRevision": "1.2.3"},
			},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "web"},
			"syncPolicy":  map[string]any{"automated": map[string]any{}},
		},
	}}
	a := AppFromUnstructured(u)
	if len(a.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(a.Sources))
	}
	if got := a.Sources[0].Location(); got != "a" {
		t.Fatalf("location(path) = %q", got)
	}
	if got := a.Sources[1].Location(); got != "chart:nginx" {
		t.Fatalf("location(chart) = %q", got)
	}
	if got := a.Destination(); got != "https://kubernetes.default.svc/web" {
		t.Fatalf("destination = %q", got)
	}
	if got := a.AutoSyncLabel(); got != "true" {
		t.Fatalf("autoSyncLabel = %q, want plain true", got)
	}
	if got := DefaultRevision(a); got != "v1" {
		t.Fatalf("DefaultRevision = %q, want v1", got)
	}
}

func TestSourceLocationFallback(t *testing.T) {
	if got := (Source{}).Location(); got != "." {
		t.Fatalf("empty source location = %q", got)
	}
}

func TestProjectFromUnstructured(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "prod", "namespace": "argocd"},
		"spec": map[string]any{
			"description": "production",
			"sourceRepos": []any{"https://git.example.com/org/gitops.git"},
			"destinations": []any{
				map[string]any{"name": "in-cluster", "namespace": "*"},
				map[string]any{"server": "https://api", "namespace": "web"},
			},
			"clusterResourceWhitelist": []any{map[string]any{"group": "*", "kind": "*"}},
		},
	}}
	p := ProjectFromUnstructured(u)
	if p.Name != "prod" || p.Description != "production" || len(p.SourceRepos) != 1 {
		t.Fatalf("project projected wrong: %#v", p)
	}
	if len(p.Destinations) != 2 || p.Destinations[0] != "in-cluster/*" || p.Destinations[1] != "https://api/web" {
		t.Fatalf("destinations wrong: %#v", p.Destinations)
	}
	if !p.ClusterResourceAll {
		t.Fatal("clusterResourceWhitelist should be detected")
	}
}

func TestAppSetFromUnstructured(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "gen", "namespace": "argocd"},
		"spec": map[string]any{
			"generators": []any{map[string]any{"list": map[string]any{}}, map[string]any{"git": map[string]any{}}},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "ErrorOccurred", "message": "nope"}},
		},
	}}
	s := AppSetFromUnstructured(u)
	if len(s.Generators) != 2 || s.Generators[0] != "list" || s.Generators[1] != "git" {
		t.Fatalf("generators wrong: %#v", s.Generators)
	}
	if len(s.Conditions) != 1 || s.Conditions[0].Type != "ErrorOccurred" {
		t.Fatalf("conditions wrong: %#v", s.Conditions)
	}
}
