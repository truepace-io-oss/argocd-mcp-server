#!/usr/bin/env bash
# Creates a kind cluster with a real Argo CD installation for the e2e_kind suite.
# Usage: ./setup-kind.sh [cluster-name] [argocd-version]
set -euo pipefail

CLUSTER="${1:-amcp-e2e}"
ARGOCD_VERSION="${2:-v3.5.2}"

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
fi

kubectl --context "kind-$CLUSTER" create namespace argocd --dry-run=client -o yaml | kubectl --context "kind-$CLUSTER" apply -f -
# --server-side is required: Argo CD's ApplicationSet CRD exceeds the 262144-byte
# limit for the client-side last-applied-configuration annotation.
kubectl --context "kind-$CLUSTER" -n argocd apply --server-side --force-conflicts -f \
  "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"

echo "waiting for Argo CD to become ready…"
kubectl --context "kind-$CLUSTER" -n argocd rollout status deploy/argocd-repo-server --timeout=300s
kubectl --context "kind-$CLUSTER" -n argocd rollout status deploy/argocd-server --timeout=300s
kubectl --context "kind-$CLUSTER" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
echo "Argo CD ${ARGOCD_VERSION} is ready in kind cluster ${CLUSTER}"
