package pizza

import (
	"time"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// PizzaOrderV1 is the original pizza pipeline: take the order, cook it, hand it to a
// courier, deliver. This is where the demo started.
func PizzaOrderV1(ctx workflow.Context, in OrderInput) error {
	state := OrderState{
		Version: "v1",
		Pizza:   in.Pizza,
		Steps:   []StepLabel{StepReceived, StepCooking, StepOutForDelivery, StepDelivered},
	}
	if err := workflow.SetQueryHandler(ctx, GetStateQuery, func() (OrderState, error) {
		return state, nil
	}); err != nil {
		return err
	}

	// v1's activities always succeed, so a generous start-to-close timeout is all it
	// needs; Temporal's default activity retry applies but is never exercised here.
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: stepDwell + 15*time.Second,
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
	if err := workflow.ExecuteActivity(ctx, OutForDelivery, in).Get(ctx, nil); err != nil {
		return err
	}

	// Mark the order done before the final delivery runs so the all-green stepper stays
	// on the dashboard during that activity's dwell — the dashboard lists only Running
	// workflows, so the card leaves the board once this workflow completes.
	state.CurrentStep = 3
	state.Done = true
	return workflow.ExecuteActivity(ctx, Deliver, in).Get(ctx, nil)
}

// RegisterV1 registers the v1 workflow under the shared PizzaOrder type (Pinned) plus
// v1's own activities, on a worker built for v1.
func RegisterV1(w worker.Worker) {
	w.RegisterWorkflowWithOptions(PizzaOrderV1, workflow.RegisterOptions{
		Name:               WorkflowTypeName,
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterActivity(Receive)
	w.RegisterActivity(Cook)
	w.RegisterActivity(OutForDelivery)
	w.RegisterActivity(Deliver)
}
