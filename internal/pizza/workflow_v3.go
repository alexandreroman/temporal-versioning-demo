package pizza

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

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
	// retry forever (MaximumAttempts: 0) with the backoff capped at droneAttempt, so a
	// failing order stays red/Running and never fails/completes — the regression the
	// demo rolls back. The drone's per-attempt wait is activity-side, so no workflow timer.
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: stepDwell + 15*time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 0, MaximumInterval: droneAttempt},
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
	// With unlimited native retry this Get blocks until the activity succeeds (it never
	// does) or the workflow is cancelled, so the order stalls red/Running forever.
	state.CurrentStep = 3
	state.Failing = true
	if err := workflow.ExecuteActivity(ctx, DroneDelivery, in).Get(ctx, nil); err != nil {
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
