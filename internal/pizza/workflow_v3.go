package pizza

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// droneRetryWindow bounds how long v3 keeps retrying the broken drone before giving up.
// 15 minutes covers a live demo (the presenter recovers stuck orders within minutes)
// while capping the Temporal Cloud actions a demo left unattended can burn: about 25-30
// attempts per stuck order at the ~35s cadence set by droneRetryMaxInterval.
// It is a var rather than a const so unit tests can shrink it (see workflow_test.go).
var droneRetryWindow = 15 * time.Minute

// droneRetryMaxInterval caps the drone's exponential retry backoff (default 1s initial
// interval, doubled on each attempt). Once capped, a stuck order retries roughly every
// ~35s (a 5s attempt plus a 30s wait): about 100 attempts per hour, which keeps Temporal
// Cloud actions low without any visible change on the dashboard (it shows no retry counter).
const droneRetryMaxInterval = 30 * time.Second

// PizzaOrderV3 is v2 with the courier hand-off replaced by an (intentionally broken)
// drone delivery — a regression a team might ship and then roll back.
func PizzaOrderV3(ctx workflow.Context, in OrderInput) error {
	state := OrderState{
		Version: "v3",
		Pizza:   in.Pizza,
		Steps:   []StepLabel{StepReceived, StepCooking, StepQualityCheck, StepDroneDelivery, StepDelivered},
	}
	if err := workflow.SetQueryHandler(ctx, GetStateQuery, func() (OrderState, error) {
		return state, nil
	}); err != nil {
		return err
	}

	// v3 introduces the deterministically-broken drone, so it tunes its own retry:
	// unlimited attempts (MaximumAttempts: 0) with the backoff capped at
	// droneRetryMaxInterval, so a failing order stays red/Running — the regression the demo
	// rolls back. The drone step bounds that retry by duration (see droneRetryWindow). The
	// drone's per-attempt wait is activity-side, so no workflow timer.
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: stepDwell + 15*time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 0, MaximumInterval: droneRetryMaxInterval},
	})

	state.CurrentStep = 0
	if err := workflow.ExecuteActivity(ctx, Receive, in).Get(ctx, nil); err != nil {
		return err
	}

	state.CurrentStep = 1
	if err := workflow.ExecuteActivity(ctx, Cook, in).Get(ctx, nil); err != nil {
		return err
	}

	state.CurrentStep = 2
	if err := workflow.ExecuteActivity(ctx, QualityCheck, in).Get(ctx, nil); err != nil {
		return err
	}

	// The drone is deterministically broken, so mark this step failing as we enter it.
	// Native retry keeps re-attempting it, so the order stalls red/Running until it is
	// recovered onto a healthy version. If nobody recovers it, the schedule-to-close
	// timeout stops the retry after droneRetryWindow and the workflow fails with the
	// resulting activity error, which takes the order off the dashboard (it lists
	// Running workflows only).
	state.CurrentStep = 3
	state.Failing = true
	droneCtx := workflow.WithScheduleToCloseTimeout(ctx, droneRetryWindow)
	if err := workflow.ExecuteActivity(droneCtx, DroneDelivery, in).Get(droneCtx, nil); err != nil {
		return err
	}

	// Unreachable while the drone stays broken, but kept so the shape mirrors v1/v2.
	state.CurrentStep = 4
	state.Failing = false
	state.Done = true
	return workflow.ExecuteActivity(ctx, Deliver, in).Get(ctx, nil)
}

// RegisterV3 registers the v3 workflow under the shared PizzaOrder type (Pinned) plus
// v3's own activities, on a worker built for v3.
func RegisterV3(w worker.Worker) {
	w.RegisterWorkflowWithOptions(PizzaOrderV3, workflow.RegisterOptions{
		Name:               WorkflowTypeName,
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterActivity(Receive)
	w.RegisterActivity(Cook)
	w.RegisterActivity(QualityCheck)
	w.RegisterActivity(DroneDelivery)
	w.RegisterActivity(Deliver)
}
