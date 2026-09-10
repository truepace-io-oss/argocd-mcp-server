package mcpserver

import (
	"context"
	"errors"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClassifyResult(t *testing.T) {
	cases := []struct {
		name string
		res  *mcp.CallToolResult
		err  error
		want string
	}{
		{"ok", textResult("all good"), nil, "ok"},
		{"nil result", nil, nil, "ok"},
		{"protocol error", nil, errors.New("boom"), "error"},
		{"forbidden", errorResult(errors.New(`applications.argoproj.io is forbidden`)), nil, "forbidden"},
		{"conflict", errorResult(errors.New("another operation is already in progress on argocd/x")), nil, "conflict"},
		{"blocked global", errorResult(errors.New("this MCP instance is configured read-only")), nil, "blocked"},
		{"blocked instance", errorResult(errors.New(`writes are disabled for Argo CD instance "x" (readOnly)`)), nil, "blocked"},
		{"other", errorResult(errors.New("not found")), nil, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyResult(c.res, c.err); got != c.want {
				t.Fatalf("classifyResult = %q, want %q", got, c.want)
			}
		})
	}
}

func TestInstanceLabel(t *testing.T) {
	s, _ := buildTestServer(t, false) // default instance = "tools"
	if got := s.instanceLabel(instanceParam{Instance: "demo"}); got != "demo" {
		t.Fatalf("explicit instance label = %q", got)
	}
	if got := s.instanceLabel(instanceParam{}); got != "tools" {
		t.Fatalf("empty instance should map to the default, got %q", got)
	}
	if got := s.instanceLabel(struct{}{}); got != "-" {
		t.Fatalf("input without an instance should map to '-', got %q", got)
	}
	// Promoted through the embedded structs the real tools use.
	if got := s.instanceLabel(appRef{instanceParam: instanceParam{Instance: "demo"}}); got != "demo" {
		t.Fatalf("appRef should promote metricInstance, got %q", got)
	}
	if got := s.instanceLabel(appsListParam{}); got != "tools" {
		t.Fatalf("appsListParam should promote metricInstance, got %q", got)
	}
}

// TestInitiatorFallback covers the unauthenticated path. The authenticated path
// cannot be constructed here — the go-sdk stores TokenInfo under an unexported
// context key with no exported setter — so it is asserted end-to-end in
// test/e2e/e2e_auth_test.go, which drives the real bearer-token middleware.
func TestInitiatorFallback(t *testing.T) {
	if got := initiator(context.Background()); got != "argocd-mcp" {
		t.Fatalf("initiator without auth = %q", got)
	}
	if info := sdkauth.TokenInfoFromContext(context.Background()); info != nil {
		t.Fatalf("bare context should carry no token info, got %+v", info)
	}
}
