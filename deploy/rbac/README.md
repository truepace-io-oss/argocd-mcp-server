# Remote-cluster RBAC

Apply one of these in a cluster whose Argo CD should be managed by an
`argocd-mcp` running **elsewhere**:

| Directory | Grants |
|-----------|--------|
| `read-only/` | `get, list, watch` on `applications`, `appprojects`, `applicationsets` in the `argocd` namespace |
| `sync/` | the above **plus** `patch` on `applications` — required for `app_sync`, `app_refresh`, `app_rollback`, `app_terminate_operation` |

```bash
kubectl --context <ctx> apply -f read-only/     # or sync/
./extract-credentials.sh <ctx>
```

Store the printed `token` and `ca.crt` as **two separate** entries in your
secret backend (Vault, Bitwarden Secrets Manager, …)
and wire them into the chart:

```yaml
remoteInstances:
  - name: cluster-b
    server: https://<printed server URL>
    namespace: argocd
    readOnly: true
    eso:
      tokenRef: <uuid-of-token-secret>
      caRef: <uuid-of-ca-secret>
```

Nothing else is required in the target cluster: no ingress, no VPN, no Argo CD
account or API token, and no change to `argocd-cm` / `argocd-rbac-cm`.

## Before enabling the `sync` tier

`patch` on `applications` is a privilege-escalation path unless the **write
guard** is in force (see `../../docs/rbac.md`). The guard is a
`ValidatingAdmissionPolicy`, which only protects anything if the cluster's
admission plugin is enabled. Check that first:

```bash
./check-admission-plugin.sh <kube-context> [more-contexts…]
#   exit 0 = enforcing        -> safe to enable the sync tier
#   exit 1 = NOT enforcing    -> do not enable it
#   exit 2 = could not check  -> no permission / API not served
```

Safe to run against production: the probe policy is scoped to one ConfigMap
name, uses `failurePolicy: Ignore`, tests with a server-side dry run, and
removes itself on exit.

Note that writing the `Application` CR bypasses Argo CD's own RBAC — the Role
here is the entire blast radius. See `../../docs/rbac.md`.
