package argocd

// This file is the single source of truth for the *write guard*: the
// ValidatingAdmissionPolicy that constrains what the argocd-mcp ServiceAccount
// may change on an Application.
//
// Why it exists
// -------------
// Kubernetes RBAC is resource-scoped, not field-scoped: granting `patch` on
// `applications` grants write access to the WHOLE object. That is a privilege
// escalation path, because three different fields can redirect a sync at an
// attacker-controlled source:
//
//  1. spec.source / spec.sources          — the application's own source
//  2. operation.sync.source / .sources    — Argo CD: "Source overrides the
//     source definition set in the
//     application"
//  3. status.history[].source             — app_rollback reads history to build
//     the sync, so poisoned history is
//     laundered into a real deployment
//
// The controller then applies those manifests with ITS privileges, which are
// typically cluster-admin. Guarding only `spec` is therefore not enough — this
// was verified empirically against a real Argo CD: with a spec-only policy,
// (2) and (3) both went through.
//
// A ValidatingAdmissionPolicy closes all three, because admission (unlike
// authorization) sees the old object, the new object and the requesting user.
//
// The expressions below are mirrored in:
//   - deploy/helm/argocd-mcp/templates/rbac.yaml           (local instance)
//   - environments prototypes/argocd-mcp-agent/ytt/policy.yaml (remote clusters)
//
// They are asserted end-to-end against a real cluster in
// test/e2e/kind/e2e_guard_test.go, and verified at runtime on every managed
// instance by Instance.CheckWriteGuard.
const (
	// GuardExprSpecUnchanged pins the application's own source definition.
	GuardExprSpecUnchanged = `object.spec == oldObject.spec`

	// GuardExprNoOperationSourceOverride forbids redirecting the sync from
	// inside the operation. The nested has() guards matter: has() errors when an
	// intermediate field is missing, and with failurePolicy=Fail an error denies
	// the request — which would break legitimate operations that carry no sync.
	GuardExprNoOperationSourceOverride = `!has(object.operation) || !has(object.operation.sync) || ` +
		`(!has(object.operation.sync.source) && !has(object.operation.sync.sources))`

	// GuardExprHistoryUnchanged stops history poisoning, which app_rollback
	// would otherwise turn into a sync from an attacker-chosen source.
	GuardExprHistoryUnchanged = `(has(object.status) && has(object.status.history) ? object.status.history : []) == ` +
		`(has(oldObject.status) && has(oldObject.status.history) ? oldObject.status.history : [])`

	// Messages surfaced to whoever tripped the policy.
	GuardMsgSpec      = "argocd-mcp may not modify Application.spec"
	GuardMsgOperation = "argocd-mcp may not override the source inside an operation"
	GuardMsgHistory   = "argocd-mcp may not modify status.history"

	// PolicyName is the name of the ValidatingAdmissionPolicy and its binding.
	PolicyName = "argocd-mcp-write-guard"
)

// GuardValidation is one CEL rule of the write guard.
type GuardValidation struct {
	Expression string
	Message    string
}

// GuardValidations returns the guard's rules in a stable order, so the chart,
// the GitOps prototype and the tests all render the same policy.
func GuardValidations() []GuardValidation {
	return []GuardValidation{
		{GuardExprSpecUnchanged, GuardMsgSpec},
		{GuardExprNoOperationSourceOverride, GuardMsgOperation},
		{GuardExprHistoryUnchanged, GuardMsgHistory},
	}
}

// GuardProbePatch is a deliberately forbidden patch used to verify at runtime
// that the guard is in force. It is only ever sent with dryRun=All, so it never
// changes anything: it asks the API server "would you accept a spec rewrite?"
// and the only acceptable answer is no.
//
// The value is inert even if a guard were missing and the dry-run flag were
// somehow lost: it sets spec.project to the name it already cannot be, rather
// than pointing the application at a different source.
func GuardProbePatch() []byte {
	return []byte(`{"spec":{"project":"argocd-mcp-write-guard-probe"}}`)
}

// GuardState is the outcome of a write-guard probe.
type GuardState int

const (
	// GuardUnknown means the probe could not be performed (e.g. no Application
	// exists to probe against).
	GuardUnknown GuardState = iota
	// GuardActive means a forbidden spec rewrite was rejected — by the admission
	// policy, or by RBAC not granting patch at all. Either way it cannot happen.
	GuardActive
	// GuardMissing means a forbidden spec rewrite was ACCEPTED in dry-run: this
	// instance's credentials can rewrite an Application's source.
	GuardMissing
)

func (g GuardState) String() string {
	switch g {
	case GuardActive:
		return "active"
	case GuardMissing:
		return "MISSING"
	default:
		return "unknown"
	}
}
