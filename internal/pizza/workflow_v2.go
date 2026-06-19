package pizza

import (
	"time"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// PizzaOrderV2 is v1 with a quality-check step added after cooking — the kind of
// incremental change a team ships as the next version of the workflow.
func PizzaOrderV2(ctx workflow.Context, in OrderInput) error {
	state := OrderState{
		Version: "v2",
		Pizza:   in.Pizza,
		Steps:   []StepLabel{StepReceived, StepCooking, StepQualityCheck, StepOutForDelivery, StepDelivered},
	}
	if err := workflow.SetQueryHandler(ctx, GetStateQuery, func() (OrderState, error) {
		return state, nil
	}); err != nil {
		return err
	}

	// v2's activities always succeed, so a generous start-to-close timeout is all it
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
	if err := workflow.ExecuteActivity(ctx, QualityCheck, in).Get(ctx, nil); err != nil {
		return err
	}

	state.CurrentStep = 3
	if err := workflow.ExecuteActivity(ctx, OutForDelivery, in).Get(ctx, nil); err != nil {
		return err
	}

	// Mark the order done before the final delivery runs so the all-green stepper stays
	// on the dashboard during that activity's dwell.
	state.CurrentStep = 4
	state.Done = true
	return workflow.ExecuteActivity(ctx, Deliver, in).Get(ctx, nil)
}

// RegisterV2 registers the v2 workflow under the shared PizzaOrder type (Pinned) plus
// v2's own activities, on a worker built for v2.
func RegisterV2(w worker.Worker) {
	w.RegisterWorkflowWithOptions(PizzaOrderV2, workflow.RegisterOptions{
		Name:               WorkflowTypeName,
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterActivity(Receive)
	w.RegisterActivity(Cook)
	w.RegisterActivity(QualityCheck)
	w.RegisterActivity(OutForDelivery)
	w.RegisterActivity(Deliver)
}
