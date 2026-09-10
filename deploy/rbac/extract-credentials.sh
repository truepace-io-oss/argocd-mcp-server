#!/usr/bin/env bash
# Prints the ServiceAccount token and CA certificate an argocd-mcp instance needs
# to manage THIS cluster's Argo CD. Store them as two separate entries in your
# secret backend (Bitwarden, Vault, …) and reference them from the chart's
# remoteInstances[].eso.{tokenRef,caRef}.
#
# Usage: ./extract-credentials.sh [kube-context] [namespace] [secret-name]
set -euo pipefail

CONTEXT="${1:-}"
NAMESPACE="${2:-argocd-mcp}"
SECRET="${3:-argocd-mcp-token}"
KCTL=(kubectl)
[[ -n "$CONTEXT" ]] && KCTL+=(--context "$CONTEXT")

echo "# server (use as remoteInstances[].server)"
"${KCTL[@]}" config view --minify -o jsonpath='{.clusters[0].cluster.server}'
echo; echo

echo "# token  → store as its own secret, reference via eso.tokenRef"
"${KCTL[@]}" -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.data.token}' | base64 -d
echo; echo

echo "# ca.crt → store as its own secret, reference via eso.caRef"
"${KCTL[@]}" -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.data.ca\.crt}' | base64 -d
