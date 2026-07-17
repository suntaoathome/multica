#!/usr/bin/env bash
set -euo pipefail

helm lint deploy/helm/multica -f deploy/jke/staging-values.yaml
helm template handoff-staging deploy/helm/multica -n handoff-staging \
  -f deploy/jke/staging-values.yaml \
  --set-string images.backend.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  --set-string images.frontend.digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb >/dev/null
helm lint deploy/helm/multica -f deploy/jke/multica-values.yaml
helm template multica deploy/helm/multica -n multica \
  -f deploy/jke/multica-values.yaml \
  --set-string images.backend.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  --set-string images.frontend.digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb >/dev/null
bash -n deploy/jke/scripts/*.sh

if command -v shellcheck >/dev/null; then
  shellcheck deploy/jke/scripts/*.sh
fi
