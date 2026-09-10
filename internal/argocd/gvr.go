// Package argocd is the domain layer: it knows the Argo CD custom resources,
// how to project them into small Go structs the MCP can render, and how to
// build the exact merge patches the Argo CD API server itself writes when a
// user syncs, refreshes, rolls back or terminates an operation.
//
// It deliberately does NOT depend on the argo-cd Go module (which pulls in an
// enormous transitive tree); everything is done through unstructured access.
package argocd

import "k8s.io/apimachinery/pkg/runtime/schema"

const (
	// Group and Version of the Argo CD custom resources.
	Group   = "argoproj.io"
	Version = "v1alpha1"

	// AnnotationRefresh asks the application controller to re-compare the
	// application against its source. The controller removes the annotation
	// once it has reconciled.
	AnnotationRefresh = "argocd.argoproj.io/refresh"

	// RefreshNormal re-compares against the cached manifests; RefreshHard also
	// invalidates the repo-server manifest cache.
	RefreshNormal = "normal"
	RefreshHard   = "hard"

	// Operation phases as reported in status.operationState.phase.
	PhaseRunning     = "Running"
	PhaseTerminating = "Terminating"
	PhaseFailed      = "Failed"
	PhaseError       = "Error"
	PhaseSucceeded   = "Succeeded"

	// Sync and health status values used by the guards and by app_wait.
	SyncSynced    = "Synced"
	HealthHealthy = "Healthy"
	StrategyApply = "apply"
	StrategyHook  = "hook"
)

var (
	// AppGVR is the Application resource (namespaced).
	AppGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "applications"}
	// ProjectGVR is the AppProject resource (namespaced).
	ProjectGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "appprojects"}
	// AppSetGVR is the ApplicationSet resource (namespaced).
	AppSetGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "applicationsets"}
)
