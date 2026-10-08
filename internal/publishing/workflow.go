// Package publishing publishes pizza orders for a bounded time.
//
// A single OrderPublishing workflow, always under the same workflow ID, runs
// the PublishOrders activity, which starts one order per interval. Publishing
// is open while the workflow runs and has not marked publishing as stopped in
// its memo. It stops on its own when the activity's schedule-to-close window
// runs out, or at once on a pause signal. Temporal owns the window, so it
// survives backend restarts.
//
// Once publishing stops, the run enters its closing phase: it waits closeDelay
// on a durable timer, then the CloseOrders activity terminates the orders still
// running, by then only stuck ones. Resuming publishing terminates the run and
// starts a new one, so a resume within the delay keeps every order.
package publishing

import (
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const (
	// WorkflowTypeName is the Temporal workflow type of order publishing.
	WorkflowTypeName = "OrderPublishing"
	// PublishOrdersActivityName is the Temporal activity type that starts orders.
	PublishOrdersActivityName = "PublishOrders"
	// CloseOrdersActivityName is the Temporal activity type that terminates the
	// orders left open once publishing has stopped.
	CloseOrdersActivityName = "CloseOrders"
	// PauseSignalName is the signal that stops publishing.
	PauseSignalName = "pause"
	// TaskQueue is polled by the backend's unversioned publishing worker.
	TaskQueue = "order-publishing"
	// WorkflowID is fixed so there is at most one publishing run at a time.
	WorkflowID = "order-publishing"
)

// The run records why publishing stopped in its memo under stoppedMemoKey, so
// a Describe alone tells a run in its closing phase from one still publishing.
const (
	stoppedMemoKey    = "publishingStopped"
	stopReasonTimeout = "timeout"
	stopReasonPaused  = "paused"
)

const (
	// heartbeatTimeout only serves crash detection: when the backend dies, the
	// activity stops heartbeating and is retried within a minute.
	heartbeatTimeout = time.Minute
	// closeDelay is how long a run waits after publishing stops before closing
	// the orders left open. Healthy orders finish well within it, and it leaves
	// time to resume, which keeps every order.
	closeDelay = 10 * time.Minute
	// closeOrdersTimeout bounds CloseOrders, all attempts together.
	closeOrdersTimeout = time.Minute
)

// OrderPublishing publishes orders for timeout, or until a pause signal. It
// then waits closeDelay, closes the orders left open and completes.
func OrderPublishing(ctx workflow.Context, timeout time.Duration) error {
	reason, err := publish(ctx, timeout)
	if err != nil {
		return err
	}
	// This runs in the workflow task that ended publishing, so no Describe can
	// see the run Running without the mark once it has stopped publishing.
	if err := workflow.UpsertMemo(ctx, map[string]any{stoppedMemoKey: reason}); err != nil {
		return err
	}
	return closeOrders(ctx)
}

// publish runs PublishOrders until its window runs out or a pause signal
// arrives, and returns the stop reason.
func publish(ctx workflow.Context, timeout time.Duration) (string, error) {
	publishCtx, cancel := workflow.WithCancel(ctx)
	workflow.Go(ctx, func(ctx workflow.Context) {
		// Keep receiving for the whole run: a pause during the closing phase is
		// consumed here and changes nothing, since cancel only reaches
		// PublishOrders.
		pause := workflow.GetSignalChannel(ctx, PauseSignalName)
		for {
			pause.Receive(ctx, nil)
			cancel()
		}
	})

	publishCtx = workflow.WithActivityOptions(publishCtx, activityOptions(timeout))
	err := workflow.ExecuteActivity(publishCtx, PublishOrdersActivityName).Get(publishCtx, nil)
	switch {
	case temporal.IsCanceledError(err):
		return stopReasonPaused, nil
	case err == nil || windowRanOut(err):
		// This is how publishing normally ends. PublishOrders returns nil only
		// once it finds its run no longer current; should that result still
		// reach the run, publishing ended on its own all the same.
		return stopReasonTimeout, nil
	default:
		return "", err
	}
}

// closeOrders waits closeDelay, then terminates the orders still running.
func closeOrders(ctx workflow.Context) error {
	// A durable timer: the wait survives backend restarts, and a resume ends it
	// by terminating this run.
	if err := workflow.Sleep(ctx, closeDelay); err != nil {
		return err
	}
	// The bound is taken after the wait, not when publishing stopped. A resume
	// terminates this run, so no newer run can have started an order before the
	// timer fired: every order started earlier belongs to this run or an older
	// one, including one that slipped in just as publishing stopped. The bound
	// still spares the orders of a run that a resume starts while CloseOrders is
	// executing, since the activity can outlive this run's termination.
	startedBefore := workflow.Now(ctx)
	ctx = workflow.WithActivityOptions(ctx, closeOrdersActivityOptions())
	return workflow.ExecuteActivity(ctx, CloseOrdersActivityName, startedBefore).Get(ctx, nil)
}

// activityOptions schedules PublishOrders for a window of timeout.
func activityOptions(timeout time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// Schedule-to-close rather than start-to-close: it bounds all attempts
		// together, so an attempt retried after a backend crash only gets the
		// time left in the window.
		ScheduleToCloseTimeout: timeout,
		HeartbeatTimeout:       heartbeatTimeout,
		// Every backend restart hands the activity over through a retry, so the
		// backoff is capped low: with the default 100 s cap, a few restarts in
		// one window would stall publishing for minutes. Attempts stay
		// unlimited; the schedule-to-close window bounds them.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumInterval: 5 * time.Second,
		},
	}
}

// closeOrdersActivityOptions schedules CloseOrders. It is idempotent, so it is
// retried with the same capped backoff as PublishOrders until its timeout.
func closeOrdersActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		ScheduleToCloseTimeout: closeOrdersTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumInterval: 5 * time.Second,
		},
	}
}

// windowRanOut reports whether err means the activity's window is over. The
// server reports it as a schedule-to-close timeout, or, when an attempt fails
// too close to the deadline to be retried, as that attempt's error with the
// retry state TIMEOUT. Both mean the same thing here.
func windowRanOut(err error) bool {
	var activityErr *temporal.ActivityError
	if errors.As(err, &activityErr) && activityErr.RetryState() == enumspb.RETRY_STATE_TIMEOUT {
		return true
	}
	var timeoutErr *temporal.TimeoutError
	return errors.As(err, &timeoutErr) && timeoutErr.TimeoutType() == enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE
}

// stopReason reads the publishing-stopped mark from a run's memo. The boolean
// is false while the run is still publishing, or when info is nil (no run). A
// mark whose reason cannot be decoded still counts as stopped, with an empty
// reason: the run only sets it once publishing has stopped.
func stopReason(info *workflowpb.WorkflowExecutionInfo) (string, bool) {
	payload, ok := info.GetMemo().GetFields()[stoppedMemoKey]
	if !ok {
		return "", false
	}
	var reason string
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &reason); err != nil {
		return "", true
	}
	return reason, true
}

// Register registers the OrderPublishing workflow and p's activities on w under
// their Temporal type names.
func Register(w worker.Worker, p *Publisher) {
	w.RegisterWorkflowWithOptions(OrderPublishing, workflow.RegisterOptions{Name: WorkflowTypeName})
	w.RegisterActivityWithOptions(p.PublishOrders, activity.RegisterOptions{Name: PublishOrdersActivityName})
	w.RegisterActivityWithOptions(p.CloseOrders, activity.RegisterOptions{Name: CloseOrdersActivityName})
}
