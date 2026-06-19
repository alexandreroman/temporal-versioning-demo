---
name: "Each pizza workflow version is self-contained; activities are shared exported funcs with hardcoded dwells"
description: "v1/v2/v3 each own their workflow_vN.go (inline chain, state/getState, activity options, RegisterVN); activities are exported funcs in activities.go with dwells hardcoded as package vars; each RegisterVN registers only its version's funcs; only types.go contract + main.go version switch are shared"
type: feedback
---

# Each pizza workflow version is self-contained; activities are shared exported funcs with hardcoded dwells

Each version lives alone in `internal/pizza/workflow_vN.go` and is **self-contained** —
there is no shared workflow-logic file. Every version, inline and on its own:

- builds its own `OrderState{Version: "vN", Pizza, Steps: []StepLabel{...}}` and
  registers the `getState` query itself via `workflow.SetQueryHandler`;
- sets its **own** `workflow.ActivityOptions` (v1/v2: just a `StartToCloseTimeout`;
  v3 adds the drone's `RetryPolicy{MaximumAttempts:0, MaximumInterval: droneAttempt}`);
- runs activities as a literal top-to-bottom sequence of
  `workflow.ExecuteActivity(ctx, Receive, in).Get(ctx, nil)`, advancing
  `state.CurrentStep` / setting `state.Failing` (v3 drone) / `state.Done` inline;
- exposes its own `RegisterVN(w worker.Worker)` that registers the workflow plus
  **only the activity functions that version runs**.

**Activities — shared exported funcs, dwells hardcoded.** `activities.go` holds the
activity implementations as **exported package functions** with signature
`func Name(ctx context.Context, in OrderInput) error`: `Receive`, `Cook`,
`QualityCheck`, `OutForDelivery`, `Deliver`, `DroneDelivery` (plus the `dwell` helper).
They are exported so the Temporal activity type names stay capitalized in the Web UI.
Each function **hardcodes its own dwell** — no duration parameter. The dwell values are
unexported package `var`s in `activities.go` (`stepDwell`, `deliveredDwell`,
`droneAttempt`) so the white-box test (`package pizza`) can zero them via `init()` for
speed (see [[workflow-waits-activity-side]], [[demo-timing]]).

Each `RegisterVN` registers **only its version's activity functions** — v1:
`Receive/Cook/OutForDelivery/Deliver`; v2: + `QualityCheck`; v3:
`Receive/Cook/QualityCheck/DroneDelivery/Deliver` (no `OutForDelivery`). A worker never
registers another version's activities (no Drone on v1/v2).

**Keep versions self-contained.** Duplication across the three files is intended — it
mirrors a team evolving the workflow over time (v2 = v1 + QC; v3 = v2 with Out→Drone).
Per-version workflow logic stays in its own file and is never centralized into a shared
helper, table, or activity struct; the only shared pieces are the contract (`types.go`)
and the activity implementations (`activities.go`).

`types.go` holds the **contract** only: the `OrderState` type and `GetStateQuery` name
(the common interface for retrieving steps), `WorkflowTypeName`, the `StepLabel` step
consts, `TaskQueue`, `Menu`, `OrderInput`. `WorkflowTypeName` is a single shared const so
all versions register the same type — that is what lets Worker Versioning route
`PizzaOrder` across Worker Deployment Versions. Timing values live in `activities.go`.

Worker bootstrap is the one place that knows about versions: `cmd/worker/main.go` does
`switch v { case V1: pizza.RegisterV1(w); ... }` after `ParseVersion(PIZZA_VERSION)`. The
`Version` type/`ParseVersion` exist only for that selection; workflow logic never branches
on version.

**Why:** the demo is customer-facing and pedagogical — it must read like a workflow a real
team grew across versions, each frozen as shipped, with each version owning its own steps,
options, and registered activity surface.

**How to apply:** to add a v4, copy the closest `workflow_vN.go`, edit its inline
steps/state/options and its `RegisterV4`, extend the `main.go` switch and the `Version`
enum. Add any new activity as an exported func in `activities.go` (dwell hardcoded via a
package `var` the test can zero). Keep `WorkflowTypeName` shared and `Pinned`; tests read
steps through the `getState` query.
