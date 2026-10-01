// Package publishing publishes pizza orders for a bounded time.
//
// A single OrderPublishing workflow, always under the same workflow ID, runs
// the PublishOrders activity, which starts one order per interval. Publishing
// is open while the workflow runs. It ends on its own when the activity's
// schedule-to-close window runs out, or at once when the workflow is
// terminated. Temporal owns the window, so it survives backend restarts.
package publishing

import (
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const (
	// WorkflowTypeName is the Temporal workflow type of order publishing.
	WorkflowTypeName = "OrderPublishing"
	// PublishOrdersActivityName is the Temporal activity type that starts orders.
	PublishOrdersActivityName = "PublishOrders"
	// TaskQueue is polled by the backend's unversioned publishing worker.
	TaskQueue = "order-publishing"
	// WorkflowID is fixed so there is at most one publishing run at a time.
	WorkflowID = "order-publishing"
)

// heartbeatTimeout only serves crash detection: when the backend dies, the
// activity stops heartbeating and is retried within a minute.
const heartbeatTimeout = time.Minute

// OrderPublishing publishes orders for timeout. It completes once the window
// runs out; pausing publishing terminates it.
func OrderPublishing(ctx workflow.Context, timeout time.Duration) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptions(timeout))
	err := workflow.ExecuteActivity(ctx, PublishOrdersActivityName).Get(ctx, nil)
	if windowRanOut(err) {
		// This is how publishing normally ends.
		return nil
	}
	return err
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

// Register registers the OrderPublishing workflow and p's PublishOrders
// activity on w under their Temporal type names.
func Register(w worker.Worker, p *Publisher) {
	w.RegisterWorkflowWithOptions(OrderPublishing, workflow.RegisterOptions{Name: WorkflowTypeName})
	w.RegisterActivityWithOptions(p.PublishOrders, activity.RegisterOptions{Name: PublishOrdersActivityName})
}
