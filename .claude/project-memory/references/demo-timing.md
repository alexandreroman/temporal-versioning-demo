---
name: "Demo timing and the v3 drone regression"
description: "Step/Delivered dwell values and why Delivered is separate; order cadence and ramp steps; v3 drone always-fails with native unlimited retry"
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
  **native unlimited** retry (`MaximumAttempts: 0`, `MaximumInterval` capped to
  keep the cadence lively). The order stalls **red and stays Running forever — it
  never ends `Failed`**; there is no manual retry loop and no retry counter. Each
  failing attempt takes ~5 s (`droneAttempt`).
- All dwell is activity-side, never workflow timers — see
  [[workflow-waits-activity-side]].

**Why:** these durations are tuned to the on-screen narrative and are coupled to
the UI collapse timing — retune them together.
