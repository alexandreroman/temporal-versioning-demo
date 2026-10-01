package publishing

import (
	"context"

	"go.temporal.io/sdk/activity"
)

// ActivityOptions is how OrderPublishing schedules PublishOrders.
var ActivityOptions = activityOptions

// ExecutionTimeoutMargin is how long a publishing run may outlive its window.
const ExecutionTimeoutMargin = executionTimeoutMargin

// ErrWorkerStopping is the error PublishOrders fails with when its worker stops.
var ErrWorkerStopping = errWorkerStopping

// Publish runs the PublishOrders loop outside an activity, for tests.
func (p *Publisher) Publish(ctx context.Context, info activity.Info, workerStop <-chan struct{},
	heartbeat func(),
) error {
	return p.publish(ctx, info, workerStop, heartbeat)
}
