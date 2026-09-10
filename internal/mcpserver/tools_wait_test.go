package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestAppWaitAlreadySynced(t *testing.T) {
	s, _ := buildTestServer(t, false)
	res, _, _ := s.appWait(ctx(), nil, appWaitParam{appRef: appRef{Name: "frontend"}})
	if res.IsError {
		t.Fatalf("wait on an already-healthy app should succeed: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "reached sync=Synced health=Healthy") {
		t.Fatalf("result wrong: %s", text(t, res))
	}
}

func TestAppWaitPollsUntilSynced(t *testing.T) {
	s, dyn := buildTestServer(t, false,
		newApp(appOpts{name: "slow", sync: "OutOfSync", health: "Progressing", opPhase: argocd.PhaseRunning}))
	reads := 0
	dyn.PrependReactor("get", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads < 2 {
			return false, nil, nil // first read: the seeded, still-progressing object
		}
		return true, newApp(appOpts{name: "slow", sync: "Synced", health: "Healthy", revision: "abc"}), nil
	})
	res, _, _ := s.appWait(ctx(), nil, appWaitParam{appRef: appRef{Name: "slow"}, TimeoutSeconds: 10, PollSeconds: 1})
	if res.IsError {
		t.Fatalf("wait should have succeeded after the second poll: %s", text(t, res))
	}
	if reads < 2 {
		t.Fatalf("expected at least two reads, got %d", reads)
	}
}

func TestAppWaitFailsFastOnFailedOperation(t *testing.T) {
	s, _ := buildTestServer(t, false,
		newApp(appOpts{name: "broken", sync: "OutOfSync", health: "Degraded", opPhase: argocd.PhaseFailed}))
	res, _, _ := s.appWait(ctx(), nil, appWaitParam{appRef: appRef{Name: "broken"}, TimeoutSeconds: 10})
	if !res.IsError || !strings.Contains(text(t, res), "ended in phase Failed") {
		t.Fatalf("expected an immediate failure, got: %s", text(t, res))
	}
}

func TestAppWaitTimeout(t *testing.T) {
	s, _ := buildTestServer(t, false,
		newApp(appOpts{name: "stuck", sync: "OutOfSync", health: "Progressing"}))
	start := time.Now()
	res, _, _ := s.appWait(ctx(), nil, appWaitParam{appRef: appRef{Name: "stuck"}, TimeoutSeconds: 1, PollSeconds: 1})
	if !res.IsError || !strings.Contains(text(t, res), "timed out after 1s") {
		t.Fatalf("expected a timeout error, got: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "sync=OutOfSync") {
		t.Fatalf("timeout should report the last observed state: %s", text(t, res))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestAppWaitForHealthyFalse(t *testing.T) {
	s, _ := buildTestServer(t, false,
		newApp(appOpts{name: "degraded", sync: "Synced", health: "Degraded"}))
	no := false
	res, _, _ := s.appWait(ctx(), nil, appWaitParam{appRef: appRef{Name: "degraded"}, ForHealthy: &no, TimeoutSeconds: 2})
	if res.IsError {
		t.Fatalf("forHealthy=false should accept a Synced but Degraded app: %s", text(t, res))
	}
}

func TestAppWaitRespectsCancellation(t *testing.T) {
	s, _ := buildTestServer(t, false,
		newApp(appOpts{name: "stuck", sync: "OutOfSync", health: "Progressing"}))
	c, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	res, _, _ := s.appWait(c, nil, appWaitParam{appRef: appRef{Name: "stuck"}, TimeoutSeconds: 60, PollSeconds: 1})
	if !res.IsError {
		t.Fatalf("cancelled wait should return an error, got: %s", text(t, res))
	}
}

func TestClamp(t *testing.T) {
	cases := [][4]int{
		{0, 1, 30, 3},   // unset -> default
		{-5, 1, 30, 3},  // negative -> default
		{100, 1, 30, 3}, // above max -> max
		{2, 1, 30, 3},   // in range -> itself
	}
	wants := []int{3, 3, 30, 2}
	for i, c := range cases {
		if got := clamp(c[0], c[1], c[2], c[3]); got != wants[i] {
			t.Fatalf("clamp%v = %d, want %d", c, got, wants[i])
		}
	}
	if got := clamp(0, 5, 30, 1); got != 1 {
		t.Fatalf("default wins over min: %d", got)
	}
}
