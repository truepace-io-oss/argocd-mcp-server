//go:build e2e_kind

// Package kind runs the argocd-mcp server against a real Argo CD installation in
// a kind cluster. It is the only test that exercises the complete path
// refresh → sync → wait → workload exists, because it is the only one with a
// running application-controller.
//
// Run with:
//
//	./setup-kind.sh          # once: create the cluster and install Argo CD
//	go test -tags e2e_kind ./test/e2e/kind/... -v
package kind

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/mcpserver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	appName   = "amcp-guestbook"
	argocdNS  = "argocd"
	appTarget = "amcp-e2e-target"
)

// kubeconfigPath returns a SINGLE kubeconfig file for the MCP's kubeconfig
// credential mode. KUBECONFIG may legitimately be a ":"-separated list of files;
// the server takes one path, so a list is resolved to its first entry.
func kubeconfigPath() string {
	if v := os.Getenv("AMCP_KIND_KUBECONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("KUBECONFIG"); v != "" {
		if i := strings.IndexByte(v, os.PathListSeparator); i >= 0 {
			return v[:i]
		}
		return v
	}
	return os.Getenv("HOME") + "/.kube/config"
}

func kubeContext() string {
	if v := os.Getenv("AMCP_KIND_CONTEXT"); v != "" {
		return v
	}
	return "kind-amcp-e2e"
}

// restConfig is the admin client used for seeding and verification. It honours
// the full KUBECONFIG precedence list, unlike the single-file path the MCP
// itself consumes.
func restConfig(t *testing.T) *rest.Config {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext()},
	).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig context %q: %v (run ./setup-kind.sh first)", kubeContext(), err)
	}
	return cfg
}

// startMCP serves the MCP against the kind cluster's Argo CD via the kubeconfig
// credential mode and returns a connected client session.
func startMCP(t *testing.T) *mcp.ClientSession {
	t.Helper()
	cfg := &config.Config{
		LogLevel:        "error",
		DefaultInstance: "kind",
		Instances: []config.InstanceConfig{{
			Name:           "kind",
			KubeconfigFile: kubeconfigPath(),
			Context:        kubeContext(),
			Namespace:      argocdNS,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	reg, err := instances.Build(cfg)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	return startMCPFromRegistry(t, reg, cfg)
}

// startMCPFromRegistry serves an MCP over an already-built registry, so tests
// can choose the credentials (e.g. a ServiceAccount token) themselves.
func startMCPFromRegistry(t *testing.T, reg *instances.Registry, cfg *config.Config) *mcp.ClientSession {
	t.Helper()
	mcpSrv := mcpserver.New(reg, cfg).MCPServer()
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "kind-e2e", Version: "0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var out string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	return out, res.IsError
}

// TestKindSyncFlow drives a real Argo CD through the MCP: create an Application
// pointing at a public repo, refresh it, sync it, wait for it, and verify the
// workload actually exists in the cluster.
func TestKindSyncFlow(t *testing.T) {
	rc := restConfig(t)
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Seed the Application (manual sync policy — the MCP triggers the sync).
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": argocd.Group + "/" + argocd.Version,
		"kind":       "Application",
		"metadata":   map[string]any{"name": appName, "namespace": argocdNS},
		"spec": map[string]any{
			"project": "default",
			"source": map[string]any{
				"repoURL":        "https://github.com/argoproj/argocd-example-apps.git",
				"path":           "guestbook",
				"targetRevision": "HEAD",
			},
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": appTarget,
			},
			// Manual sync policy: the MCP triggers every sync, and the destination
			// namespace is created via the operation's own syncOptions.
		},
	}}
	_, err = dyn.Resource(argocd.AppGVR).Namespace(argocdNS).Create(ctx, app, metav1.CreateOptions{})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create application: %v", err)
	}
	t.Cleanup(func() {
		_ = dyn.Resource(argocd.AppGVR).Namespace(argocdNS).Delete(context.Background(), appName, metav1.DeleteOptions{})
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), appTarget, metav1.DeleteOptions{})
	})

	sess := startMCP(t)

	if out, isErr := callText(t, sess, "instances_list", map[string]any{}); isErr || !strings.Contains(out, "reachable") {
		t.Fatalf("instances_list: isErr=%v out=%s", isErr, out)
	}

	// Refresh: the controller must clear the annotation and reconcile.
	out, isErr := callText(t, sess, "app_refresh", map[string]any{"name": appName, "timeoutSeconds": 120})
	if isErr {
		t.Fatalf("app_refresh failed: %s", out)
	}
	t.Logf("refresh: %s", out)

	// Sync, then wait for the application to become Synced + Healthy. The
	// syncOptions are passed through the tool (not the Application spec), which
	// also proves that operation.sync.syncOptions reaches the real controller:
	// without CreateNamespace the sync fails with "namespace not found".
	if out, isErr = callText(t, sess, "app_sync", map[string]any{
		"name": appName, "revision": "HEAD",
		"syncOptions": []any{"CreateNamespace=true"},
	}); isErr {
		t.Fatalf("app_sync failed: %s", out)
	}
	out, isErr = callText(t, sess, "app_wait", map[string]any{"name": appName, "timeoutSeconds": 600, "pollSeconds": 5})
	if isErr {
		t.Fatalf("app_wait failed: %s", out)
	}
	if !strings.Contains(out, "sync=Synced") || !strings.Contains(out, "health=Healthy") {
		t.Fatalf("application did not converge: %s", out)
	}

	// The workload really exists.
	if _, err := cs.AppsV1().Deployments(appTarget).Get(ctx, "guestbook-ui", metav1.GetOptions{}); err != nil {
		t.Fatalf("guestbook-ui deployment was not created by the sync: %v", err)
	}

	// And the MCP now reports it as managed and healthy.
	out, isErr = callText(t, sess, "app_resources", map[string]any{"name": appName, "onlyDrifted": false})
	if isErr || !strings.Contains(out, "guestbook-ui") {
		t.Fatalf("app_resources: isErr=%v out=%s", isErr, out)
	}
}
