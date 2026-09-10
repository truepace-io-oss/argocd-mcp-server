// Package config loads and validates the argocd-mcp server configuration: the
// listen address, logging, a global read-only kill-switch, and the registry of
// Argo CD instances this server manages.
//
// An "instance" is one Argo CD installation, addressed through the Kubernetes
// API server of the cluster it runs in. Authentication to that cluster is
// expressed purely as ServiceAccount credentials (in-cluster token, an explicit
// token+CA, or a kubeconfig context) — the server contains no authorization
// logic of its own; Kubernetes RBAC on argoproj.io is the only gate.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// DefaultArgoCDNamespace is where Argo CD keeps its Application objects unless
// an instance overrides it.
const DefaultArgoCDNamespace = "argocd"

// Config is the top-level server configuration.
type Config struct {
	ListenAddr      string           `json:"listenAddr"`
	MetricsAddr     string           `json:"metricsAddr"`
	LogLevel        string           `json:"logLevel"`
	ReadOnly        bool             `json:"readOnly"`
	DefaultInstance string           `json:"defaultInstance"`
	Instances       []InstanceConfig `json:"instances"`
	Auth            Auth             `json:"auth"`
}

// Auth configures how the AI agent authenticates to this MCP server (the
// client-side link). It is independent of instance authentication: this decides
// who may talk to the MCP, while ServiceAccount tokens + RBAC decide what the MCP
// may do in a cluster. When disabled, the transport is unauthenticated and must
// be protected by the deployment (e.g. an internal ingress).
type Auth struct {
	Enabled bool       `json:"enabled"`
	Static  AuthStatic `json:"static"`
	OIDC    AuthOIDC   `json:"oidc"`
}

// AuthStatic configures one or more shared bearer tokens.
type AuthStatic struct {
	Enabled bool        `json:"enabled"`
	Tokens  []AuthToken `json:"tokens"`
}

// AuthToken is a single shared bearer token. Exactly one of Token/TokenFile.
type AuthToken struct {
	Name      string `json:"name"`
	Token     string `json:"token,omitempty"`     // inline, discouraged
	TokenFile string `json:"tokenFile,omitempty"` // preferred (ESO / rotatable)
}

// AuthOIDC configures the MCP as an OAuth 2.1 resource server validating JWT
// access tokens from an OIDC provider (Authentik / Keycloak).
type AuthOIDC struct {
	Enabled        bool     `json:"enabled"`
	Issuer         string   `json:"issuer"`
	Audience       string   `json:"audience"`
	JWKSURL        string   `json:"jwksUrl,omitempty"`
	RequiredScopes []string `json:"requiredScopes,omitempty"`
	RequiredGroups []string `json:"requiredGroups,omitempty"`
	GroupsClaim    string   `json:"groupsClaim,omitempty"`
	UsernameClaim  string   `json:"usernameClaim,omitempty"`
	// ResourceMetadata controls serving /.well-known/oauth-protected-resource.
	// Defaults to true (enabled) when unset.
	ResourceMetadata *bool `json:"resourceMetadata,omitempty"`
}

// ServeResourceMetadata reports whether the protected-resource-metadata endpoint
// should be served (defaults to true).
func (o AuthOIDC) ServeResourceMetadata() bool {
	return o.ResourceMetadata == nil || *o.ResourceMetadata
}

// InstanceConfig describes one Argo CD installation: how to reach and
// authenticate to the Kubernetes API server hosting it, and where its
// Application objects live. Exactly one authentication mode must be selected:
//   - InCluster: use the pod's projected ServiceAccount token (the cluster the
//     server runs in).
//   - Server + CA + Token: a remote cluster reached with a SA token.
//   - KubeconfigFile + Context: a context from a mounted kubeconfig.
type InstanceConfig struct {
	Name string `json:"name"`

	// Mode (a): in-cluster ServiceAccount.
	InCluster bool `json:"inCluster,omitempty"`

	// Mode (b): explicit remote cluster.
	Server                   string `json:"server,omitempty"`
	CertificateAuthorityFile string `json:"certificateAuthorityFile,omitempty"`
	CertificateAuthorityData string `json:"certificateAuthorityData,omitempty"` // base64 (PEM) as in kubeconfig
	TokenFile                string `json:"tokenFile,omitempty"`
	Token                    string `json:"token,omitempty"` // inline, discouraged
	InsecureSkipTLSVerify    bool   `json:"insecureSkipTLSVerify,omitempty"`

	// Mode (c): kubeconfig context.
	KubeconfigFile string `json:"kubeconfigFile,omitempty"`
	Context        string `json:"context,omitempty"`

	// Argo CD specifics.
	Namespace             string   `json:"namespace,omitempty"`             // where Applications live; default "argocd"
	ApplicationNamespaces []string `json:"applicationNamespaces,omitempty"` // extra namespaces ("apps in any namespace")

	// Behaviour.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// authMode is an internal enum of the selected authentication mode.
type authMode int

const (
	authNone authMode = iota
	authInCluster
	authExplicit
	authKubeconfig
)

// Mode reports which authentication mode this instance uses and whether the
// selection is unambiguous.
func (c InstanceConfig) Mode() (authMode, error) {
	var modes []authMode
	if c.InCluster {
		modes = append(modes, authInCluster)
	}
	if c.Server != "" {
		modes = append(modes, authExplicit)
	}
	if c.KubeconfigFile != "" {
		modes = append(modes, authKubeconfig)
	}
	switch len(modes) {
	case 0:
		return authNone, fmt.Errorf("instance %q: no authentication mode set (need one of inCluster / server / kubeconfigFile)", c.Name)
	case 1:
		return modes[0], nil
	default:
		return authNone, fmt.Errorf("instance %q: multiple authentication modes set; choose exactly one of inCluster / server / kubeconfigFile", c.Name)
	}
}

// Namespaces returns every namespace this instance searches for Applications:
// the primary namespace first, then any additional ones, de-duplicated.
func (c InstanceConfig) Namespaces() []string {
	primary := c.Namespace
	if primary == "" {
		primary = DefaultArgoCDNamespace
	}
	out := []string{primary}
	seen := map[string]bool{primary: true}
	for _, ns := range c.ApplicationNamespaces {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	return out
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Load reads config from path (if non-empty), applies AMCP_* environment
// overrides, fills defaults and validates the result.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	cfg.applyEnv()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("AMCP_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("AMCP_METRICS_ADDR"); v != "" {
		c.MetricsAddr = v
	}
	if v := os.Getenv("AMCP_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("AMCP_DEFAULT_INSTANCE"); v != "" {
		c.DefaultInstance = v
	}
	if v := os.Getenv("AMCP_READ_ONLY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.ReadOnly = b
		}
	}
	if v := os.Getenv("AMCP_AUTH_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Auth.Enabled = b
		}
	}
	if v := os.Getenv("AMCP_AUTH_STATIC_TOKEN"); v != "" {
		c.Auth.Static.Enabled = true
		c.Auth.Static.Tokens = append(c.Auth.Static.Tokens, AuthToken{Name: "env", Token: v})
	}
	if v := os.Getenv("AMCP_AUTH_OIDC_ISSUER"); v != "" {
		c.Auth.OIDC.Enabled = true
		c.Auth.OIDC.Issuer = v
	}
	if v := os.Getenv("AMCP_AUTH_OIDC_AUDIENCE"); v != "" {
		c.Auth.OIDC.Audience = v
	}
}

func (c *Config) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = "0.0.0.0:9090"
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9091" // separate port; set "off" to disable
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	for i := range c.Instances {
		if c.Instances[i].Namespace == "" {
			c.Instances[i].Namespace = DefaultArgoCDNamespace
		}
	}
	// If exactly one instance is defined and no default is set, use it.
	if c.DefaultInstance == "" && len(c.Instances) == 1 {
		c.DefaultInstance = c.Instances[0].Name
	}
	if c.Auth.OIDC.GroupsClaim == "" {
		c.Auth.OIDC.GroupsClaim = "groups"
	}
	if c.Auth.OIDC.UsernameClaim == "" {
		c.Auth.OIDC.UsernameClaim = "preferred_username"
	}
}

// Validate enforces the invariants documented on Config/InstanceConfig. It
// returns the first violation found. Non-fatal normalisations (file-vs-inline
// precedence) are handled at credential-build time, not here.
func (c *Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid logLevel %q (want debug|info|warn|error)", c.LogLevel)
	}
	if len(c.Instances) == 0 {
		return fmt.Errorf("no Argo CD instances configured")
	}

	seen := map[string]bool{}
	for _, in := range c.Instances {
		if in.Name == "" {
			return fmt.Errorf("instance with empty name")
		}
		if !nameRe.MatchString(in.Name) {
			return fmt.Errorf("instance %q: name must be a DNS label (lowercase alphanumeric and '-')", in.Name)
		}
		if seen[in.Name] {
			return fmt.Errorf("duplicate instance name %q", in.Name)
		}
		seen[in.Name] = true

		if in.Namespace != "" && !nameRe.MatchString(in.Namespace) {
			return fmt.Errorf("instance %q: namespace %q must be a DNS label", in.Name, in.Namespace)
		}
		for _, ns := range in.ApplicationNamespaces {
			if !nameRe.MatchString(ns) {
				return fmt.Errorf("instance %q: applicationNamespaces entry %q must be a DNS label", in.Name, ns)
			}
		}

		mode, err := in.Mode()
		if err != nil {
			return err
		}
		switch mode {
		case authExplicit:
			if in.CertificateAuthorityFile == "" && in.CertificateAuthorityData == "" && !in.InsecureSkipTLSVerify {
				return fmt.Errorf("instance %q: server set but no certificateAuthorityFile/Data and insecureSkipTLSVerify is false", in.Name)
			}
			if in.TokenFile == "" && in.Token == "" {
				return fmt.Errorf("instance %q: server set but no tokenFile/token provided", in.Name)
			}
			if in.InsecureSkipTLSVerify && (in.CertificateAuthorityFile != "" || in.CertificateAuthorityData != "") {
				return fmt.Errorf("instance %q: insecureSkipTLSVerify must not be combined with a CA", in.Name)
			}
		case authKubeconfig:
			if in.Context == "" {
				return fmt.Errorf("instance %q: kubeconfigFile set but no context selected", in.Name)
			}
		}
	}

	if c.DefaultInstance == "" {
		return fmt.Errorf("defaultInstance must be set when more than one instance is configured")
	}
	if !seen[c.DefaultInstance] {
		return fmt.Errorf("defaultInstance %q is not one of the configured instances", c.DefaultInstance)
	}

	return c.Auth.validate()
}

func (a Auth) validate() error {
	if !a.Enabled {
		return nil
	}
	if !a.Static.Enabled && !a.OIDC.Enabled {
		return fmt.Errorf("auth.enabled is true but neither auth.static nor auth.oidc is enabled")
	}
	if a.Static.Enabled {
		if len(a.Static.Tokens) == 0 {
			return fmt.Errorf("auth.static.enabled is true but no tokens configured")
		}
		names := map[string]bool{}
		for i, t := range a.Static.Tokens {
			if t.Name == "" {
				return fmt.Errorf("auth.static.tokens[%d]: name is required", i)
			}
			if names[t.Name] {
				return fmt.Errorf("auth.static.tokens: duplicate name %q", t.Name)
			}
			names[t.Name] = true
			if (t.Token == "") == (t.TokenFile == "") {
				return fmt.Errorf("auth.static.tokens[%q]: set exactly one of token or tokenFile", t.Name)
			}
		}
	}
	if a.OIDC.Enabled {
		if a.OIDC.Issuer == "" {
			return fmt.Errorf("auth.oidc.enabled is true but issuer is empty")
		}
		if !strings.HasPrefix(a.OIDC.Issuer, "https://") {
			return fmt.Errorf("auth.oidc.issuer must be an https URL")
		}
		if a.OIDC.Audience == "" {
			return fmt.Errorf("auth.oidc.enabled is true but audience is empty")
		}
	}
	return nil
}

// Warnings returns non-fatal advisories (loud but not blocking), e.g. inline
// secrets or insecure TLS. Callers log these at startup.
func (c *Config) Warnings() []string {
	var w []string
	for _, in := range c.Instances {
		if in.Token != "" && in.TokenFile != "" {
			w = append(w, fmt.Sprintf("instance %q: both token and tokenFile set; tokenFile takes precedence", in.Name))
		}
		if in.CertificateAuthorityData != "" && in.CertificateAuthorityFile != "" {
			w = append(w, fmt.Sprintf("instance %q: both certificateAuthorityData and certificateAuthorityFile set; file takes precedence", in.Name))
		}
		if in.Token != "" {
			w = append(w, fmt.Sprintf("instance %q: inline token is discouraged; prefer tokenFile (ESO/projected token, auto-reloaded)", in.Name))
		}
		if in.InsecureSkipTLSVerify {
			w = append(w, fmt.Sprintf("instance %q: insecureSkipTLSVerify=true — TLS verification disabled, do not use in production", in.Name))
		}
	}
	if c.Auth.Static.Enabled {
		for _, t := range c.Auth.Static.Tokens {
			if t.Token != "" {
				w = append(w, fmt.Sprintf("auth.static.tokens[%q]: inline token is discouraged; prefer tokenFile (ESO/rotatable)", t.Name))
			}
		}
	}
	if c.Auth.Enabled {
		w = append(w, "auth is enabled — ensure the transport is TLS-protected (internal ingress or server TLS); bearer tokens over plaintext are insecure")
	}
	return w
}

// InstanceNames returns the configured instance names in order.
func (c *Config) InstanceNames() []string {
	names := make([]string, 0, len(c.Instances))
	for _, in := range c.Instances {
		names = append(names, in.Name)
	}
	return names
}

// String redacts secrets for safe logging.
func (c InstanceConfig) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "name=%s", c.Name)
	switch {
	case c.InCluster:
		b.WriteString(" mode=in-cluster")
	case c.Server != "":
		fmt.Fprintf(&b, " mode=explicit server=%s", c.Server)
	case c.KubeconfigFile != "":
		fmt.Fprintf(&b, " mode=kubeconfig context=%s", c.Context)
	}
	fmt.Fprintf(&b, " namespace=%s readOnly=%t", c.Namespace, c.ReadOnly)
	return b.String()
}
