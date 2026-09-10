# RBAC

Authorization is **entirely Kubernetes RBAC**. The server has no policy engine;
it only adds a read-only kill-switch on top (see below).

## The important caveat

Because `argocd-mcp` writes the `Application` custom resource directly, Argo CD's
own RBAC (`argocd-rbac-cm`, projects, SSO groups) is **not consulted**. The
Kubernetes Role bound to the server's ServiceAccount is the entire blast radius.

Worse: RBAC is **resource-scoped, not field-scoped**. `patch` on `applications`
grants write access to the *whole object*, and three separate fields can
redirect a sync at an attacker-controlled source:

| Field | Why it is dangerous |
|---|---|
| `spec.source` / `spec.sources` | the application's own source definition |
| `operation.sync.source` / `.sources` | Argo CD: *"Source overrides the source definition set in the application"* — no `spec` change needed |
| `status.history[].source` | `app_rollback` reads history to build the sync, so poisoned history becomes a real deployment |

The application-controller then applies those manifests with **its** privileges,
which are typically cluster-admin. All three paths were confirmed exploitable
against a real Argo CD; a guard on `spec` alone is **not** sufficient.

This is why the sync tier ships with the **write guard**.

That is why:

* the default tier is **read-only**;
* the sync tier is **namespaced** to the Argo CD namespace and grants only
  `patch` on `applications`;
* `create`, `update` and `delete` are never granted;
* remote instances default to `readOnly: true` in the chart's example values.

## Tiers

| Tier | Rules |
|------|-------|
| `read-only` (default) | `argoproj.io` `applications`, `appprojects`, `applicationsets` — `get, list, watch` |
| `sync` | the above **plus** `argoproj.io` `applications` — `patch` |
| `none` | nothing rendered; bring your own Role |

`patch` alone is sufficient for every mutation: all four write tools issue a JSON
merge patch, and the `Application` CRD has no status subresource, so
`app_terminate_operation` needs no `applications/status` rule either.

An optional rule grants `get` on the single Deployment `argocd-server`, used only
to show the Argo CD version in `instances_list`. Disable it with
`localInstance.rbac.versionProbe=false` for a Role that touches nothing but
`argoproj.io`.

## Local instance (the cluster the server runs in)

The chart renders one `Role` + `RoleBinding` per namespace in
`localInstance.namespace` ∪ `localInstance.applicationNamespaces`:

```bash
helm upgrade --install argocd-mcp ./deploy/helm/argocd-mcp \
  --set localInstance.namespace=argocd \
  --set localInstance.rbac.tier=sync
```

## Remote instances

The MCP is outside those clusters, so it needs a credential, not just a Role.
Apply `deploy/rbac/<tier>/` in the target cluster, then extract the token and CA:

```bash
kubectl --context <ctx> apply -f deploy/rbac/read-only/     # or sync/
./deploy/rbac/extract-credentials.sh <ctx>
```

Store `token` and `ca.crt` as **two separate** entries in your secret backend
(Vault, Bitwarden Secrets Manager, …) and reference them from the chart:

```yaml
remoteInstances:
  - name: cluster-b
    server: https://api.cluster-a.example.com:6443
    namespace: argocd
    readOnly: true
    eso:
      tokenRef: <backend-uuid-for-token>
      caRef: <backend-uuid-for-ca.crt>
```

The static ServiceAccount token does not expire and can only be revoked by
deleting the Secret — the same trade-off the sibling `kubernetes-mcp` already
makes for remote registration. Because the token is read from a file on every
request, rotating it needs no restart.

## The write guard (required for the sync tier)

A `ValidatingAdmissionPolicy` — a built-in Kubernetes resource, no controller or
webhook to run — closes all three paths. Admission, unlike authorization, sees
the old object, the new object and the requesting user, which is exactly what a
field-level rule needs.

It is rendered **only together with write access** (`localInstance.rbac.tier=sync`
in the chart, `allowSync: true` in the `argocd-mcp-agent` prototype), so the
dangerous verb can never ship unaccompanied. The CEL lives in
`internal/argocd/guard.go` and is mirrored into both.

```yaml
matchConditions:                       # scoped to this ServiceAccount only, so a
  - name: only-argocd-mcp              # broken policy cannot wedge the controller
    expression: 'request.userInfo.username == "system:serviceaccount:argocd-mcp:argocd-mcp"'
validations:
  - expression: 'object.spec == oldObject.spec'
  - expression: '!has(object.operation) || !has(object.operation.sync) || (!has(object.operation.sync.source) && !has(object.operation.sync.sources))'
  - expression: '<status.history unchanged>'
```

The nested `has()` guards are load-bearing: `has()` errors on a missing
intermediate field, and with `failurePolicy: Fail` an error **denies** the
request — without them, a legitimate operation carrying no sync block would be
rejected.

### Prerequisite: confirm the cluster enforces admission policies

A `ValidatingAdmissionPolicy` object can exist and be silently ignored if the API
server's admission plugin of the same name is disabled — which looks exactly like
protection while providing none. Managed control planes do not always document
their plugin set, so verify it functionally:

```bash
./deploy/rbac/check-admission-plugin.sh admin@tools admin@cluster-b
```

The script installs a probe policy scoped to a single ConfigMap name
(`failurePolicy: Ignore`, so it can never affect anything else), tests it with a
**server-side dry run** (nothing is persisted), removes itself on exit including
on Ctrl-C, and exits 0 (enforcing) / 1 (not enforcing) / 2 (could not check).

If a cluster reports **NOT ENFORCING**, do not enable `allowSync` or
`rbacTier: sync` there — the guard would provide no protection.

### Rollout

1. Enable `allowSync` (and therefore the guard) with
   `validationActions: ["Audit"]`.
2. Run normal MCP traffic; confirm the audit log shows zero violations.
3. Switch to `validationActions: ["Deny"]`.
4. **Confirm `amcp_write_guard_active == 1`** for that instance before treating
   write access as safe.

Step 4 is not ceremony. Policy activation is **asynchronous**: the objects exist
before the API server enforces them. On a healthy cluster this is sub-second
(measured at ~1 s), but on a loaded or degraded API server it has been observed
to take **several minutes**. Never assume the guard is live because `kubectl
apply` returned — confirm it.

### Runtime verification

The server verifies its own containment. Every five minutes, for each instance,
it sends a deliberately forbidden `spec` patch with `dryRun=All` (the full
admission chain runs, nothing is persisted) and expects a rejection:

| Result | Meaning | `amcp_write_guard_active` |
|---|---|---|
| denied by the admission policy | guard in force | 1 |
| denied by RBAC (no `patch`) | writes impossible anyway | 1 |
| **accepted** | these credentials can rewrite a source | **0** — alert |
| probe impossible (no Application, API error) | unknown | metric absent |

An unknown state never reports as safe. `instances_list` shows
`writeGuard=active|MISSING|unknown` and prints an explicit warning when a
writable instance is unguarded. This catches every silent failure mode: the
policy deleted, the binding removed, the admission plugin disabled by a
control-plane upgrade, or `matchConditions` drifting after a rename.

### Complementary: `resourceNames`

RBAC *can* scope `patch` to named objects (it cannot for `list`/`watch`). Broad
read, narrow write bounds the blast radius without any admission machinery:

```yaml
- apiGroups: ["argoproj.io"]
  resources: ["applications"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["argoproj.io"]
  resources: ["applications"]
  resourceNames: ["web-api"]
  verbs: ["patch"]
```

## The read-only kill-switch (defense in depth)

Two independent flags block every mutating tool *before* the API call:

* `config.readOnly` — global, for the whole server;
* `instances[].readOnly` — per Argo CD instance.

Blocked calls increment `amcp_writes_blocked_total{mcp_instance,reason}` and are
classified as `blocked` in `amcp_tool_calls_total`. They are not a substitute for
RBAC — they exist so an over-privileged token cannot be used to write through an
instance an operator intends to be read-only.

## Verifying

```bash
kubectl auth can-i --as=system:serviceaccount:argocd-mcp:argocd-mcp \
  -n argocd list applications.argoproj.io      # expect: yes
kubectl auth can-i --as=system:serviceaccount:argocd-mcp:argocd-mcp \
  -n argocd patch applications.argoproj.io     # yes only on the sync tier
kubectl auth can-i --as=system:serviceaccount:argocd-mcp:argocd-mcp \
  -n argocd delete applications.argoproj.io    # expect: no, always
```

The E2E suite asserts exactly this: a read-only ServiceAccount can `apps_list`
but receives a Kubernetes `Forbidden` on `app_sync`, and no operation is written.
