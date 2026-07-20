WITH ranked AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY issue_id
               ORDER BY CASE status
                   WHEN 'running' THEN 0
                   WHEN 'waiting_local_directory' THEN 1
                   WHEN 'dispatched' THEN 2
                   WHEN 'queued' THEN 3
                   ELSE 4
               END, created_at, id
           ) AS active_rank
    FROM agent_task_queue
    WHERE issue_id IS NOT NULL
      AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
      AND trigger_comment_id IS NULL
      AND is_leader_task = FALSE
      AND originator_source = 'issue_assignment'
)
UPDATE agent_task_queue AS task
SET status = 'cancelled',
    completed_at = now(),
    failure_reason = 'orchestration_superseded',
    error = 'Superseded while enabling the single-active-task-per-issue fence',
    prepare_lease_expires_at = NULL
FROM ranked
WHERE task.id = ranked.id AND ranked.active_rank > 1;

DROP INDEX IF EXISTS idx_one_pending_task_per_issue_agent;
