# AI-104 orchestration failure matrix

The current model stores issue lifecycle and task execution separately, but no
single transaction owns the transition between them. The following deterministic
interleavings define the Stage 1 regression surface.

| Scenario | Current split boundary | Required invariant |
| --- | --- | --- |
| `in_progress` with no active task | Issue status update and task enqueue are separate writes; failure handling can leave no successor | A non-terminal executable issue has one active execution or an explicit blocked/not-ready reason |
| daemon retry races patrol reassign | `FailTask` creates a retry in its transaction, while reassignment/cancellation uses independent issue/task writes | One issue execution generation admits one controller; stale generations cannot enqueue |
| member comment during direct run | comment admission dedup covers `queued/dispatched`, while active execution also includes `running` | Plain comments join/defer to the current generation; they do not create a second controller |
| two controllers cancel independently | cancellation targets task rows, not an issue execution generation | Cancelling a stale controller cannot strand or cancel the winning generation |
| review starts before implementation settles | review SHA dedup and issue status do not establish a barrier over active implementation tasks | Review admission requires the implementation generation to be terminal and its reviewed head fixed |
| next Autopilot tick overlaps prior run | trigger/run idempotency is local to a trigger key; unfinished prior work is not the project readiness gate | At most one open self-iteration generation per project/policy |

The focused failing test in `handler/orchestration_gap_test.go` proves the third
row through the real comment trigger path. It requires no timing assertions: a
seeded `running` task followed by a plain member comment deterministically leaves
two active rows today.

The common repair boundary should be a database-backed issue execution
generation (or equivalent lease) acquired transactionally by every producer:
assignment, comments, retry, patrol, review, and Autopilot. Status remains a
workflow label; readiness must be derived from generation/task state plus a
typed not-ready reason, rather than inferred from `issue.status` alone.
