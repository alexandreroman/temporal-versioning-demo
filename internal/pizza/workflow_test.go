package pizza

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// Activity dwell times are hardcoded in activities.go; zero them here so the test suite
// runs instantly instead of waiting the real demo pacing.
func init() {
	stepDwell = 0
	deliveredDwell = 0
	droneAttempt = 0
}

// registerActivities registers all pizza activities on a test environment. A workflow
// only executes the ones its version uses; registering the full set keeps the tests
// simple (production workers register only their version's set via RegisterVN).
func registerActivities(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivity(Receive)
	env.RegisterActivity(Cook)
	env.RegisterActivity(QualityCheck)
	env.RegisterActivity(OutForDelivery)
	env.RegisterActivity(Deliver)
	env.RegisterActivity(DroneDelivery)
}

// queryState reads a workflow's OrderState through the shared getState query — now the
// only way to inspect a version's steps, since there is no static step table.
func queryState(t *testing.T, env *testsuite.TestWorkflowEnvironment) OrderState {
	t.Helper()
	val, err := env.QueryWorkflow(GetStateQuery)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	var st OrderState
	if err := val.Get(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return st
}

func TestV1CompletesFourSteps(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerActivities(env)

	env.ExecuteWorkflow(PizzaOrderV1, OrderInput{OrderID: 1, Pizza: "Margherita"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	st := queryState(t, env)
	want := []StepLabel{StepReceived, StepCooking, StepOutForDelivery, StepDelivered}
	if !slices.Equal(st.Steps, want) {
		t.Fatalf("v1 steps = %v, want %v", st.Steps, want)
	}
	if st.Version != "v1" {
		t.Fatalf("v1 version = %q, want v1", st.Version)
	}
	if !st.Done {
		t.Fatalf("v1 should finish all-green, got Done=%v", st.Done)
	}
}

func TestV2HasQualityCheckStep(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerActivities(env)

	env.ExecuteWorkflow(PizzaOrderV2, OrderInput{OrderID: 2, Pizza: "Pepperoni"})

	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("v2 should complete cleanly: completed=%v err=%v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	st := queryState(t, env)
	if len(st.Steps) != 5 || st.Steps[2] != StepQualityCheck {
		t.Fatalf("v2 must have a Quality check as the 3rd step, got %v", st.Steps)
	}
}

func TestV3StallsOnDrone(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerActivities(env)

	// v3 stalls on the always-failing drone. Native retry keeps re-attempting the drone
	// activity for droneRetryWindow, so the workflow stays Running, marked failing, until
	// it is recovered or the window elapses. We therefore inspect the stalled state once
	// the retry loop is observed and then cancel, rather than running to completion.
	//
	// Waiting for the *second* DroneDelivery start is the deterministic signal: it
	// proves the first attempt already failed and the durable retry is under way, so
	// the workflow is genuinely stalled on the drone step. A fixed mock-clock delay is
	// not reliable here — with zero activity dwells the clock barely advances and the
	// callback can fire before the workflow reaches the drone.
	droneStarts := 0
	asserted := false
	env.SetOnActivityStartedListener(
		func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
			if info.ActivityType.Name != "DroneDelivery" {
				return
			}
			droneStarts++
			if droneStarts < 2 || asserted {
				return
			}
			asserted = true

			val, err := env.QueryWorkflow(GetStateQuery)
			if err != nil {
				t.Errorf("query failed: %v", err)
				return
			}
			var st OrderState
			if err := val.Get(&st); err != nil {
				t.Errorf("decode state: %v", err)
				return
			}
			if st.Version != "v3" {
				t.Errorf("expected version v3, got %q", st.Version)
			}
			if !st.Failing {
				t.Errorf("expected drone step to be failing, got %+v", st)
			}
			if st.Done {
				t.Errorf("v3 must not complete while the drone is failing, got Done=true")
			}
			if st.Steps[st.CurrentStep] != StepDroneDelivery {
				t.Errorf("expected current step Drone, got %v", st.Steps[st.CurrentStep])
			}
			// Release the blocked drone .Get so the test ends instead of hanging.
			env.CancelWorkflow()
		},
	)

	env.ExecuteWorkflow(PizzaOrderV3, OrderInput{OrderID: 3, Pizza: "Diavola"})

	if !asserted {
		t.Fatal("drone never reached its retry loop; stall not observed")
	}
}

func TestV3FailsOnceDroneRetryWindowElapses(t *testing.T) {
	// The test environment caps "unlimited" retries at 10 attempts. On its auto-skipping
	// mock clock the backoff (1s, 2s, 4s, ... capped at droneRetryMaxInterval) spreads
	// those attempts over ~2.5 minutes, so the real one-hour window can never be the limit
	// here. Shrink the window to 5s instead: it still fits a few attempts (at 0s, 1s and
	// 3s), and failing within it proves the window, not the test environment's attempt
	// cap, ended the drone's retries.
	original := droneRetryWindow
	droneRetryWindow = 5 * time.Second
	t.Cleanup(func() { droneRetryWindow = original })

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerActivities(env)

	droneStarts := 0
	var firstDroneStart time.Time
	env.SetOnActivityStartedListener(
		func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
			if info.ActivityType.Name != "DroneDelivery" {
				return
			}
			droneStarts++
			if droneStarts == 1 {
				firstDroneStart = env.Now()
			}
		},
	)

	env.ExecuteWorkflow(PizzaOrderV3, OrderInput{OrderID: 4, Pizza: "Capricciosa"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("v3 should end once the drone retry window elapses")
	}
	err := env.GetWorkflowError()
	var activityErr *temporal.ActivityError
	if !errors.As(err, &activityErr) {
		t.Fatalf("expected the drone's activity error, got %v", err)
	}
	if droneStarts < 2 {
		t.Fatalf("expected the drone to be retried, got %d attempt(s)", droneStarts)
	}
	if elapsed := env.Now().Sub(firstDroneStart); elapsed > droneRetryWindow {
		t.Fatalf("drone retried for %v, want at most the %v retry window", elapsed, droneRetryWindow)
	}
}
