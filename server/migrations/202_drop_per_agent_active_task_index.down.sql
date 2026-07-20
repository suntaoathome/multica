CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_one_pending_task_per_issue_agent ON agent_task_queue(issue_id, agent_id) WHERE issue_id IS NOT NULL AND status IN ('queued', 'dispatched');
