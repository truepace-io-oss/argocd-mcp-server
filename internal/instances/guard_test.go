package instances

import (
	"context"
	"strings"
	"testing"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckWriteGuardMissing(t *testing.T) {
	// The fake client accepts the patch — i.e. a spec rewrite would succeed.
	in := NewForTest("tools", "argocd", newFakeDynamic(app("argocd", "one")), false)
	state, reason, err := in.CheckWriteGuard(context.Background())
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if state != argocd.GuardMissing {
		t.Fatalf("accepted spec rewrite must report MISSING, got %v (%s)", state, reason)
	}
}

func TestCheckWriteGuardActiveViaAdmissionPolicy(t *testing.T) {
	dyn := newFakeDynamic(app("argocd", "one"))
	dyn.PrependReactor("patch", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInvalid(
			schema.GroupKind{Group: argocd.Group, Kind: "Application"}, "one", nil)
	})
	// Simulate the real message shape the API server returns for a VAP denial.
	dyn.PrependReactor("patch", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: argocd.Group, Resource: "applications"}, "one",
			errFromString("ValidatingAdmissionPolicy 'argocd-mcp-write-guard' with binding 'argocd-mcp-write-guard' denied request: argocd-mcp may not modify Application.spec"))
	})
	in := NewForTest("tools", "argocd", dyn, false)
	state, reason, err := in.CheckWriteGuard(context.Background())
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if state != argocd.GuardActive || !strings.Contains(reason, "admission policy") {
		t.Fatalf("VAP denial must report active/admission policy, got %v (%s)", state, reason)
	}
}

func TestCheckWriteGuardActiveViaRBAC(t *testing.T) {
	dyn := newFakeDynamic(app("argocd", "one"))
	dyn.PrependReactor("patch", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: argocd.Group, Resource: "applications"}, "one", nil)
	})
	in := NewForTest("tools", "argocd", dyn, false)
	state, reason, err := in.CheckWriteGuard(context.Background())
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if state != argocd.GuardActive || !strings.Contains(reason, "RBAC") {
		t.Fatalf("RBAC denial must also report active, got %v (%s)", state, reason)
	}
}

func TestCheckWriteGuardUnknownWithoutApplications(t *testing.T) {
	in := NewForTest("tools", "argocd", newFakeDynamic(), false)
	state, reason, err := in.CheckWriteGuard(context.Background())
	if err != nil || state != argocd.GuardUnknown || !strings.Contains(reason, "no Application") {
		t.Fatalf("empty instance must be unknown, got %v (%s) err=%v", state, reason, err)
	}
}

func TestCheckWriteGuardUnknownOnUnexpectedError(t *testing.T) {
	dyn := newFakeDynamic(app("argocd", "one"))
	dyn.PrependReactor("patch", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})
	in := NewForTest("tools", "argocd", dyn, false)
	state, _, err := in.CheckWriteGuard(context.Background())
	if state != argocd.GuardUnknown || err == nil {
		// An unexpected failure must never be mistaken for "guarded".
		t.Fatalf("transport failure must be unknown, got %v err=%v", state, err)
	}
}

// TestCheckWriteGuardSendsTheProbePatch asserts the request that goes out.
//
// The dry-run guarantee itself cannot be asserted here: client-go's fake
// dynamic client ignores PatchOptions.DryRun and applies patches regardless.
// It is asserted against a real API server in
// test/e2e/kind/e2e_guard_test.go, which verifies the object is untouched.
func TestCheckWriteGuardSendsTheProbePatch(t *testing.T) {
	dyn := newFakeDynamic(app("argocd", "one"))
	in := NewForTest("tools", "argocd", dyn, false)
	if _, _, err := in.CheckWriteGuard(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sawPatch bool
	for _, a := range dyn.Actions() {
		p, ok := a.(k8stesting.PatchAction)
		if !ok || a.GetVerb() != "patch" {
			continue
		}
		sawPatch = true
		if string(p.GetPatch()) != string(argocd.GuardProbePatch()) {
			t.Fatalf("probe sent an unexpected body: %s", p.GetPatch())
		}
	}
	if !sawPatch {
		t.Fatal("expected a patch action")
	}
}

func errFromString(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
