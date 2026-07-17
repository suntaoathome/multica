# Handoff staging and safe self-upgrade runbook

## Environment boundary

Production stays the control plane. Staging is the target under test.

| Environment | URL | Namespace | Helm release | CLI profile |
| --- | --- | --- | --- | --- |
| Production | `https://handoff.oxygent.org.cn` | `multica` | `multica` | `default` |
| Staging | `https://handoff-staging.oxygent.org.cn` | `handoff-staging` | `handoff-staging` | `staging` |

Staging owns separate PostgreSQL and uploads PVCs, JWT and database secrets,
ALB/EIP, DNS record, workspace, PAT, daemon state, workspaces root, and health
port. It may share the cluster, image registry, SMTP relay, and wildcard TLS
certificate. Never copy production application data into staging.

## Normal staging deployment

Use immutable source-SHA tags. Do not promote a mutable `latest` tag.

The preferred path is the `Handoff staging` GitHub workflow. Dispatch it with
a full commit SHA. It runs the repository gates, builds SHA-tagged images,
captures the registry digests, deploys those digests atomically, and runs both
agent canaries. Its `handoff-qualified-<sha>` artifact is the promotion record.

```bash
helm lint deploy/helm/multica -f deploy/jke/staging-values.yaml

helm upgrade --install handoff-staging deploy/helm/multica \
  --namespace handoff-staging \
  -f deploy/jke/staging-values.yaml \
  --set images.backend.tag="$BACKEND_TAG" \
  --set images.frontend.tag="$FRONTEND_TAG" \
  --atomic --wait --timeout 15m

kubectl apply -f deploy/jke/staging-guardrails.yaml
kubectl apply -f deploy/jke/staging-proxy.yaml
kubectl -n handoff-staging rollout status deploy/handoff-staging-proxy --timeout=3m

deploy/jke/scripts/verify-handoff-environment.sh \
  https://handoff-staging.oxygent.org.cn staging
```

Create one synthetic canary Issue for Codex and one for Claude. Both must reach
`done`, post a task-scoped agent comment, and record non-zero usage before a
production promotion. A healthy HTTP endpoint alone is not a promotion gate.

## Required promotion gates

1. PR checks pass: Go tests, TypeScript tests/typecheck, Helm lint, and relevant
   end-to-end tests.
2. Images use source-SHA tags and their registry digests are recorded on the
   release Issue.
3. The staging Helm upgrade completes with `--atomic --wait`.
4. HTTP smoke, TLS, WebSocket daemon wakeup, Codex canary, and Claude canary pass.
5. The independent Reviewer posts approval on the release Issue.
6. Capture production Helm state and a database dump before changing production.

## Production pre-change backup

This path is deliberately independent of the Handoff API.

```bash
ts="$(date -u +%Y%m%dT%H%M%SZ)"
backup_dir="/root/handoff_backups/$ts"
install -d -m 0700 "$backup_dir"

helm history multica -n multica -o json > "$backup_dir/helm-history.json"
helm get values multica -n multica --all > "$backup_dir/helm-values.yaml"
helm get manifest multica -n multica > "$backup_dir/helm-manifest.yaml"

kubectl -n multica exec deploy/multica-postgres -- sh -c \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
  > "$backup_dir/postgres.dump"
chmod 0600 "$backup_dir/postgres.dump"
```

## Production promotion

Use the `Handoff production promotion` workflow. GitHub's
`handoff-production` environment must have required reviewers. Supply the same
commit and two digests from the qualified staging artifact, then type
`PROMOTE_HANDOFF_PRODUCTION`. There is no push/tag trigger for production.

```bash
previous_revision="$(helm history multica -n multica -o json | jq -r 'map(select(.status == "deployed")) | last | .revision')"

helm upgrade multica deploy/helm/multica \
  --namespace multica \
  -f deploy/jke/multica-values.yaml \
  --set images.backend.tag="$BACKEND_TAG" \
  --set images.frontend.tag="$FRONTEND_TAG" \
  --atomic --wait --timeout 15m

deploy/jke/scripts/verify-handoff-environment.sh \
  https://handoff.oxygent.org.cn default
```

If any post-deploy gate fails, do not ask the broken Handoff instance to repair
itself. Roll back out-of-band:

```bash
helm rollback multica "$previous_revision" -n multica --wait --timeout 15m
deploy/jke/scripts/verify-handoff-environment.sh \
  https://handoff.oxygent.org.cn default
```

## Recovery when Handoff is unavailable

These controls do not depend on the Handoff web/API process:

```bash
systemctl status multica-daemon.service
systemctl status multica-staging-daemon.service
kubectl -n multica get pods,deploy,svc,pvc
kubectl -n multica logs deploy/multica-backend --previous --tail=200
helm history multica -n multica
```

Re-run staging without the web UI with `gh workflow run handoff-staging.yml -f
ref=<full-sha>`. Inspect with `gh run list --workflow handoff-staging.yml` and
`gh run watch <run-id>`. Production rollback is deliberately out-of-band:
first inspect `helm history multica -n multica`, then run `helm rollback multica
<known-good-revision> -n multica --wait --timeout 15m`. The deploy script does
this automatically on a failed upgrade or post-deploy smoke check.

The source of truth is GitHub plus immutable registry tags. Kubernetes Secrets
stay out of Git and Issues. If a staging secret is lost, rotate and recreate it;
never copy the production JWT or database password into staging.
