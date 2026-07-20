CREATE TABLE self_iteration_candidate (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    project_id UUID NOT NULL,
    snapshot_hash TEXT NOT NULL,
    policy_version INTEGER NOT NULL DEFAULT 1,
    state TEXT NOT NULL DEFAULT 'proposed' CHECK (state IN ('proposed', 'accepted', 'rejected', 'superseded')),
    title TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE orchestration_audit_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    project_id UUID,
    issue_id UUID,
    event_type TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO orchestration_audit_event (workspace_id, project_id, issue_id, event_type, reason_code, payload, created_at)
SELECT i.workspace_id, i.project_id, t.issue_id, 'execution.superseded', 'single_active_migration',
       jsonb_build_object('task_id', t.id, 'agent_id', t.agent_id), COALESCE(t.completed_at, now())
FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
WHERE t.failure_reason = 'orchestration_superseded';
