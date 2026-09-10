# Deploying via a GitOps repo (myks + ytt + Helm + Argo CD)

The chart is self-contained, so any deployment mechanism works. This document
describes the wiring for a **myks**-based GitOps repository, which is the shape
this server was built against: prototypes under `prototypes/<app>/`, environments
under `envs/<env>/`, rendered manifests committed to `rendered/` and consumed by
Argo CD.

Adapt the names to your own repo — `tools` below is the cluster the MCP runs in,
and `cluster-a` … `cluster-c` are the clusters whose Argo CD it also manages.

## 1. Give each remote cluster an identity — `argocd-mcp-agent`

The MCP lives in one cluster (`tools` below) and reaches every other cluster
through that cluster's kube-apiserver. Each remote cluster therefore needs a
ServiceAccount with RBAC on `argoproj.io` **in its `argocd` namespace**, plus a
static token Secret so the credential can be extracted once.

`prototypes/argocd-mcp-agent/` provides exactly that:

```yaml
application:
  argocdMcpAgent:
    namespace: argocd-mcp
    argocdNamespace: argocd
    serviceAccountName: argocd-mcp
    allowSync: false        # true adds `patch` on applications
    createStaticToken: true
```

Enable it with `- proto: argocd-mcp-agent` in the target environment's
`env-data.ytt.yaml`. Start **read-only** everywhere; flip `allowSync: true` per
environment once the read path is proven.

## 2. Extract the credentials into your secret backend

```bash
kubectl --context <ctx> -n argocd-mcp get secret argocd-mcp-token \
  -o jsonpath='{.data.token}'   | base64 -d     # → secret "<env>-argocd-mcp-token"
kubectl --context <ctx> -n argocd-mcp get secret argocd-mcp-token \
  -o jsonpath='{.data.ca\.crt}' | base64 -d     # → secret "<env>-argocd-mcp-ca"
```

Two **separate** entries per cluster (the chart maps them to `eso.tokenRef` and
`eso.caRef`). The `server:` URL is that cluster's API server endpoint.

## 3. Add the `argocd-mcp` prototype

```
prototypes/argocd-mcp/
├── app-data.ytt.yaml        # schema: image, rbacTier, ingress host, OIDC, remoteInstances
├── vendir/                  # pulls oci://ghcr.io/truepace-io-oss/charts/argocd-mcp
└── helm/argocd-mcp.yaml     # maps env-data → chart values
```

The `helm/argocd-mcp.yaml` file maps your environment's data values onto the
chart's `localInstance` / `remoteInstances` / `auth` / `ingress` values.

## 4. Authentik OIDC client

Register a **public** OAuth2 client (PKCE, localhost redirect URIs) for the AI
agent in your identity provider. With Authentik this can be done declaratively
via a blueprint; the issuer is then
`https://auth.<your-domain>/application/o/argocd-mcp/` and the chart's
`auth.oidc.audience` is the client id. Gate access with a group and set it as
`auth.oidc.requiredGroups`. See [`auth.md`](./auth.md).

## 5. Ingress

`ingress.enabled: true`, `className: nginx`, host
`argocd-mcp.<cluster.domain>`. The public ingress is safe **because** access is
gated by OIDC — keep the chart's streaming annotations.

## 6. Pin the image and chart versions

`image.tag` from the `build-and-push` CI job
(`ghcr.io/truepace-io-oss/argocd-mcp-server`) and the chart version from
`publish-chart`. Add `#! renovate:` annotations in the prototype so Renovate can
bump both.

## 7. Enable in the environment

Add `- proto: argocd-mcp` to `envs/<env>/env-data.ytt.yaml`, put the per-env
overrides in `envs/<env>/_apps/argocd-mcp/app-data.ytt.yaml`, then render
**everything** (not just the changed app — `myks all` also syncs vendir, which
`myks render` alone does not):

```bash
myks all ALL
```

Commit `rendered/`; Argo CD syncs from there.

## 8. Verify

1. `instances_list` — every instance reachable, with its app count.
2. `apps_list` per instance — matches the Argo CD UI.
3. `app_refresh` on a harmless app — the annotation appears and clears.
4. `app_sync` with `dryRun: true`, then a real sync plus `app_wait`.
5. Grafana folder "MCP Servers" — `amcp_instance_up == 1` for every instance.
