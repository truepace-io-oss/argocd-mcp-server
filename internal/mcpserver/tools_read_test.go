package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func ctx() context.Context { return context.Background() }

func TestInstancesList(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.instancesList(ctx(), nil, struct{}{})
	got := text(t, res)
	if !strings.Contains(got, "tools (default)") || !strings.Contains(got, "reachable") {
		t.Fatalf("instances_list wrong: %s", got)
	}
	if !strings.Contains(got, "namespaces=argocd") {
		t.Fatalf("instances_list should show namespaces: %s", got)
	}
}

func TestAppsList(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appsList(ctx(), nil, appsListParam{})
	got := text(t, res)
	if !strings.Contains(got, "3 application(s)") {
		t.Fatalf("count wrong: %s", got)
	}
	for _, want := range []string{"argocd/nexus", "argocd/frontend", "argocd/sandbox"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s: %s", want, got)
		}
	}
	if !strings.Contains(got, "OutOfSync 1, Synced 2") || !strings.Contains(got, "Degraded 1, Healthy 2") {
		t.Fatalf("histogram wrong: %s", got)
	}
	// A 40-char SHA is shortened; a short revision is kept verbatim.
	if !strings.Contains(got, "rev=8f3c1ad ") {
		t.Fatalf("revision not shortened: %s", got)
	}
	if !strings.Contains(got, "auto=true(selfHeal,prune)") {
		t.Fatalf("auto-sync label wrong: %s", got)
	}
}

func TestAppsListFilters(t *testing.T) {
	s, _ := buildTestServer(t, false)
	cases := []struct {
		name  string
		in    appsListParam
		want  string
		count string
	}{
		{"project", appsListParam{Project: "dev"}, "sandbox", "1 application(s)"},
		{"syncStatus", appsListParam{SyncStatus: "outofsync"}, "nexus", "1 application(s)"},
		{"healthStatus", appsListParam{HealthStatus: "Healthy"}, "frontend", "2 application(s)"},
		{"nameContains", appsListParam{NameContains: "NEX"}, "nexus", "1 application(s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _, _ := s.appsList(ctx(), nil, c.in)
			got := text(t, res)
			if !strings.Contains(got, c.count) || !strings.Contains(got, c.want) {
				t.Fatalf("filter %s wrong: %s", c.name, got)
			}
		})
	}
}

func TestAppsListLabelSelector(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appsList(ctx(), nil, appsListParam{LabelSelector: "tier=backend"})
	got := text(t, res)
	if !strings.Contains(got, "1 application(s)") || !strings.Contains(got, "nexus") {
		t.Fatalf("label selector not applied server-side: %s", got)
	}
}

func TestAppsListSearchesAllConfiguredNamespaces(t *testing.T) {
	dyn := newFakeDynamic(
		newApp(appOpts{namespace: "argocd", name: "a", sync: "Synced", health: "Healthy"}),
		newApp(appOpts{namespace: "team-a", name: "b", sync: "Synced", health: "Healthy"}),
	)
	inst := instances.NewForTest("tools", "argocd", dyn, false, "team-a")
	s := &Server{reg: instances.NewRegistryForTest("tools", inst)}

	res, _, _ := s.appsList(ctx(), nil, appsListParam{})
	if got := text(t, res); !strings.Contains(got, "2 application(s)") || !strings.Contains(got, "team-a/b") {
		t.Fatalf("apps in additional namespaces not listed: %s", got)
	}
	// An explicit namespace narrows the search again.
	res, _, _ = s.appsList(ctx(), nil, appsListParam{namespaceParam: namespaceParam{Namespace: "team-a"}})
	if got := text(t, res); !strings.Contains(got, "1 application(s)") || strings.Contains(got, "argocd/a") {
		t.Fatalf("explicit namespace not honoured: %s", got)
	}
}

func TestAppGet(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appGet(ctx(), nil, appRef{Name: "nexus"})
	got := text(t, res)
	for _, want := range []string{
		"Application argocd/nexus @ tools",
		"project:     prod",
		"destination: in-cluster/nexus",
		"sync:        OutOfSync",
		"health:      Degraded",
		"resources:   2 managed, 1 drifted",
		"id=2 revision=r2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("app_get missing %q:\n%s", want, got)
		}
	}
}

func TestAppGetNotFound(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appGet(ctx(), nil, appRef{Name: "ghost"})
	if !res.IsError || !strings.Contains(text(t, res), "not found") {
		t.Fatalf("expected a not-found tool error, got: %s", text(t, res))
	}
}

func TestAppGetRequiresName(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appGet(ctx(), nil, appRef{})
	if !res.IsError || !strings.Contains(text(t, res), "name is required") {
		t.Fatalf("expected a name-required error, got: %s", text(t, res))
	}
}

func TestUnknownInstance(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appsList(ctx(), nil, appsListParam{namespaceParam: namespaceParam{instanceParam: instanceParam{Instance: "nope"}}})
	if !res.IsError || !strings.Contains(text(t, res), "unknown Argo CD instance") {
		t.Fatalf("expected unknown-instance error, got: %s", text(t, res))
	}
}

func TestAppResources(t *testing.T) {
	s, _ := buildTestServer(t, false)

	res, _, _ := s.appResources(ctx(), nil, appResourcesParam{appRef: appRef{Name: "nexus"}})
	got := text(t, res)
	if !strings.Contains(got, "1 drifted resource(s) of 2 managed") || !strings.Contains(got, "apps/Deployment nexus/core") {
		t.Fatalf("drifted-only listing wrong: %s", got)
	}
	if strings.Contains(got, "Service") {
		t.Fatalf("healthy resource should be filtered out: %s", got)
	}

	all := false
	res, _, _ = s.appResources(ctx(), nil, appResourcesParam{appRef: appRef{Name: "nexus"}, OnlyDrifted: &all})
	got = text(t, res)
	if !strings.Contains(got, "2 resource(s) of 2 managed") || !strings.Contains(got, "Service nexus/core") {
		t.Fatalf("full listing wrong: %s", got)
	}
}

func TestAppResourcesNoDrift(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appResources(ctx(), nil, appResourcesParam{appRef: appRef{Name: "frontend"}})
	if got := text(t, res); !strings.Contains(got, "everything is Synced and Healthy") {
		t.Fatalf("expected the no-drift hint: %s", got)
	}
}

func TestAppHistoryLimit(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appHistory(ctx(), nil, appHistoryParam{appRef: appRef{Name: "nexus"}, Limit: 1})
	got := text(t, res)
	if !strings.Contains(got, "1 history entr(ies)") || !strings.Contains(got, "id=2") || strings.Contains(got, "id=1") {
		t.Fatalf("limit should keep the newest entry: %s", got)
	}
}

func TestProjectsList(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.projectsList(ctx(), nil, namespaceParam{})
	got := text(t, res)
	if !strings.Contains(got, "1 AppProject(s) @ tools") || !strings.Contains(got, "argocd/prod") {
		t.Fatalf("projects_list wrong: %s", got)
	}
	if !strings.Contains(got, "destinations: in-cluster/*") {
		t.Fatalf("destinations missing: %s", got)
	}
}

func TestAppSetsList(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appSetsList(ctx(), nil, namespaceParam{})
	got := text(t, res)
	if !strings.Contains(got, "1 ApplicationSet(s) @ tools") || !strings.Contains(got, "generators=list") {
		t.Fatalf("appsets_list wrong: %s", got)
	}
	if !strings.Contains(got, "applications=1") {
		t.Fatalf("owned application count wrong: %s", got)
	}
}

func TestListErrorsSurfaceVerbatim(t *testing.T) {
	s, dyn := buildTestServer(t, false)
	dyn.PrependReactor("list", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: argocd.Group, Resource: "applications"}, "", nil)
	})
	res, _, _ := s.appsList(ctx(), nil, appsListParam{})
	if !res.IsError || !strings.Contains(strings.ToLower(text(t, res)), "forbidden") {
		t.Fatalf("RBAC errors must reach the model verbatim: %s", text(t, res))
	}
	if got := classifyResult(res, nil); got != "forbidden" {
		t.Fatalf("classifyResult = %q, want forbidden", got)
	}
}

func TestMCPServerRegistersTools(t *testing.T) {
	s, _ := buildTestServer(t, false)
	if s.MCPServer() == nil {
		t.Fatal("MCPServer() returned nil")
	}
}
