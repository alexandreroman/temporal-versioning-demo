---
name: "Demo timing and the v3 drone regression"
description: "Step/Delivered dwell values and why Delivered is separate; order cadence and ramp steps; v3 drone always fails, retried for up to a 15 min window"
type: project
---

# Demo timing and the v3 drone regression

- Each work step takes ~15 s of **activity** time (`stepDwell`); the final
  `Deliver`/"Done" step has its own `deliveredDwell` = 7 s. Orders start every
  ~6 s; the UI ramp increments 25/50/100 % (smallest canary 25 %).
- These three durations are **unexported package `var`s in `activities.go`**
  (`stepDwell`, `deliveredDwell`, `droneAttempt`), hardcoded inside each activity
  function; the white-box test zeroes them via `init()` so the suite stays fast.
- **Why `DeliveredDwell` is separate:** the order is marked Done right before
  `Deliver` runs, and the dashboard lists only Running workflows, so the all-green
  card stays on the board during `Deliver`'s dwell. The frontend keeps it visible
  ~4 s then collapses it; `DeliveredDwell` (7 s) is sized to outlast that collapse
  so the node isn't removed mid-animation. See [[frontend-conventions]].
- **v3 regression:** the Drone delivery activity always fails, using Temporal's
  native retry with unlimited attempts (`MaximumAttempts: 0`; default 1 s
  initial interval ×2, capped at a 30 s `MaximumInterval`), bounded by
  duration: the drone call carries a `ScheduleToCloseTimeout` of
  `droneRetryWindow` (15 min, a package `var` in `workflow_v3.go`). An order
  stalls **red/Running until it is recovered**; one nobody recovers fails after
  the window and leaves the dashboard. There is no manual retry loop and no
  retry counter. Each failing attempt takes ~5 s (`droneAttempt`), so in steady
  state an attempt lands roughly every 35 s.
- **Why the backoff cap and the window:** each drone retry is a billable
  Temporal Cloud action. The 30 s cap keeps that count low (the dashboard shows
  no retry counter, so the cadence is invisible there) and the window stops it
  entirely. The window is a duration (not an attempt count) so it reads plainly
  and stays independent of the retry cadence; 15 min covers a live demo
  (stuck orders get recovered within minutes) and caps a stuck order at
  ~25-30 attempts.
- The Temporal Go test env silently caps `MaximumAttempts: 0` at 10 attempts, so
  the window test shrinks `droneRetryWindow` below that span to prove it bites.
- All dwell is activity-side, never workflow timers — see
  [[workflow-waits-activity-side]].

**Why:** these durations are tuned to the on-screen narrative and are coupled to
the UI collapse timing — retune them together.
