package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
defaultInstance: local
instances:
  - name: local
    inCluster: true
`

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:9090" || cfg.MetricsAddr != ":9091" || cfg.LogLevel != "info" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.Instances[0].Namespace != DefaultArgoCDNamespace {
		t.Fatalf("namespace default not applied: %q", cfg.Instances[0].Namespace)
	}
	if cfg.Auth.OIDC.GroupsClaim != "groups" || cfg.Auth.OIDC.UsernameClaim != "preferred_username" {
		t.Fatalf("oidc claim defaults not applied: %+v", cfg.Auth.OIDC)
	}
}

func TestSingleInstanceBecomesDefault(t *testing.T) {
	cfg, err := Load(writeCfg(t, "instances:\n  - name: only\n    inCluster: true\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultInstance != "only" {
		t.Fatalf("single instance should become the default, got %q", cfg.DefaultInstance)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("AMCP_LOG_LEVEL", "debug")
	t.Setenv("AMCP_READ_ONLY", "true")
	t.Setenv("AMCP_LISTEN_ADDR", "127.0.0.1:1234")
	t.Setenv("AMCP_METRICS_ADDR", "off")
	t.Setenv("AMCP_AUTH_STATIC_TOKEN", "s3cret")
	t.Setenv("AMCP_AUTH_ENABLED", "true")

	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LogLevel != "debug" || !cfg.ReadOnly || cfg.ListenAddr != "127.0.0.1:1234" || cfg.MetricsAddr != "off" {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
	if !cfg.Auth.Enabled || !cfg.Auth.Static.Enabled || len(cfg.Auth.Static.Tokens) != 1 {
		t.Fatalf("static token from env not applied: %+v", cfg.Auth)
	}
}

func TestEnvOIDCOverride(t *testing.T) {
	t.Setenv("AMCP_AUTH_ENABLED", "true")
	t.Setenv("AMCP_AUTH_OIDC_ISSUER", "https://auth.example.com/application/o/x/")
	t.Setenv("AMCP_AUTH_OIDC_AUDIENCE", "argocd-mcp")
	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Auth.OIDC.Enabled || cfg.Auth.OIDC.Audience != "argocd-mcp" {
		t.Fatalf("oidc env override not applied: %+v", cfg.Auth.OIDC)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no instances", "defaultInstance: x\ninstances: []\n", "no Argo CD instances configured"},
		{"bad log level", minimal + "logLevel: shout\n", "invalid logLevel"},
		{"empty name", "instances:\n  - inCluster: true\n", "instance with empty name"},
		{"bad name", "instances:\n  - name: Local_1\n    inCluster: true\n", "must be a DNS label"},
		{"duplicate", "defaultInstance: a\ninstances:\n  - name: a\n    inCluster: true\n  - name: a\n    inCluster: true\n", "duplicate instance name"},
		{"no mode", "instances:\n  - name: a\n", "no authentication mode set"},
		{"two modes", "instances:\n  - name: a\n    inCluster: true\n    server: https://x:6443\n", "multiple authentication modes"},
		{"no ca", "instances:\n  - name: a\n    server: https://x:6443\n    token: t\n", "no certificateAuthorityFile/Data"},
		{"no token", "instances:\n  - name: a\n    server: https://x:6443\n    insecureSkipTLSVerify: true\n", "no tokenFile/token"},
		{"insecure plus ca", "instances:\n  - name: a\n    server: https://x:6443\n    token: t\n    insecureSkipTLSVerify: true\n    certificateAuthorityData: eA==\n", "must not be combined with a CA"},
		{"kubeconfig no context", "instances:\n  - name: a\n    kubeconfigFile: /tmp/kc\n", "no context selected"},
		{"unknown default", "defaultInstance: nope\ninstances:\n  - name: a\n    inCluster: true\n  - name: b\n    inCluster: true\n", "is not one of the configured instances"},
		{"missing default", "instances:\n  - name: a\n    inCluster: true\n  - name: b\n    inCluster: true\n", "defaultInstance must be set"},
		{"bad namespace", "instances:\n  - name: a\n    inCluster: true\n    namespace: Bad_NS\n", "must be a DNS label"},
		{"bad app namespace", "instances:\n  - name: a\n    inCluster: true\n    applicationNamespaces: [\"Bad_NS\"]\n", "applicationNamespaces entry"},
		{"auth no verifier", minimal + "auth:\n  enabled: true\n", "neither auth.static nor auth.oidc"},
		{"auth static no tokens", minimal + "auth:\n  enabled: true\n  static:\n    enabled: true\n", "no tokens configured"},
		{"auth token both", minimal + "auth:\n  enabled: true\n  static:\n    enabled: true\n    tokens:\n      - name: a\n        token: x\n        tokenFile: /y\n", "exactly one of token or tokenFile"},
		{"auth oidc no issuer", minimal + "auth:\n  enabled: true\n  oidc:\n    enabled: true\n    audience: a\n", "issuer is empty"},
		{"auth oidc http issuer", minimal + "auth:\n  enabled: true\n  oidc:\n    enabled: true\n    issuer: http://x/\n    audience: a\n", "must be an https URL"},
		{"auth oidc no audience", minimal + "auth:\n  enabled: true\n  oidc:\n    enabled: true\n    issuer: https://x/\n", "audience is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, c.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaultInstance: a
instances:
  - name: a
    server: https://x:6443
    token: inline
    insecureSkipTLSVerify: true
auth:
  enabled: true
  static:
    enabled: true
    tokens:
      - name: t
        token: inline
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	joined := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{"inline token is discouraged", "insecureSkipTLSVerify=true", "auth is enabled"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q: %s", want, joined)
		}
	}
}

func TestNamespaces(t *testing.T) {
	in := InstanceConfig{Name: "a", Namespace: "argocd", ApplicationNamespaces: []string{"team-a", "argocd", "", "team-b"}}
	got := in.Namespaces()
	want := []string{"argocd", "team-a", "team-b"}
	if len(got) != len(want) {
		t.Fatalf("Namespaces() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Namespaces() = %v, want %v", got, want)
		}
	}
	// Empty namespace falls back to the Argo CD default.
	if ns := (InstanceConfig{}).Namespaces(); len(ns) != 1 || ns[0] != DefaultArgoCDNamespace {
		t.Fatalf("default namespace fallback broken: %v", ns)
	}
}

func TestInstanceStringRedacts(t *testing.T) {
	s := InstanceConfig{Name: "a", Server: "https://x:6443", Token: "super-secret", Namespace: "argocd"}.String()
	if strings.Contains(s, "super-secret") {
		t.Fatalf("String() leaked the token: %s", s)
	}
}

func TestLoadWithoutPath(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error when no config and no instances are provided")
	}
}

// TestExampleConfigLoads keeps examples/config.yaml honest: it must always be a
// valid configuration, so the documented example cannot drift from the schema.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatalf("examples/config.yaml does not load: %v", err)
	}
	if len(cfg.Instances) < 3 {
		t.Fatalf("example should document all three credential modes, got %d instances", len(cfg.Instances))
	}
	modes := map[string]bool{}
	for _, in := range cfg.Instances {
		switch {
		case in.InCluster:
			modes["inCluster"] = true
		case in.Server != "":
			modes["server"] = true
		case in.KubeconfigFile != "":
			modes["kubeconfig"] = true
		}
	}
	for _, want := range []string{"inCluster", "server", "kubeconfig"} {
		if !modes[want] {
			t.Fatalf("examples/config.yaml no longer documents the %q credential mode", want)
		}
	}
}

// TestChartRenderedConfigShape mirrors what deploy/helm/argocd-mcp renders, so a
// chart change that breaks the server's schema fails here rather than in a cluster.
func TestChartRenderedConfigShape(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
listenAddr: "0.0.0.0:9090"
metricsAddr: ":9091"
logLevel: "info"
readOnly: false
defaultInstance: "local"
instances:
  - name: "local"
    inCluster: true
    namespace: "argocd"
    applicationNamespaces: ["team-a"]
    readOnly: false
  - name: "demo"
    server: "https://x:6443"
    certificateAuthorityFile: /etc/amcp/instances/demo/ca.crt
    tokenFile: /etc/amcp/instances/demo/token
    namespace: "argocd"
    readOnly: true
auth:
  enabled: true
  static:
    enabled: false
    tokens:
  oidc:
    enabled: true
    issuer: "https://auth.example.com/application/o/argocd-mcp/"
    audience: "argocd-mcp"
    jwksUrl: ""
    groupsClaim: "groups"
    usernameClaim: "preferred_username"
    resourceMetadata: true
    requiredScopes: []
    requiredGroups: []
`))
	if err != nil {
		t.Fatalf("chart-shaped config does not load: %v", err)
	}
	if len(cfg.Instances) != 2 || !cfg.Instances[0].InCluster || cfg.Instances[1].TokenFile == "" {
		t.Fatalf("chart-shaped config parsed wrong: %+v", cfg.Instances)
	}
	if len(cfg.Instances[0].Namespaces()) != 2 {
		t.Fatalf("applicationNamespaces not honoured: %v", cfg.Instances[0].Namespaces())
	}
	if !cfg.Auth.OIDC.Enabled || !cfg.Auth.OIDC.ServeResourceMetadata() {
		t.Fatalf("auth block parsed wrong: %+v", cfg.Auth)
	}
}
