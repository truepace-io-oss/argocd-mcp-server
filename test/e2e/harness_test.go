// Package e2e drives the argocd-mcp server end-to-end against a real Kubernetes
// API server (provided by envtest) with the real Argo CD CRDs installed, using
// the official MCP client over the streamable-HTTP transport.
//
// No Argo CD controller runs here, so the write tests assert the *intent* the
// server writes to the Application CR — which is exactly the contract between
// this MCP and Argo CD. The full sync path is covered by test/e2e/kind.
package e2e

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/mcpserver"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	testEnv  *envtest.Environment
	adminCfg *rest.Config
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Fail loudly rather than silently skipping: the Makefile / CI must set
		// KUBEBUILDER_ASSETS (via `setup-envtest use`).
		println("KUBEBUILDER_ASSETS is not set; run via `make test-e2e` (installs envtest binaries via setup-envtest)")
		os.Exit(1)
	}
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		println("failed to start envtest:", err.Error())
		os.Exit(1)
	}
	adminCfg = cfg
	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

func adminClient(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	cs, err := kubernetes.NewForConfig(adminCfg)
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	return cs
}

func adminDynamic(t *testing.T) dynamic.Interface {
	t.Helper()
	dyn, err := dynamic.NewForConfig(adminCfg)
	if err != nil {
		t.Fatalf("admin dynamic client: %v", err)
	}
	return dyn
}

// mintToken creates a ServiceAccount (if missing) and returns a short-lived
// bound token via the TokenRequest API — exactly how an operator would provision
// credentials for the MCP.
func mintToken(t *testing.T, cs *kubernetes.Clientset, ns, sa string) string {
	t.Helper()
	ctx := context.Background()
	_, _ = cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: ns},
	}, metav1.CreateOptions{})
	exp := int64(3600)
	tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa, &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &exp},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("mint token for %s/%s: %v", ns, sa, err)
	}
	return tr.Status.Token
}

func ensureNamespace(t *testing.T, cs *kubernetes.Clientset, name string) {
	t.Helper()
	_, err := cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// grantArgoRole creates the namespaced Role the argocd-mcp-agent prototype
// deploys and binds it to the ServiceAccount. verbs are the argoproj.io verbs;
// read-only means [get list watch], sync adds "patch".
func grantArgoRole(t *testing.T, cs *kubernetes.Clientset, ns, sa, roleName string, verbs []string) {
	t.Helper()
	ctx := context.Background()
	_, err := cs.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{argocd.Group},
			Resources: []string{"applications", "appprojects", "applicationsets"},
			Verbs:     verbs,
		}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create role %s: %v", roleName, err)
	}
	_, err = cs.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: ns}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create rolebinding %s: %v", roleName, err)
	}
}

// caData returns the API server CA the admin config trusts, base64-encoded for
// InstanceConfig.CertificateAuthorityData.
func caData(t *testing.T) string {
	t.Helper()
	if len(adminCfg.CAData) == 0 {
		t.Fatal("envtest admin config has no CAData")
	}
	return base64.StdEncoding.EncodeToString(adminCfg.CAData)
}

// mcpConfig builds a one-instance server config pointing at envtest.
func mcpConfig(instanceName, namespace, token string, instanceReadOnly, globalReadOnly bool) *config.Config {
	return &config.Config{
		LogLevel:        "error",
		ReadOnly:        globalReadOnly,
		DefaultInstance: instanceName,
		Instances: []config.InstanceConfig{{
			Name:                     instanceName,
			Server:                   adminCfg.Host,
			CertificateAuthorityData: "", // filled by the caller via caData
			Token:                    token,
			Namespace:                namespace,
			ReadOnly:                 instanceReadOnly,
		}},
	}
}

// startMCP builds and serves an argocd-mcp instance for one token-scoped
// cluster, returning a connected MCP client session.
func startMCP(t *testing.T, instanceName, namespace, token string, instanceReadOnly, globalReadOnly bool) *mcp.ClientSession {
	t.Helper()
	cfg := mcpConfig(instanceName, namespace, token, instanceReadOnly, globalReadOnly)
	cfg.Instances[0].CertificateAuthorityData = caData(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	reg, err := instances.Build(cfg)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	mcpSrv := mcpserver.New(reg, cfg).MCPServer()

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callText calls a tool and returns (text, isError).
func callText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

// --- Argo CD object seeding (through the admin client) ---

type seedOpts struct {
	project   string
	sync      string
	health    string
	revision  string
	autoSync  bool
	resources []any
	history   []any
}

// seedApp creates an Application and then writes its status (the CRD has no
// status subresource, so a single update carries both).
func seedApp(t *testing.T, ns, name string, o seedOpts) {
	t.Helper()
	dyn := adminDynamic(t)
	if o.project == "" {
		o.project = "default"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": argocd.Group + "/" + argocd.Version,
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"project": o.project,
			"source": map[string]any{
				"repoURL":        "https://git.example.com/org/gitops.git",
				"path":           "rendered/envs/e2e/" + name,
				"targetRevision": "main",
			},
			"destination": map[string]any{"name": "in-cluster", "namespace": name},
		},
	}}
	if o.autoSync {
		_ = unstructured.SetNestedMap(obj.Object, map[string]any{"selfHeal": true, "prune": true}, "spec", "syncPolicy", "automated")
	}
	created, err := dyn.Resource(argocd.AppGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("seed application %s/%s: %v", ns, name, err)
	}
	if err != nil { // already exists: fetch it so we can patch the status
		created, err = dyn.Resource(argocd.AppGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get seeded application: %v", err)
		}
	}

	status := map[string]any{
		"sync":         map[string]any{"status": o.sync, "revision": o.revision},
		"health":       map[string]any{"status": o.health},
		"reconciledAt": time.Now().UTC().Format(time.RFC3339),
	}
	if o.resources != nil {
		status["resources"] = o.resources
	}
	if o.history != nil {
		status["history"] = o.history
	}
	if err := unstructured.SetNestedMap(created.Object, status, "status"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if _, err := dyn.Resource(argocd.AppGVR).Namespace(ns).Update(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write status for %s/%s: %v", ns, name, err)
	}
}

// getApp reads an Application back with the admin client, to verify what the MCP wrote.
func getApp(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := adminDynamic(t).Resource(argocd.AppGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read back %s/%s: %v", ns, name, err)
	}
	return obj
}

func seedProject(t *testing.T, ns, name string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": argocd.Group + "/" + argocd.Version,
		"kind":       "AppProject",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"description":  "e2e project",
			"sourceRepos":  []any{"*"},
			"destinations": []any{map[string]any{"name": "in-cluster", "namespace": "*"}},
		},
	}}
	_, err := adminDynamic(t).Resource(argocd.ProjectGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("seed appproject: %v", err)
	}
}

func mustContain(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%s: missing %q in:\n%s", what, want, got)
	}
}

// updateApp writes an Application back with the admin client.
func updateApp(t *testing.T, ns string, obj *unstructured.Unstructured) {
	t.Helper()
	if _, err := adminDynamic(t).Resource(argocd.AppGVR).Namespace(ns).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update %s/%s: %v", ns, obj.GetName(), err)
	}
}
