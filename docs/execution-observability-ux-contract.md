# Execution observability UX contract

Status: implementation contract for Agents, Issues, Projects, and Autopilots.

## The question this UI answers

Every execution surface must answer two separate questions:

1. What is the entity's workflow state?
2. Is work actually queued or running, and if not, why?

An Issue in `in_progress`, an active Project, or an enabled Autopilot is not
proof that a task is executing. Workflow badges keep their existing meaning.
Execution state is a second, server-derived line and must never be inferred
from workflow state.

## Source of truth

Web and desktop use the workspace-scoped `agent-task-snapshot` React Query
cache for live task state. Agent availability is derived from that cache plus
the existing agent and runtime queries. WebSocket task events invalidate the
same query; the 30-second stale time and focus refetch are fallbacks.

Do not copy task data into Zustand, create per-page polling, or synthesize
execution from Issue, Project, or Autopilot status. Installed desktop clients
must parse additive response fields through the schemas in `packages/core` and
degrade unknown enum values to a neutral state.

## Canonical execution model

The compact summary is one of the following. Color is supplementary; every
state always has text and an icon.

| State | Meaning | Compact label | Detail requirement |
| --- | --- | --- | --- |
| Running | At least one task is executing | `Running N/M` | agent, issue, start time, runtime |
| Ready | A queued task is claimable and capacity is available | `Ready · queued N` | oldest queue age and next claimant |
| Waiting | Handoff owns the wait | `Waiting · {reason}` | reason, since, responsible component, safe action |
| External | Progress depends outside Handoff | `External · {reason}` | dependency/evidence, since, accountable person |
| Failed | Latest dispatch or run failed and remains unresolved | `Failed · {reason}` | failure time, attempt, evidence, retry eligibility |
| Idle | No active task and no unresolved failure | `Idle` | last terminal event when available |
| Unknown | Required observability data is unavailable | `Status unavailable` | failed data source and retry affordance |

`M` is the effective agent concurrency limit. A runtime may host several
agents; do not present runtime-wide daemon slots as an agent limit. If the
server cannot provide a trustworthy effective limit, show `Running N` rather
than inventing a denominator.

Known reason codes are rendered with localized copy; unknown codes show a
neutral `Waiting · other` or `Failed · other` label and preserve the server
message in detail. Minimum reason taxonomy:

- Waiting: `no_capacity`, `runtime_offline`, `runtime_unstable`,
  `local_directory_lock`, `serialized_by_issue_agent`, `scheduled`,
  `admission_pending`.
- External: `human_approval`, `external_service`, `repository_access`,
  `credentials`, `physical_resource`.
- Failed: `provider_auth`, `rate_limited`, `context_overflow`, `runtime_lost`,
  `cancel_failed`, `unknown`.

## Surface contract

### Agents

- Keep availability (`Online`, `Unstable`, `Offline`) separate from workload.
- The list summary shows `Running N/M`, `Ready`, `Waiting`, `Failed`, or `Idle`;
  queued work on an offline runtime is Waiting, never Working.
- Detail shows active slots first, then queued/waiting work, then the latest
  dispatch/failure/cancellation chain.
- `Cancel` is scoped to a task. `Cancel all` retains its confirmation and
  states running and queued impact separately.

### Issues

- A row/card displays Working only for a task whose `task.issue_id` equals that
  Issue's own ID.
- Parent Issues never inherit a child's Running or Queued indicator. The parent
  may show a separate non-live summary such as `1 child running`, but that text
  is not styled or announced as parent execution.
- Workflow status stays in the status column. Execution appears beside the
  identifier and opens a task detail popover/drawer.
- Queued, dispatched, and `waiting_local_directory` are active but not Running.

### Projects

- Project status and progress remain workflow summaries.
- A Project may aggregate direct Issue task counts (`2 running · 3 waiting`),
  but must not claim the Project itself is executing.
- Project detail groups the aggregate by Issue; selecting a count navigates or
  filters to those Issues using the existing Issue surface query boundary.

### Autopilots

- Enabled/paused describes trigger admission only. It is not execution state.
- Last run remains historical. A separate current-run summary is derived by
  `autopilot_run_id`, including issue-less tasks.
- `Run now` success means the request was admitted, not completed. Show the
  created run's Ready/Waiting/Running state and a link to its responsibility
  chain.
- Skipped runs must show the skip reason and must not use success styling.

## Responsibility chain and recovery actions

Detail presentation is chronological and names the actor or subsystem:

`triggered by` → `admitted by` → `dispatched to` → `started by` →
`cancel requested by` → terminal outcome.

Actions are shown only when the server explicitly marks them safe and the user
is authorized:

- Ready/Waiting: `Open runtime`, `Open blocking issue`, or `Cancel queued task`.
- External: `Open evidence` or `Copy blocker details`; no fake automatic fix.
- Failed: `Retry` only with a server-issued retry eligibility/token; otherwise
  `Open logs`.
- Running: `Cancel task` with confirmation. Cancellation is pending until the
  server reports a terminal cancellation event.

Disabled actions include an inline reason. Tooltips are supplementary, never
the only explanation.

## Responsive, empty, and accessible behavior

- Desktop uses an aligned summary column and a 320–480 px detail drawer or
  existing hover detail. Mobile collapses the same content into a full-width
  sheet; no data or action disappears solely because of viewport width.
- At 375 px, each action has a 44 px target and responsibility events stack
  vertically. Long reason text wraps; IDs and paths use break-anywhere.
- Loading uses shape-matched skeletons after 300 ms. Empty means `No active
  work`; query failure means `Execution status unavailable` with Retry. These
  states must not share copy.
- Live changes announce concise transitions through a polite live region
  (`Task started`, `Task waiting for runtime`). Cancellation and failures use
  an assertive announcement. Do not repeatedly announce elapsed-time ticks.
- Status controls and timeline entries are keyboard reachable with visible
  focus. Icons are decorative when adjacent text exists.
- `prefers-reduced-motion` removes shimmer/spin while retaining the state icon
  and label.

## Realtime acceptance scenarios

1. Queue → dispatch → running updates all four relevant surfaces from one
   snapshot invalidation without a page reload.
2. A child task starts while its parent is visible: only the child says
   Running; the parent may update its child summary.
3. Runtime goes offline with queued work: Agent and Issue change to Waiting,
   preserve queue age, and offer `Open runtime`.
4. An Autopilot creates an issue but has no current task: the run is historical
   `Issue created`, not Running.
5. Query failure after previously rendered live data does not silently become
   Idle; the surface shows stale/unknown state until a successful refresh.
6. A cancel request does not erase a task optimistically. The UI shows
   cancellation pending until the terminal event arrives.

## Backend field requirements

The current snapshot provides task IDs, agent/runtime/issue/autopilot links,
status, timestamps, failure reason, attempts, and attribution. The following
additive fields are required before the full contract can be implemented
without client inference:

| Field | Purpose |
| --- | --- |
| `execution_reason.code`, `message`, `since` | stable Ready/Waiting/External/Failed explanation |
| `execution_reason.owner_type`, `owner_id` | accountable person or subsystem |
| `execution_reason.evidence` | typed Issue/comment/runtime/log/external link |
| `effective_max_concurrent_tasks` | trustworthy `N/M` denominator |
| `claimable` and `claim_blockers[]` | distinguish Ready from Waiting |
| `cancel_requested_at`, `cancel_requested_by` | do not misreport cancellation as terminal |
| `recovery_actions[]` | authorized server-approved action, target, disabled reason, optional retry token |
| `dispatch_history[]` | latest dispatch/cancel responsibility chain without reconstructing logs |

These fields belong on the existing workspace task snapshot (or a batched
task-observability projection referenced by it), not four new page endpoints.
They are additive and optional for mixed-version desktop compatibility.

## Current implementation audit

- Agents already derive availability/workload and counts from the shared
  snapshot, but list copy folds Running and Queued into a generic task count.
- Issues already distinguish Running from Queued and bind activity directly to
  `task.issue_id`; regression tests cover hidden children and issue-less tasks.
- Projects expose workflow progress and can reuse the scoped Issue surface, but
  have no reason aggregation yet.
- Autopilots expose last-run outcome and next-run time; current execution and
  responsibility require the fields above and correlation by
  `autopilot_run_id`.

