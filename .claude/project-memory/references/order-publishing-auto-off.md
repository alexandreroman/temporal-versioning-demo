---
name: "Order publishing is one Temporal activity bounded by a timeout"
description: "OrderPublishing workflow runs a single PublishOrders activity whose ScheduleToClose is the publishing window; fixed ID, startup keeps, resume resets, pause terminates"
type: project
---

# Order publishing is one Temporal activity bounded by a timeout

The backend publishes pizza orders through the `OrderPublishing` workflow
(fixed workflow ID `order-publishing`), run by an **unversioned** worker hosted
in the backend on its own `order-publishing` task queue. The workflow runs a
single long-running `PublishOrders` activity that starts one order per tick.
Publishing is open while the workflow run is Running.

- **The window is the activity's `ScheduleToCloseTimeout`**
  (`PIZZA_PUBLISHING_TIMEOUT`, default `15m`): Temporal ends publishing, the
  SDK turns it into the activity context deadline (local, on-time stop), and
  it spans all retries, so a retry after a crash only gets the remaining time.
  The workflow treats that timeout as a normal end (Completed). The workflow
  has no timer.
- **Pause is immediate without heartbeats:** before each order the activity
  describes its own run **by run ID** and stops when it is no longer Running
  (Describe is not a billable action). The run ID matters: after a Resume, a
  new run holds the same workflow ID.
- **Heartbeat (1 min timeout) only detects a crashed backend.** A graceful
  worker stop (Air reload, SIGTERM) fails the attempt through the worker stop
  channel (`WorkerStopTimeout` 5 s, Air backend `kill_delay` 2 s), so the
  next worker retries it within seconds. Every restart is a retry of the same
  activity, so its retry policy is capped at 5 s; the server default backoff
  (up to 100 s) would widen the gap with each restart.
- A window whose last retry cannot fit before the deadline ends with retry
  state TIMEOUT; the workflow treats it as a normal end too, so a window never
  closes as Failed.
- **Startup:** conflict policy `USE_EXISTING` keeps a running window across
  backend restarts (no workflow-ID clash); a restart after expiry or pause
  opens a fresh window. **"Resume orders":** `TERMINATE_EXISTING`, a full new
  window. **"Pause orders":** `TerminateWorkflow`, closed at once server-side.
- **Safety net:** each run has a `WorkflowExecutionTimeout` of window + 1 min,
  so the server closes a run stuck on a non-determinism error (a deploy that
  changes this workflow's code while `USE_EXISTING` keeps an old run) or one
  whose window expired while the backend was down.
- The toggle state is cached from Describe, refreshed every poll interval.
  The state is read with Describe, not a query: a query is a billable action
  and needs a worker.
- "Publishing" matches the UI vocabulary ("Order publishing is paused",
  `publishing-toggle`). The README mentions only `PIZZA_PUBLISHING_TIMEOUT`,
  its role and default — not the workflow.

**Why:** the auto-off keeps an unattended demo from burning Temporal actions;
the window must survive backend restarts and Temporal must own it (same motive
as the v3 drone window in [[demo-timing]]).

**How to apply:** keep the window on the activity's schedule-to-close (not on
a workflow timer), keep the run-ID check before each order, and keep the
publishing worker free of deployment options so it stays out of the pizza
rollout.
