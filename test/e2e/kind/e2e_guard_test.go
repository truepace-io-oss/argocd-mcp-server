//go:build e2e_kind

// Write-guard integration test.
//
// This is the test that decides whether the sync tier is safe to enable. It
// installs the ValidatingAdmissionPolicy built from internal/argocd's canonical
// expressions, then proves two things against a REAL API server with a REAL
// Argo CD:
//
//  1. every known escalation path is denied, and
//  2. everything the MCP legitimately does still works.
//
// A guard that silently fails to enforce is worse than no guard, so the suite
// starts with a negative control: it refuses to assert anything until a
// known-bad patch is actually rejected.
package kind

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	guardNS   = "argocd-mcp"
	guardSA   = "argocd-mcp"
	guardApp  = "amcp-guard-target"
	guardUser = "system:serviceaccount:" + guardNS + ":" + guardSA
)

// installGuard applies the policy exactly as the chart and the GitOps prototype
// render it, from the same canonical expressions.
func installGuard(t *testing.T, cs *kubernetes.Clientset, actions []admissionv1.ValidationAction) {
	t.Helper()
	ctx := context.Background()
	fail := admissionv1.Fail

	vals := make([]admissionv1.Validation, 0, 3)
	for _, v := range argocd.GuardValidations() {
		vals = append(vals, admissionv1.Validation{Expression: v.Expression, Message: v.Message})
	}

	_ = cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(ctx, argocd.PolicyName, metav1.DeleteOptions{})
	_ = cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, argocd.PolicyName, metav1.DeleteOptions{})

	pol := &admissionv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: argocd.PolicyName},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionv1.MatchResources{
				ResourceRules: []admissionv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionv1.RuleWithOperations{
						Operations: []admissionv1.OperationType{admissionv1.Update},
						Rule: admissionv1.Rule{
							APIGroups:   []string{argocd.Group},
							APIVersions: []string{argocd.Version},
							Resources:   []string{"applications"},
						},
					},
				}},
			},
			MatchConditions: []admissionv1.MatchCondition{{
				Name:       "only-argocd-mcp",
				Expression: fmt.Sprintf("request.userInfo.username == %q", guardUser),
			}},
			Validations: vals,
		},
	}
	if _, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, pol, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	bind := &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: argocd.PolicyName},
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        argocd.PolicyName,
			ValidationActions: actions,
		},
	}
	if _, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, bind, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(context.Background(), argocd.PolicyName, metav1.DeleteOptions{})
		_ = cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(context.Background(), argocd.PolicyName, metav1.DeleteOptions{})
	})
}

// provisionSyncTier creates the ServiceAccount and the "sync" Role the
// argocd-mcp-agent prototype deploys, and returns a bearer token for it.
func provisionSyncTier(t *testing.T, cs *kubernetes.Clientset, saName string) string {
	t.Helper()
	ctx := context.Background()
	_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: guardNS}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("namespace: %v", err)
	}
	_, err = cs.CoreV1().ServiceAccounts(guardNS).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: guardNS}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("serviceaccount: %v", err)
	}
	// Exactly the sync tier: read plus `patch`. No create/update/delete.
	roleName := "argocd-mcp-sync-" + saName
	_, err = cs.RbacV1().Roles(argocdNS).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: argocdNS},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{argocd.Group},
			Resources: []string{"applications", "appprojects", "applicationsets"},
			Verbs:     []string{"get", "list", "watch"},
		}, {
			APIGroups: []string{argocd.Group},
			Resources: []string{"applications"},
			Verbs:     []string{"patch"},
		}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("role: %v", err)
	}
	_, err = cs.RbacV1().RoleBindings(argocdNS).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: argocdNS},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: guardNS}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("rolebinding: %v", err)
	}
	exp := int64(3600)
	tr, err := cs.CoreV1().ServiceAccounts(guardNS).CreateToken(ctx, saName,
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &exp}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = cs.RbacV1().RoleBindings(argocdNS).Delete(ctx, roleName, metav1.DeleteOptions{})
		_ = cs.RbacV1().Roles(argocdNS).Delete(ctx, roleName, metav1.DeleteOptions{})
		// The namespace is deliberately NOT deleted: the policy's matchCondition
		// pins this exact ServiceAccount identity, and a terminating namespace
		// would break any test that runs next.
	})
	return tr.Status.Token
}

// mcpAsServiceAccount builds an Instance and an MCP session authenticated as the
// argocd-mcp ServiceAccount, so admission sees the identity the policy targets.
func mcpAsServiceAccount(t *testing.T, admin *rest.Config, token string) (*instances.Instance, *mcp.ClientSession) {
	t.Helper()
	if len(admin.CAData) == 0 {
		t.Fatal("kind kubeconfig has no CAData")
	}
	cfg := &config.Config{
		LogLevel:        "error",
		DefaultInstance: "kind",
		Instances: []config.InstanceConfig{{
			Name:                     "kind",
			Server:                   admin.Host,
			CertificateAuthorityData: base64.StdEncoding.EncodeToString(admin.CAData),
			Token:                    token,
			Namespace:                argocdNS,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	reg, err := instances.Build(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg.Default(), startMCPFromRegistry(t, reg, cfg)
}

func saDynamic(t *testing.T, admin *rest.Config, token string) dynamic.Interface {
	t.Helper()
	c := rest.CopyConfig(admin)
	c.BearerToken = token
	c.BearerTokenFile = ""
	c.CertData, c.KeyData = nil, nil
	d, err := dynamic.NewForConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func patchAs(c dynamic.Interface, ns, name, body string) error {
	_, err := c.Resource(argocd.AppGVR).Namespace(ns).Patch(context.Background(), name,
		types.MergePatchType, []byte(body), metav1.PatchOptions{})
	return err
}

// dryRunPatchAs runs the full admission chain without persisting anything.
func dryRunPatchAs(c dynamic.Interface, ns, name, body string) error {
	_, err := c.Resource(argocd.AppGVR).Namespace(ns).Patch(context.Background(), name,
		types.MergePatchType, []byte(body),
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	return err
}

// guardActivationTimeout is how long we wait for a freshly created
// ValidatingAdmissionPolicy to start enforcing.
//
// This is a real operational property, not just a test detail: the objects
// exist before the API server enforces them. On a healthy cluster activation is
// sub-second, but on a loaded or degraded API server it has been observed to
// take several minutes — which is exactly why the rollout must confirm
// amcp_write_guard_active rather than assume the guard is live.
const guardActivationTimeout = 5 * time.Minute

// TestKindWriteGuard is the gate on enabling the sync tier.
func TestKindWriteGuard(t *testing.T) {
	rc := restConfig(t)
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	seedGuardApp(t, dyn)
	token := provisionSyncTier(t, cs, guardSA)
	installGuard(t, cs, []admissionv1.ValidationAction{admissionv1.Deny})

	sa := saDynamic(t, rc, token)

	// --- NEGATIVE CONTROL -------------------------------------------------
	// Assert nothing until the policy is provably enforcing. Admission policies
	// activate asynchronously, and a test that runs before activation silently
	// proves nothing.
	// Admission policies activate asynchronously, so the budget is generous.
	deadline := time.Now().Add(guardActivationTimeout)
	active := false
	for time.Now().Before(deadline) {
		// dry-run: the control must prove enforcement without mutating anything.
		err := dryRunPatchAs(sa, argocdNS, guardApp, `{"spec":{"project":"probe"}}`)
		if err != nil && strings.Contains(err.Error(), argocd.PolicyName) {
			active = true
			break
		}
		if err != nil && apierrors.IsForbidden(err) && !strings.Contains(err.Error(), argocd.PolicyName) {
			t.Fatalf("request never reached admission (stopped by RBAC): %v", err)
		}
		time.Sleep(time.Second)
	}
	if !active {
		t.Fatal("write guard never became active — refusing to assert anything against an inactive policy")
	}

	// --- the escalation paths must all be closed --------------------------
	attacks := []struct{ name, body string }{
		{"spec.source rewrite", `{"spec":{"source":{"repoURL":"https://evil.example/x.git"}}}`},
		{"spec.destination rewrite", `{"spec":{"destination":{"namespace":"kube-system"}}}`},
		{"operation.sync.source override", `{"operation":{"sync":{"revision":"HEAD","source":{"repoURL":"https://evil.example/x.git"}}}}`},
		{"operation.sync.sources override", `{"operation":{"sync":{"revision":"HEAD","sources":[{"repoURL":"https://evil.example/x.git"}]}}}`},
		{"status.history poisoning", `{"status":{"history":[{"id":99,"revision":"bad","deployedAt":"2026-01-01T00:00:00Z","source":{"repoURL":"https://evil.example/x.git"}}]}}`},
	}
	for _, a := range attacks {
		if err := dryRunPatchAs(sa, argocdNS, guardApp, a.body); err == nil {
			t.Errorf("%-34s ALLOWED — escalation path is open", a.name)
		} else if !strings.Contains(err.Error(), argocd.PolicyName) {
			t.Errorf("%-34s denied, but not by the guard: %v", a.name, err)
		} else {
			t.Logf("%-34s denied by the write guard", a.name)
		}
	}

	// --- everything the MCP legitimately does must still work -------------
	inst, sess := mcpAsServiceAccount(t, rc, token)

	if out, isErr := callText(t, sess, "app_refresh", map[string]any{"name": guardApp, "wait": false}); isErr {
		t.Fatalf("app_refresh must still work under the guard: %s", out)
	}
	t.Log("app_refresh                        allowed under the guard")

	if out, isErr := callText(t, sess, "app_sync", map[string]any{"name": guardApp, "revision": "HEAD", "dryRun": true}); isErr {
		t.Fatalf("app_sync must still work under the guard: %s", out)
	}
	t.Log("app_sync                           allowed under the guard")

	// An operation that carries no sync block must not trip the has() guards.
	if err := dryRunPatchAs(sa, argocdNS, guardApp, `{"operation":{"initiatedBy":{"username":"argocd-mcp:test"}}}`); err != nil {
		t.Fatalf("an operation without a sync block must be allowed (has() short-circuit): %v", err)
	}
	t.Log("operation without sync             allowed (has() short-circuit works)")

	// --- the runtime check must agree -------------------------------------
	state, reason, err := inst.CheckWriteGuard(ctx)
	if err != nil {
		t.Fatalf("CheckWriteGuard: %v", err)
	}
	if state != argocd.GuardActive || !strings.Contains(reason, "admission policy") {
		t.Fatalf("runtime check disagrees with reality: %v (%s)", state, reason)
	}
	t.Log("CheckWriteGuard                    reports active via admission policy")

	// --- and the probe must never write -----------------------------------
	before := getGuardApp(t, dyn)
	if _, _, err := inst.CheckWriteGuard(ctx); err != nil {
		t.Fatal(err)
	}
	after := getGuardApp(t, dyn)
	if before.GetResourceVersion() != after.GetResourceVersion() {
		t.Fatal("the guard probe modified the object — it must always use dryRun=All")
	}
	proj, _, _ := unstructured.NestedString(after.Object, "spec", "project")
	if proj != "default" {
		t.Fatalf("spec.project changed to %q — the probe wrote to the cluster", proj)
	}
	t.Log("guard probe                        left the object untouched (dryRun=All)")
}

// TestKindWriteGuardMissingIsDetected proves the runtime check reports MISSING
// when no policy is installed — the alerting path must not be vacuous.
func TestKindWriteGuardMissingIsDetected(t *testing.T) {
	rc := restConfig(t)
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	seedGuardApp(t, dyn)
	// A ServiceAccount the write guard deliberately does NOT match: it has the
	// sync tier's RBAC but no admission policy covering it. That makes this
	// assertion independent of whether a policy happens to exist, and of the
	// (slow, asynchronous) activation of one.
	token := provisionSyncTier(t, cs, "argocd-mcp-unguarded")

	inst, _ := mcpAsServiceAccount(t, rc, token)
	state, reason, err := inst.CheckWriteGuard(context.Background())
	if err != nil {
		t.Fatalf("CheckWriteGuard: %v", err)
	}
	if state != argocd.GuardMissing {
		t.Fatalf("without a policy the guard must report MISSING, got %v (%s)", state, reason)
	}
	t.Logf("unguarded sync tier detected: %s", reason)
}

func seedGuardApp(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	ctx := context.Background()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": argocd.Group + "/" + argocd.Version,
		"kind":       "Application",
		"metadata":   map[string]any{"name": guardApp, "namespace": argocdNS},
		"spec": map[string]any{
			"project":     "default",
			"source":      map[string]any{"repoURL": "https://github.com/argoproj/argocd-example-apps.git", "path": "guestbook", "targetRevision": "HEAD"},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "amcp-guard-never-synced"},
		},
	}}
	_, err := dyn.Resource(argocd.AppGVR).Namespace(argocdNS).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("seed guard app: %v", err)
	}
	t.Cleanup(func() {
		_ = dyn.Resource(argocd.AppGVR).Namespace(argocdNS).Delete(context.Background(), guardApp, metav1.DeleteOptions{})
	})
}

func getGuardApp(t *testing.T, dyn dynamic.Interface) *unstructured.Unstructured {
	t.Helper()
	obj, err := dyn.Resource(argocd.AppGVR).Namespace(argocdNS).Get(context.Background(), guardApp, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return obj
}
