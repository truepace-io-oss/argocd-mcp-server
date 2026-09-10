package mcpserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
)

func TestTruncate(t *testing.T) {
	if got := truncate("  a\nb  ", 10); got != "a b" {
		t.Fatalf("truncate normalisation = %q", got)
	}
	long := strings.Repeat("x", 600)
	got := truncate(long, maxMessageLen)
	if !strings.HasSuffix(got, "…(truncated)") || len(got) < maxMessageLen {
		t.Fatalf("long message not truncated: %d chars", len(got))
	}
}

func TestShortRev(t *testing.T) {
	sha := "8f3c1ad0000000000000000000000000000000aa"
	if got := shortRev(sha); got != "8f3c1ad" {
		t.Fatalf("shortRev(sha) = %q", got)
	}
	if got := shortRev("main"); got != "main" {
		t.Fatalf("shortRev(branch) = %q", got)
	}
	if got := shortRev(""); got != "-" {
		t.Fatalf("shortRev(empty) = %q", got)
	}
	// 40 chars but not hex: keep verbatim.
	if got := shortRev(strings.Repeat("z", 40)); got != strings.Repeat("z", 40) {
		t.Fatalf("non-hex 40-char revision was shortened: %q", got)
	}
}

func TestHistogramDeterministic(t *testing.T) {
	m := map[string]int{"Synced": 2, "OutOfSync": 1}
	if got := histogram(m); got != "OutOfSync 1, Synced 2" {
		t.Fatalf("histogram = %q", got)
	}
	if got := histogram(map[string]int{}); got != "none" {
		t.Fatalf("empty histogram = %q", got)
	}
}

func TestAppsTableSortedAndEmpty(t *testing.T) {
	apps := []*argocd.App{
		{Namespace: "argocd", Name: "z", SyncStatus: "Synced", HealthStatus: "Healthy"},
		{Namespace: "argocd", Name: "a", SyncStatus: "Synced", HealthStatus: "Healthy"},
	}
	got := appsTable("tools", "argocd", apps)
	if strings.Index(got, "argocd/a") > strings.Index(got, "argocd/z") {
		t.Fatalf("apps not sorted: %s", got)
	}
	if empty := appsTable("tools", "argocd", nil); !strings.Contains(empty, "0 application(s)") {
		t.Fatalf("empty table wrong: %s", empty)
	}
}

func TestAppsTableShowsRunningOperation(t *testing.T) {
	apps := []*argocd.App{{
		Namespace: "argocd", Name: "busy", SyncStatus: "OutOfSync", HealthStatus: "Progressing",
		Operation: &argocd.OperationState{Phase: argocd.PhaseRunning},
	}}
	if got := appsTable("tools", "argocd", apps); !strings.Contains(got, "op=Running") {
		t.Fatalf("running operation not shown: %s", got)
	}
}

func TestErrorResultIsToolError(t *testing.T) {
	res := errorResult(errors.New("boom"))
	if !res.IsError {
		t.Fatal("errorResult must set IsError so the model sees the message")
	}
}

func TestDash(t *testing.T) {
	if dash("") != "-" || dash("x") != "x" {
		t.Fatal("dash broken")
	}
}

func TestJoinOrAndIndent(t *testing.T) {
	if got := joinOr(nil, "none"); got != "none" {
		t.Fatalf("joinOr(nil) = %q", got)
	}
	if got := indent("a\nb\n", "  "); got != "  a\n  b\n" {
		t.Fatalf("indent = %q", got)
	}
}
