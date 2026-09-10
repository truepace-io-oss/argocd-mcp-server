package instances

import (
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// listKinds maps the Argo CD GVRs to their list kinds; the fake dynamic client
// needs this because it has no discovery.
var listKinds = map[schema.GroupVersionResource]string{
	argocd.AppGVR:     "ApplicationList",
	argocd.ProjectGVR: "AppProjectList",
	argocd.AppSetGVR:  "ApplicationSetList",
}

func newFakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
}

func app(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"project": "default"},
	}}
}
