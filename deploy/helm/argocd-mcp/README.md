# argocd-mcp Helm chart

Deploys the **argocd-mcp** MCP server: an AI agent inspects and operates one or
more Argo CD installations through it. Every instance is reached through the
Kubernetes API server of its own cluster — no Argo CD ingress, VPN or API token
is involved — and **Kubernetes RBAC on `argoproj.io` is the only authorization
gate**. Remote-instance credentials are provided via the External Secrets Operator
(ESO), from whichever backend you use (Vault, Bitwarden Secrets Manager, AWS
Secrets Manager, …).

```bash
helm upgrade --install argocd-mcp oci://ghcr.io/truepace-io-oss/charts/argocd-mcp \
  --namespace argocd-mcp --create-namespace \
  --set localInstance.namespace=argocd
```

## What gets rendered

| Object | When |
|--------|------|
| `Deployment`, `Service` | always |
| `ServiceAccount` | always — the in-cluster identity for the local instance |
| `Role` + `RoleBinding` (one per Argo CD namespace) | `localInstance.rbac.tier` ≠ `none` |
| `ConfigMap` (`config.yaml`) | always — server + instance registry (non-secret) |
| `ExternalSecret` (per remote instance) | `externalSecrets.enabled` + `remoteInstances[].eso` |
| `ExternalSecret` (per static auth token) | `auth.static` + `eso.ref` |
| `ExternalSecret` (image pull / ingress basic-auth) | when the matching `esoRef` is set |
| `Ingress` | `ingress.enabled` |
| `ServiceMonitor` | `serviceMonitor.enabled` |
| `ConfigMap` (Grafana dashboard) | `grafanaDashboard.enabled` |

Nothing cluster-scoped is ever created.

## Local-instance RBAC tiers (`localInstance.rbac.tier`)

| Tier | Rules |
|------|-------|
| `read-only` (default) | `get, list, watch` on `applications`, `appprojects`, `applicationsets` |
| `sync` | the above plus `patch` on `applications` — needed by `app_sync`, `app_refresh`, `app_rollback`, `app_terminate_operation` |
| `none` | nothing rendered — bring your own Role |

One `Role`/`RoleBinding` pair is rendered per namespace in
`localInstance.namespace` ∪ `localInstance.applicationNamespaces`.
`create`, `update` and `delete` on `applications` are never granted.

`localInstance.rbac.versionProbe` (default `true`) adds `get` on the single
Deployment `argocd-server`, used only to show the Argo CD version in
`instances_list`.

> Writing the `Application` CR bypasses Argo CD's own RBAC (`argocd-rbac-cm`).
> The Role above is the entire blast radius — see `docs/rbac.md`.

## Managing remote Argo CD instances via ESO

```yaml
externalSecrets:
  enabled: true
  secretStore:
    kind: ClusterSecretStore
    name: secret-store

remoteInstances:
  - name: cluster-b
    server: https://api.cluster-a.example.com:6443
    namespace: argocd
    readOnly: true
    eso:
      tokenRef: <backend-uuid-for-token>
      caRef: <backend-uuid-for-ca.crt>
```

Each remote instance's Secret is mounted at
`/etc/amcp/instances/<name>/{token,ca.crt}` and referenced from `config.yaml` as
`tokenFile`/`certificateAuthorityFile`, so rotation needs no restart. Without ESO
set `existingSecret` on the entry (keys `token` and `ca.crt`).

Provision those ServiceAccounts and tokens in the target clusters with the
manifests in `deploy/rbac/` (`read-only/` or `sync/`) plus
`deploy/rbac/extract-credentials.sh`.

## Agent authentication

Optional client-side auth, independent of the instance credentials. Disabled by
default — only acceptable behind a trusted internal ingress.

```yaml
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "https://auth.example.com/application/o/argocd-mcp/"
    audience: "argocd-mcp"          # == the Authentik client_id
    requiredGroups: ["argocd-mcp-users"]
```

The verified caller is attributed on every sync/rollback as
`operation.initiatedBy.username` (`argocd-mcp:<user>`). See `docs/auth.md`.

## Read-only kill-switches

* `config.readOnly: true` — blocks every mutating tool server-wide.
* `remoteInstances[].readOnly` / `localInstance.readOnly` — per instance.

Both act *before* any API call and are counted in `amcp_writes_blocked_total`.

## Metrics

Served on a separate, unauthenticated port (`metrics.port`, default 9091). Never
route it through the ingress. `serviceMonitor.enabled=true` adds scraping;
`grafanaDashboard.enabled=true` ships the dashboard as a sidecar-discovered
ConfigMap. See `docs/metrics.md`.
