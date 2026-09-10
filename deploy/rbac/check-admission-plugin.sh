#!/usr/bin/env bash
#
# check-admission-plugin.sh — does this cluster ENFORCE ValidatingAdmissionPolicy?
#
# The argocd-mcp write guard (see ../../docs/rbac.md) is a
# ValidatingAdmissionPolicy. Those objects can exist and be silently ignored if
# the API server's admission plugin of the same name is not enabled — which
# would look exactly like protection while providing none. Managed control
# planes (Scaleway Kapsule, EKS, …) do not always document their plugin set, so
# this verifies it functionally.
#
# Safety: the probe policy uses failurePolicy=Ignore and a matchCondition that
# pins it to ONE ConfigMap name, so no other object in the cluster is ever
# evaluated. The test itself is a server-side dry run, so nothing is persisted.
# The policy is removed again on exit, including on Ctrl-C.
#
# Usage:
#   ./check-admission-plugin.sh                     # current context
#   ./check-admission-plugin.sh ctx-a ctx-b ctx-c   # several clusters
#
# Env:
#   PROBE_NAMESPACE   namespace used for the dry-run create (default: default)
#   TIMEOUT_SECONDS   how long to wait for the policy to activate (default: 120)
#
# Exit codes:
#   0  every checked cluster ENFORCES admission policies
#   1  at least one cluster does NOT
#   2  a check could not be completed (no permission, API absent, …)
set -uo pipefail

PROBE_NAME="argocd-mcp-admission-probe"
PROBE_NAMESPACE="${PROBE_NAMESPACE:-default}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-120}"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
warn()  { printf '\033[33m%s\033[0m\n' "$*"; }

CURRENT_CTX=""
cleanup() {
  [[ -n "$CURRENT_CTX" ]] || return 0
  kubectl --context "$CURRENT_CTX" delete validatingadmissionpolicybinding "$PROBE_NAME" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl --context "$CURRENT_CTX" delete validatingadmissionpolicy "$PROBE_NAME" \
    --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT INT TERM

check_context() {
  local ctx="$1"
  CURRENT_CTX="$ctx"
  echo "── $ctx"

  # 1. Build the probe policy. It can only ever match one ConfigMap name, so no
  #    other object in the cluster is evaluated by it.
  local manifest
  manifest=$(cat <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: ${PROBE_NAME}
spec:
  # Ignore: if this policy ever errors, requests pass. Safe in production.
  failurePolicy: Ignore
  matchConstraints:
    resourceRules:
      - apiGroups:   [""]
        apiVersions: ["v1"]
        operations:  ["CREATE"]
        resources:   ["configmaps"]
  matchConditions:
    # Nothing but a ConfigMap with this exact name is ever evaluated.
    - name: only-the-probe
      expression: 'has(object.metadata.name) && object.metadata.name == "${PROBE_NAME}"'
  validations:
    - expression: 'false'
      message: "ADMISSION-POLICY-ENFORCING"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: ${PROBE_NAME}
spec:
  policyName: ${PROBE_NAME}
  validationActions: [Deny]
EOF
)

  # Applying it doubles as the availability check: if the API server does not
  # serve ValidatingAdmissionPolicy, this fails with "no matches for kind".
  cleanup
  local apply_out
  if ! apply_out="$(printf '%s\n' "$manifest" | kubectl --context "$ctx" apply -f - 2>&1)"; then
    if grep -qiE 'no matches for kind|could not find the requested resource|server doesn.t have a resource type' <<<"$apply_out"; then
      red "   NOT AVAILABLE — the API server does not serve ValidatingAdmissionPolicy"
    elif grep -qi 'forbidden' <<<"$apply_out"; then
      red "   CANNOT CHECK — no permission to create admission policies (cluster-admin required)"
    else
      red "   CANNOT CHECK — ${apply_out%%$'\n'*}"
    fi
    return 2
  fi

  # 2. Poll. Activation is asynchronous: the objects exist before the API server
  #    enforces them (sub-second on a healthy cluster, minutes on a loaded one).
  local deadline=$(( SECONDS + TIMEOUT_SECONDS )) out
  while (( SECONDS < deadline )); do
    out="$(kubectl --context "$ctx" -n "$PROBE_NAMESPACE" create configmap "$PROBE_NAME" \
            --dry-run=server 2>&1)"
    if grep -q 'ADMISSION-POLICY-ENFORCING' <<<"$out"; then
      green "   ENFORCING — the write guard will work on this cluster"
      cleanup
      return 0
    fi
    # A dry-run create that succeeds means the policy was not applied (yet).
    if ! grep -qE 'created \(server dry run\)|^configmap/' <<<"$out"; then
      # Something other than "allowed" or "denied by us" — surface it.
      if grep -qiE 'forbidden|error' <<<"$out"; then
        red "   CANNOT CHECK — ${out%%$'\n'*}"
        cleanup
        return 2
      fi
    fi
    sleep 2
  done

  red  "   NOT ENFORCING — policy objects exist but were ignored after ${TIMEOUT_SECONDS}s"
  warn "   The argocd-mcp write guard would provide NO protection on this cluster."
  warn "   Do not enable allowSync / rbacTier=sync here; see docs/rbac.md."
  cleanup
  return 1
}

main() {
  local -a contexts=("$@")
  if (( ${#contexts[@]} == 0 )); then
    contexts=("$(kubectl config current-context)")
  fi

  local rc=0 result
  for ctx in "${contexts[@]}"; do
    check_context "$ctx"
    result=$?
    (( result > rc )) && rc=$result
    CURRENT_CTX=""
  done

  echo
  case $rc in
    0) green "All checked clusters enforce admission policies." ;;
    1) red   "At least one cluster does NOT enforce admission policies." ;;
    *) warn  "At least one cluster could not be checked." ;;
  esac
  return $rc
}

main "$@"
