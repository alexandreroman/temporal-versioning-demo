---
name: "Pizza workflow waits are activity-side, never workflow.Sleep"
description: "All dwell/pacing is simulated as activity execution time; no workflow timers, so no Sleep clutters the Temporal Web UI"
type: feedback
---

# Pizza workflow waits are activity-side, never workflow.Sleep

In the pizza workflow, **all dwell/pacing is simulated as activity execution
time — never `workflow.Sleep`/`workflow.NewTimer`**. There must be **no timers in
the workflow history**.

- Dwell durations (per-step and the drone per-attempt time) are **hardcoded inside
  each activity function** as unexported package `var`s in `activities.go`
  (`stepDwell`, `deliveredDwell`, `droneAttempt`); the white-box test (`package
  pizza`) sets them to **zero via `init()`** so the suite stays fast.
- The order is marked `Done` **before** the final `Deliver` activity runs, so the
  completed all-green stepper is visible during that activity's dwell (the
  dashboard lists only Running workflows).

**Why:** the demo is customer-facing; a `workflow.Sleep` shows as a timer and
reads as a fake step, whereas activity execution time reads as real processing.
This holds even for the drone retry backoff — it is activity-side, not a
`workflow.Sleep`.

**How to apply:** to make a step take time, add a context-aware wait inside its
activity (not the workflow), using a package-level duration `var` that the test
zeroes. Durations are in [[demo-timing]].
