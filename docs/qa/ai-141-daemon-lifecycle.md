# AI-141 daemon lifecycle E2E

Change-ID: `AI-141-daemon-lifecycle-v1`

The harness `scripts/qa/ai-141-daemon-lifecycle.sh` creates a disposable
PostgreSQL container and launches separate server, daemon, and agent processes.
The test agent implements the Codex app-server JSON-RPC boundary without model
credentials. Ready work and lifecycle transitions are created through the
CLI/API. SQL in the harness is read-only and is used only to poll/assert
evidence. The script never targets Staging or production.

The lifecycle matrix proves:

- six simultaneous ready issues occupy all six real daemon slots;
- a running task is preempted through the cancel-task API;
- review and external-wait fences remain idle across an observed 30-second
  server ready-recovery patrol;
- reassign and comment operations race live assignment controllers without
  creating duplicates, and the exact comment ID is recorded in task-side
  `delivered_comment_ids`; and
- a real daemon `SIGKILL` followed by a new daemon process recovers unfinished
  work within 60 seconds.

Run from the repository root:

```sh
scripts/qa/ai-141-daemon-lifecycle.sh
```

Raw server, daemon, migration, API, and task evidence is written to
`artifacts/ai-141/`. The acceptance line is `result.log`; it records the six
running slots, recovery latency, active and duplicate controller counts,
task-side comment delivery, observed patrol, and declared-wait fence state.

For a fast rerun against binaries already built in the artifact directory, set
`AI141_SKIP_BUILD=1`. A normal acceptance run must omit this option so all
three executables are rebuilt from the candidate revision.

Rollback: remove the test-only command, harness, and this document. There are
no schema or production runtime changes.
