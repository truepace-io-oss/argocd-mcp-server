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

func TestPingReachable(t *testing.T) {
	in := NewForTest("tools", "argocd", newFakeDynamic(app("argocd", "one")), false)
	got, err := in.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !strings.Contains(got, "reachable") || !strings.Contains(got, "argocd") {
		t.Fatalf("Ping status = %q", got)
	}
}

func TestPingMissingCRD(t *testing.T) {
	dyn := newFakeDynamic()
	dyn.PrependReactor("list", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: argocd.Group, Resource: "applications"}, "")
	})
	in := NewForTest("tools", "argocd", dyn, false)
	_, err := in.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Application CRD is not installed") {
		t.Fatalf("expected a missing-CRD hint, got %v", err)
	}
}

func TestPingForbiddenIsNotMistakenForMissingCRD(t *testing.T) {
	dyn := newFakeDynamic()
	dyn.PrependReactor("list", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: argocd.Group, Resource: "applications"}, "", nil)
	})
	in := NewForTest("tools", "argocd", dyn, false)
	_, err := in.Ping(context.Background())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		t.Fatalf("forbidden must be surfaced verbatim, got %v", err)
	}
}

func TestNewForTestDefaults(t *testing.T) {
	in := NewForTest("x", "", newFakeDynamic(), false)
	if in.Namespace != "argocd" || len(in.Namespaces) != 1 {
		t.Fatalf("defaults wrong: %+v", in)
	}
	if v := in.argoVersion(context.Background()); v != "" {
		t.Fatalf("argoVersion without a typed client should be empty, got %q", v)
	}
}
