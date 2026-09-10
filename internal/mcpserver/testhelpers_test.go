package mcpserver

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// listKinds maps the Argo CD GVRs to their list kinds; the fake dynamic client
// needs this because it has no discovery.
var listKinds = map[schema.GroupVersionResource]string{
	argocd.AppGVR:     "ApplicationList",
	argocd.ProjectGVR: "AppProjectList",
	argocd.AppSetGVR:  "ApplicationSetList",
}

func newFakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
}

// appOpts describes the seeded Application fixtures.
type appOpts struct {
	namespace   string
	name        string
	project     string
	sync        string
	health      string
	revision    string
	autoSync    bool
	selfHeal    bool
	opPhase     string
	pendingOp   bool
	resources   []map[string]any
	history     []map[string]any
	labels      map[string]string
	appSetOwner string
}

func newApp(o appOpts) *unstructured.Unstructured {
	if o.namespace == "" {
		o.namespace = "argocd"
	}
	meta := map[string]any{"name": o.name, "namespace": o.namespace}
	if o.labels != nil {
		labels := map[string]any{}
		for k, v := range o.labels {
			labels[k] = v
		}
		meta["labels"] = labels
	}
	if o.appSetOwner != "" {
		meta["ownerReferences"] = []any{map[string]any{
			"apiVersion": "argoproj.io/v1alpha1",
			"kind":       "ApplicationSet",
			"name":       o.appSetOwner,
			"uid":        "u-" + o.appSetOwner,
		}}
	}

	spec := map[string]any{
		"project": o.project,
		"source": map[string]any{
			"repoURL":        "https://git.example.com/org/gitops.git",
			"path":           "rendered/envs/x/" + o.name,
			"targetRevision": "main",
		},
		"destination": map[string]any{"name": "in-cluster", "namespace": o.name},
	}
	if o.autoSync {
		spec["syncPolicy"] = map[string]any{"automated": map[string]any{"selfHeal": o.selfHeal, "prune": true}}
	}

	status := map[string]any{
		"sync":         map[string]any{"status": o.sync, "revision": o.revision},
		"health":       map[string]any{"status": o.health},
		"reconciledAt": "2026-09-08T07:00:00Z",
	}
	if o.resources != nil {
		res := make([]any, 0, len(o.resources))
		for _, r := range o.resources {
			res = append(res, r)
		}
		status["resources"] = res
	}
	if o.history != nil {
		h := make([]any, 0, len(o.history))
		for _, e := range o.history {
			h = append(h, e)
		}
		status["history"] = h
	}
	if o.opPhase != "" {
		status["operationState"] = map[string]any{"phase": o.opPhase, "message": "in flight"}
	}

	obj := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   meta,
		"spec":       spec,
		"status":     status,
	}
	if o.pendingOp {
		obj["operation"] = map[string]any{"sync": map[string]any{"revision": "HEAD"}}
	}
	return &unstructured.Unstructured{Object: obj}
}

func newProject(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "AppProject",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"description":  "project " + name,
			"sourceRepos":  []any{"https://git.example.com/org/gitops.git"},
			"destinations": []any{map[string]any{"name": "in-cluster", "namespace": "*"}},
		},
	}}
}

func newAppSet(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "ApplicationSet",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"generators": []any{map[string]any{"list": map[string]any{}}}},
	}}
}

// seededObjects is the standard fixture set used by the read-tool tests.
func seededObjects() []runtime.Object {
	return []runtime.Object{
		newApp(appOpts{
			name: "nexus", project: "prod", sync: "OutOfSync", health: "Degraded", revision: "8f3c1ad0000000000000000000000000000000aa",
			autoSync: true, selfHeal: true, labels: map[string]string{"tier": "backend"},
			resources: []map[string]any{
				{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "nexus", "name": "core",
					"status": "OutOfSync", "health": map[string]any{"status": "Degraded", "message": "crash"}},
				{"version": "v1", "kind": "Service", "namespace": "nexus", "name": "core",
					"status": "Synced", "health": map[string]any{"status": "Healthy"}},
			},
			history: []map[string]any{
				{"id": int64(1), "revision": "r1", "deployedAt": "2026-09-01T10:00:00Z"},
				{"id": int64(2), "revision": "r2", "deployedAt": "2026-09-05T10:00:00Z",
					"source": map[string]any{"repoURL": "https://git/x.git", "path": "a"}},
			},
		}),
		newApp(appOpts{name: "frontend", project: "prod", sync: "Synced", health: "Healthy", revision: "abc", appSetOwner: "gen"}),
		newApp(appOpts{name: "sandbox", project: "dev", sync: "Synced", health: "Healthy", revision: "def"}),
		newProject("argocd", "prod"),
		newAppSet("argocd", "gen"),
	}
}

// buildTestServer wires a Server over a fake dynamic client.
func buildTestServer(t *testing.T, instanceReadOnly bool, objs ...runtime.Object) (*Server, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	if len(objs) == 0 {
		objs = seededObjects()
	}
	dyn := newFakeDynamic(objs...)
	inst := instances.NewForTest("tools", "argocd", dyn, instanceReadOnly)
	reg := instances.NewRegistryForTest("tools", inst)
	return &Server{reg: reg, readOnly: false}, dyn
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is not text: %T", res.Content[0])
	}
	return tc.Text
}
