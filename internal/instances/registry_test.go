package instances

import (
	"strings"
	"testing"

	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
)

func TestRegistryGet(t *testing.T) {
	a := NewForTest("tools", "argocd", newFakeDynamic(), false)
	b := NewForTest("demo", "argocd", newFakeDynamic(), true)
	r := NewRegistryForTest("tools", a, b)

	if got, err := r.Get(""); err != nil || got.Name != "tools" {
		t.Fatalf("empty name should resolve to the default: %v %v", got, err)
	}
	if got, err := r.Get("demo"); err != nil || got.Name != "demo" {
		t.Fatalf("Get(demo) = %v %v", got, err)
	}
	_, err := r.Get("nope")
	if err == nil || !strings.Contains(err.Error(), "unknown Argo CD instance") {
		t.Fatalf("expected unknown-instance error, got %v", err)
	}
	if !strings.Contains(err.Error(), "tools") || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("error should list configured instances: %v", err)
	}
	if r.DefaultName() != "tools" || r.Default().Name != "tools" {
		t.Fatal("default instance wrong")
	}
}

func TestRegistryAllSorted(t *testing.T) {
	r := NewRegistryForTest("z",
		NewForTest("z", "argocd", newFakeDynamic(), false),
		NewForTest("a", "argocd", newFakeDynamic(), false),
		NewForTest("m", "argocd", newFakeDynamic(), false),
	)
	got := []string{}
	for _, in := range r.All() {
		got = append(got, in.Name)
	}
	if strings.Join(got, ",") != "a,m,z" {
		t.Fatalf("All() = %v, want sorted", got)
	}
	// Names() keeps configuration order.
	if strings.Join(r.Names(), ",") != "z,a,m" {
		t.Fatalf("Names() = %v, want configuration order", r.Names())
	}
}

func TestBuildRejectsUnknownDefault(t *testing.T) {
	// A kubeconfig-based instance is buildable without a cluster, but the
	// default name check must still fire.
	cfg := &config.Config{
		DefaultInstance: "ghost",
		Instances:       []config.InstanceConfig{{Name: "real", Server: "https://x:6443", Token: "t", InsecureSkipTLSVerify: true, Namespace: "argocd"}},
	}
	if _, err := Build(cfg); err == nil || !strings.Contains(err.Error(), "not found after build") {
		t.Fatalf("expected defaultInstance error, got %v", err)
	}
}

func TestBuildInstanceFields(t *testing.T) {
	cfg := &config.Config{
		DefaultInstance: "remote",
		Instances: []config.InstanceConfig{{
			Name: "remote", Server: "https://x:6443", Token: "t", InsecureSkipTLSVerify: true,
			Namespace: "argocd", ApplicationNamespaces: []string{"team-a"}, ReadOnly: true,
		}},
	}
	reg, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	in := reg.Default()
	if in.Namespace != "argocd" || len(in.Namespaces) != 2 || in.Namespaces[1] != "team-a" || !in.ReadOnly {
		t.Fatalf("instance built wrong: %+v", in)
	}
	if in.RESTConfig.Host != "https://x:6443" || in.RESTConfig.UserAgent != "argocd-mcp" {
		t.Fatalf("rest config wrong: %+v", in.RESTConfig)
	}
	if !in.RESTConfig.TLSClientConfig.Insecure {
		t.Fatal("insecureSkipTLSVerify not propagated")
	}
}

func TestRestConfigCADataDecoding(t *testing.T) {
	_, err := restConfigFor(config.InstanceConfig{Name: "x", Server: "https://x:6443", Token: "t", CertificateAuthorityData: "not-base64!!"})
	if err == nil || !strings.Contains(err.Error(), "not valid base64") {
		t.Fatalf("expected base64 error, got %v", err)
	}
	rc, err := restConfigFor(config.InstanceConfig{Name: "x", Server: "https://x:6443", TokenFile: "/etc/amcp/token", CertificateAuthorityFile: "/etc/amcp/ca.crt"})
	if err != nil {
		t.Fatalf("restConfigFor: %v", err)
	}
	if rc.BearerTokenFile != "/etc/amcp/token" || rc.TLSClientConfig.CAFile != "/etc/amcp/ca.crt" {
		t.Fatalf("file-based credentials not used: %+v", rc)
	}
}
