package argocd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGuardValidationsStableAndComplete(t *testing.T) {
	v := GuardValidations()
	if len(v) != 3 {
		t.Fatalf("expected 3 guard rules, got %d", len(v))
	}
	// The three escalation paths that were confirmed exploitable must each be
	// covered; a spec-only guard is provably insufficient.
	want := []string{"object.spec", "operation.sync", "status.history"}
	for i, w := range want {
		if !strings.Contains(v[i].Expression, w) {
			t.Fatalf("rule %d does not cover %q: %s", i, w, v[i].Expression)
		}
		if v[i].Message == "" {
			t.Fatalf("rule %d has no message", i)
		}
	}
}

// The nested has() guards are load-bearing: has() errors when an intermediate
// field is missing, and with failurePolicy=Fail an error denies the request.
func TestOperationExpressionGuardsIntermediateFields(t *testing.T) {
	e := GuardExprNoOperationSourceOverride
	for _, needed := range []string{"!has(object.operation)", "!has(object.operation.sync)"} {
		if !strings.Contains(e, needed) {
			t.Fatalf("expression must short-circuit on %s to avoid denying legitimate operations: %s", needed, e)
		}
	}
}

func TestGuardProbePatchIsInert(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(GuardProbePatch(), &m); err != nil {
		t.Fatalf("probe patch is not valid JSON: %v", err)
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		t.Fatalf("probe must target spec (that is what the guard protects): %v", m)
	}
	// It must not carry a source: even if the guard were missing and the dry-run
	// flag were lost, the probe must not be able to redirect a deployment.
	for _, forbidden := range []string{"source", "sources"} {
		if _, bad := spec[forbidden]; bad {
			t.Fatalf("probe patch must never contain %q", forbidden)
		}
	}
	if len(m) != 1 {
		t.Fatalf("probe patch should touch spec only, got %v", m)
	}
}

func TestGuardStateString(t *testing.T) {
	cases := map[GuardState]string{GuardActive: "active", GuardMissing: "MISSING", GuardUnknown: "unknown"}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Fatalf("GuardState(%d) = %q, want %q", st, got, want)
		}
	}
	// The zero value must be "unknown": an unprobed guard must never read as safe.
	var zero GuardState
	if zero != GuardUnknown {
		t.Fatal("zero value of GuardState must be GuardUnknown")
	}
}
