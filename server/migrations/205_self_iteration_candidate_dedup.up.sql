CREATE UNIQUE INDEX CONCURRENTLY idx_self_iteration_candidate_live_snapshot ON self_iteration_candidate(project_id, snapshot_hash, policy_version) WHERE state IN ('proposed', 'accepted');
