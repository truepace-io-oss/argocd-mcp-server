// Package instances holds the ready-to-use Kubernetes clients for every Argo CD
// installation this MCP manages, and a registry to look them up by name.
//
// An instance is addressed through the Kubernetes API server of the cluster it
// runs in — never through the Argo CD HTTP API — so remote instances need no
// ingress, no VPN and no Argo CD account: only a ServiceAccount whose RBAC
// allows working with argoproj.io resources in the Argo CD namespace.
package instances

import (
	"context"
	"fmt"
	"strings"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Instance is one Argo CD installation with ready clients. The clients are
// goroutine-safe and pool connections, so an Instance is built once and shared
// across all concurrent tool invocations.
type Instance struct {
	Name string
	// Namespace is the primary Argo CD namespace (where Applications live).
	Namespace string
	// Namespaces is Namespace plus any additional application namespaces.
	Namespaces []string
	ReadOnly   bool

	RESTConfig *rest.Config
	Dynamic    dynamic.Interface
	// Typed is used only for the best-effort Argo CD version probe; it may be
	// nil in tests.
	Typed kubernetes.Interface
}

// newInstance constructs the clients for one instance from its config. It does
// not contact the API server.
func newInstance(c config.InstanceConfig) (*Instance, error) {
	rc, err := restConfigFor(c)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("instance %q: build dynamic client: %w", c.Name, err)
	}
	typed, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("instance %q: build typed client: %w", c.Name, err)
	}
	nss := c.Namespaces()
	return &Instance{
		Name:       c.Name,
		Namespace:  nss[0],
		Namespaces: nss,
		ReadOnly:   c.ReadOnly,
		RESTConfig: rc,
		Dynamic:    dyn,
		Typed:      typed,
	}, nil
}

// NewForTest builds an Instance from an already-constructed (usually fake)
// dynamic client, so unit tests need no API server.
func NewForTest(name, namespace string, dyn dynamic.Interface, readOnly bool, extraNamespaces ...string) *Instance {
	if namespace == "" {
		namespace = config.DefaultArgoCDNamespace
	}
	return &Instance{
		Name:       name,
		Namespace:  namespace,
		Namespaces: append([]string{namespace}, extraNamespaces...),
		ReadOnly:   readOnly,
		Dynamic:    dyn,
	}
}

// Ping reports whether the Argo CD API surface is reachable. It deliberately
// probes the Application resource rather than just the cluster: a reachable
// cluster without Argo CD (or without RBAC) is not a usable instance. It must
// never be fatal at startup.
func (i *Instance) Ping(ctx context.Context) (string, error) {
	list, err := argocd.ListApps(ctx, i.Dynamic, i.Namespace, metav1.ListOptions{Limit: 1})
	if err != nil {
		if isNoArgoCD(err) {
			return "", fmt.Errorf("reachable, but the Application CRD is not installed (is Argo CD deployed in namespace %q?)", i.Namespace)
		}
		return "", err
	}
	status := fmt.Sprintf("reachable (namespace %s", i.Namespace)
	if v := i.argoVersion(ctx); v != "" {
		status += ", argocd " + v
	}
	status += ")"
	_ = list
	return status, nil
}

// argoVersion reads the Argo CD version from the argocd-server Deployment image
// tag. Best effort: any failure (no RBAC, no such Deployment) yields "".
func (i *Instance) argoVersion(ctx context.Context) string {
	if i.Typed == nil {
		return ""
	}
	dep, err := i.Typed.AppsV1().Deployments(i.Namespace).Get(ctx, "argocd-server", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if idx := strings.LastIndex(c.Image, ":"); idx > 0 {
			return c.Image[idx+1:]
		}
	}
	return ""
}

// CheckWriteGuard verifies at runtime that this instance's credentials CANNOT
// rewrite an Application's source. It sends a deliberately forbidden spec patch
// with dryRun=All — the API server runs the full admission chain and persists
// nothing — and expects it to be rejected.
//
// A rejection is good news whichever gate produced it: the admission policy
// (guard installed) or RBAC (no patch permission at all). Acceptance means this
// token can redirect a sync at an arbitrary source, which is the escalation
// path the write guard exists to close.
//
// It returns the state, a short human-readable reason, and any transport error.
func (i *Instance) CheckWriteGuard(ctx context.Context) (argocd.GuardState, string, error) {
	list, err := argocd.ListApps(ctx, i.Dynamic, i.Namespace, metav1.ListOptions{Limit: 1})
	if err != nil {
		return argocd.GuardUnknown, "cannot list applications", err
	}
	if len(list.Items) == 0 {
		return argocd.GuardUnknown, "no Application to probe against", nil
	}
	target := list.Items[0]

	_, err = i.Dynamic.Resource(argocd.AppGVR).Namespace(target.GetNamespace()).Patch(
		ctx, target.GetName(), argocd.PatchType, argocd.GuardProbePatch(),
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})

	switch {
	case err == nil:
		return argocd.GuardMissing, "a spec rewrite was accepted (dry-run)", nil
	case strings.Contains(err.Error(), "ValidatingAdmissionPolicy"):
		return argocd.GuardActive, "blocked by admission policy", nil
	case apierrors.IsForbidden(err):
		return argocd.GuardActive, "blocked by RBAC (no patch permission)", nil
	default:
		// An unexpected error is not evidence of safety.
		return argocd.GuardUnknown, "probe failed", err
	}
}

// isNoArgoCD reports whether the error means the Argo CD CRDs are absent.
func isNoArgoCD(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not find the requested resource") ||
		strings.Contains(msg, "the server could not find the requested resource") ||
		strings.Contains(msg, `no matches for kind`) ||
		strings.Contains(msg, "applications.argoproj.io") && strings.Contains(msg, "not found") && !strings.Contains(msg, "forbidden")
}
