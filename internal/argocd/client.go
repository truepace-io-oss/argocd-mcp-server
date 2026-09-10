package argocd

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// ListApps lists Applications in one namespace.
func ListApps(ctx context.Context, dyn dynamic.Interface, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return dyn.Resource(AppGVR).Namespace(namespace).List(ctx, opts)
}

// GetApp fetches one Application.
func GetApp(ctx context.Context, dyn dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	return dyn.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// PatchApp applies a patch to one Application. Every mutation this server
// performs is a JSON merge patch on the main resource — the Application CRD
// declares no status subresource, so status is writable this way too, and the
// ServiceAccount needs only the `patch` verb on `applications`.
func PatchApp(ctx context.Context, dyn dynamic.Interface, namespace, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error) {
	return dyn.Resource(AppGVR).Namespace(namespace).Patch(ctx, name, pt, data, metav1.PatchOptions{})
}

// ListProjects lists AppProjects in one namespace.
func ListProjects(ctx context.Context, dyn dynamic.Interface, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return dyn.Resource(ProjectGVR).Namespace(namespace).List(ctx, opts)
}

// ListAppSets lists ApplicationSets in one namespace.
func ListAppSets(ctx context.Context, dyn dynamic.Interface, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return dyn.Resource(AppSetGVR).Namespace(namespace).List(ctx, opts)
}
