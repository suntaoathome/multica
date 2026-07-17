#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: deploy-handoff.sh staging|production <backend-digest> <frontend-digest> [--confirm-production]" >&2
  exit 2
}

environment="${1:-}"
backend_digest="${2:-}"
frontend_digest="${3:-}"
confirmation="${4:-}"
helm_timeout="${HANDOFF_HELM_TIMEOUT:-15m}"
[[ "$backend_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || usage
[[ "$frontend_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || usage
[[ "$helm_timeout" =~ ^[1-9][0-9]*(s|m)$ ]] || {
  echo "HANDOFF_HELM_TIMEOUT must be a positive duration in seconds or minutes (for example 90s or 15m)" >&2
  exit 2
}
: "${HANDOFF_EXPECTED_KUBE_CONTEXT:?set HANDOFF_EXPECTED_KUBE_CONTEXT to the approved cluster context}"
: "${HANDOFF_EXPECTED_KUBE_SERVER:?set HANDOFF_EXPECTED_KUBE_SERVER to the approved Kubernetes API URL}"

case "$environment" in
  staging)
    namespace=handoff-staging
    release=handoff-staging
    values=deploy/jke/staging-values.yaml
    base_url=https://handoff-staging.oxygent.org.cn
    profile=staging
    ;;
  production)
    [[ "$confirmation" == "--confirm-production" ]] || {
      echo "production requires --confirm-production" >&2
      exit 2
    }
    namespace=multica
    release=multica
    values=deploy/jke/multica-values.yaml
    base_url=https://handoff.oxygent.org.cn
    profile=default
    ;;
  *) usage ;;
esac

current_context="$(kubectl config current-context)"
current_server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
[[ "$current_context" == "$HANDOFF_EXPECTED_KUBE_CONTEXT" ]] || {
  echo "kubectl context '$current_context' does not match approved context '$HANDOFF_EXPECTED_KUBE_CONTEXT'" >&2
  exit 1
}
[[ "$current_server" == "$HANDOFF_EXPECTED_KUBE_SERVER" ]] || {
  echo "Kubernetes API '$current_server' does not match the approved server" >&2
  exit 1
}

current_namespace="$(kubectl config view --minify -o jsonpath='{..namespace}')"
[[ -z "$current_namespace" || "$current_namespace" == "$namespace" ]] || {
  echo "kubectl context namespace '$current_namespace' does not match '$namespace'" >&2
  exit 1
}

previous_revision="$(helm history "$release" -n "$namespace" -o json 2>/dev/null | jq -r 'map(select(.status == "deployed")) | last | .revision // empty')"
rollback() {
  rc=$?
  if [[ $rc -ne 0 && -n "$previous_revision" ]]; then
    echo "deployment failed; rolling $release back to revision $previous_revision" >&2
    helm rollback "$release" "$previous_revision" -n "$namespace" --wait --timeout "$helm_timeout"
  fi
  exit "$rc"
}
trap rollback EXIT

helm upgrade --install "$release" deploy/helm/multica \
  --namespace "$namespace" --create-namespace -f "$values" \
  --set-string "images.backend.digest=$backend_digest" \
  --set-string "images.frontend.digest=$frontend_digest" \
  --atomic --wait --timeout "$helm_timeout"
kubectl -n "$namespace" rollout status "deployment/${release}-backend" --timeout=5m
kubectl -n "$namespace" rollout status "deployment/${release}-frontend" --timeout=5m
deploy/jke/scripts/verify-handoff-environment.sh "$base_url" "$profile"
trap - EXIT
printf 'deployed %s backend=%s frontend=%s\n' "$environment" "$backend_digest" "$frontend_digest"
