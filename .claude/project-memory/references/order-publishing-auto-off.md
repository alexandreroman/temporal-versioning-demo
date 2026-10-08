---
name: "Order publishing is one Temporal activity bounded by a timeout"
description: "OrderPublishing runs PublishOrders (window = ScheduleToClose), then a 10 min closing phase that terminates stuck orders; pause signals, resume resets"
type: project
---

# Order publishing is one Temporal activity bounded by a timeout

The backend publishes pizza orders through the `OrderPublishing` workflow
(fixed workflow ID `order-publishing`), run by an **unversioned** worker hosted
in the backend on its own `order-publishing` task queue. The workflow runs a
single long-running `PublishOrders` activity that starts one order per tick,
then a closing phase. Publishing is open while the run is Running **and** its
memo has no `publishingStopped` mark.

- **The window is the activity's `ScheduleToCloseTimeout`**
  (`PIZZA_PUBLISHING_TIMEOUT`, default `15m`): Temporal ends publishing, the
  SDK turns it into the activity context deadline (local, on-time stop), and
  it spans all retries, so a retry after a crash only gets the remaining time.
  A window whose last retry cannot fit before the deadline ends with retry
  state TIMEOUT; both count as a normal end, never as Failed.
- **"Pause orders" is the `pause` signal.** The workflow cancels
  `PublishOrders` and, in the same workflow task, upserts the memo mark
  `publishingStopped` (`timeout` or `paused`). Before each order the activity
  describes its own run **by run ID** and stops when the run is not Running or
  carries the mark (Describe is not a billable action). The run ID matters:
  after a Resume, a new run holds the same workflow ID.
- **Closing phase:** once publishing stops (window or pause), the run waits a
  **durable workflow timer** `closeDelay` (10 min, a constant), then the
  `CloseOrders` activity terminates every `PizzaOrder` still Running that
  started before the timer fired — by then only stuck ones (v3 drone retries,
  orders pinned to a version without workers). Healthy orders finish well
  within the delay. A pause signal during the closing phase changes nothing.
- **"Resume orders":** `TERMINATE_EXISTING`, a full new window. During the
  closing phase this terminates the waiting run and its timer, so no order is
  closed. **Startup:** `USE_EXISTING` keeps a running run across backend
  restarts; the cached state then comes from a Describe, so a kept run in its
  closing phase reads paused.
- Timers are fine in `OrderPublishing`: the no-timer rule of
  [[workflow-waits-activity-side]] covers the customer-facing pizza workflows
  only.
- **Heartbeat (1 min timeout) only detects a crashed backend.** A graceful
  worker stop (Air reload, SIGTERM) fails the attempt through the worker stop
  channel (`WorkerStopTimeout` 5 s, Air backend `kill_delay` 2 s), so the
  next worker retries it within seconds. Every restart is a retry of the same
  activity, so its retry policy is capped at 5 s; the server default backoff
  (up to 100 s) would widen the gap with each restart.
- **Safety net:** each run has a `WorkflowExecutionTimeout` of window +
  `closeDelay` + 1 min (`CloseOrders` schedule-to-close) + 1 min margin, so
  the server closes a run stuck on a non-determinism error (a deploy that
  changes this workflow's code while `USE_EXISTING` keeps an old run) or one
  whose window expired while the backend was down.
- The toggle state is cached from Describe (Running and no stop mark),
  refreshed every poll interval. Pause remembers the run ID it signalled and
  keeps reading paused for that run until its mark shows up, so the toggle
  does not flicker back to open. The state is read with Describe, not a
  query: a query is a billable action and needs a worker.
- "Publishing" matches the UI vocabulary ("Order publishing is paused",
  `publishing-toggle`). The README mentions only `PIZZA_PUBLISHING_TIMEOUT`,
  its role and default, and the 10 min closing of stuck orders — not the
  workflow.

**Why:** the auto-off and the closing phase keep an unattended demo from
burning Temporal actions and from showing stuck orders forever; the window and
the delay must survive backend restarts and Temporal must own them (same
motive as the v3 drone window in [[demo-timing]]).

**How to apply:** keep the window on the activity's schedule-to-close, the
closing delay on a workflow timer, the run-ID and memo checks before each
order, and the publishing worker free of deployment options so it stays out of
the pizza rollout.
