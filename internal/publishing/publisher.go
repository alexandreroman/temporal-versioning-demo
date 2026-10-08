package publishing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// errWorkerStopping fails the attempt when the worker shuts down. The failure
// is reported before the worker exits, so Temporal retries the activity on the
// next worker after a short backoff instead of waiting for the heartbeat
// timeout. It stays retryable; the benign category only keeps it out of the
// error logs, since it happens on every backend restart.
var errWorkerStopping = temporal.NewApplicationErrorWithOptions("worker stopping", "WorkerStopping",
	temporal.ApplicationErrorOptions{Category: temporal.ApplicationErrorCategoryBenign})

// Publisher hosts the PublishOrders activity, which starts pizza orders so
// there are always in-flight workflows, and the CloseOrders activity, which
// terminates the ones left open once publishing has stopped.
type Publisher struct {
	c         client.Client
	taskQueue string
	interval  time.Duration
	logger    *slog.Logger
}

// NewPublisher builds a Publisher that starts one order per interval on the
// pizza task queue taskQueue.
func NewPublisher(c client.Client, taskQueue string, interval time.Duration, logger *slog.Logger) *Publisher {
	return &Publisher{c: c, taskQueue: taskQueue, interval: interval, logger: logger}
}

// PublishOrders starts one order per interval until its context is done (the
// schedule-to-close deadline), the worker starts stopping, or this attempt is
// no longer the current one of a running workflow run.
func (p *Publisher) PublishOrders(ctx context.Context) error {
	info := activity.GetInfo(ctx)
	workerStop := activity.GetWorkerStopChannel(ctx)
	heartbeat := func() { activity.RecordHeartbeat(ctx) }
	return p.publish(ctx, info, workerStop, heartbeat)
}

// publish is the PublishOrders loop, with the activity context lookups lifted
// out so tests can drive it directly.
//
// Orders are started WITHOUT an explicit version so Temporal routes them by the
// deployment's Current/Ramping config; Pinned behaviour then locks each to its
// start version.
func (p *Publisher) publish(ctx context.Context, info activity.Info, workerStop <-chan struct{},
	heartbeat func(),
) error {
	// Seed the order counter from the wall clock so order IDs do not reset to
	// order-1 on every attempt or backend restart and collide with a still-open
	// order from an earlier one. This is activity code, so the clock is fine.
	id := int(time.Now().Unix())
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-workerStop:
			return errWorkerStopping
		case <-t.C:
		}
		heartbeat()

		current, err := p.isCurrent(ctx, info)
		if err != nil {
			// Skip before incrementing so a failed check does not burn an order ID.
			p.logger.Warn("failed to check order publishing run, skipping tick", "err", err)
			continue
		}
		if !current {
			// Paused, timed out, replaced or superseded: stop without one more
			// order.
			return nil
		}

		id++
		in := pizza.OrderInput{OrderID: id, Pizza: pizza.Menu[id%len(pizza.Menu)]}
		opts := client.StartWorkflowOptions{
			ID:        fmt.Sprintf("order-%d", id),
			TaskQueue: p.taskQueue,
		}
		if _, err := p.c.ExecuteWorkflow(ctx, opts, pizza.WorkflowTypeName, in); err != nil {
			p.logger.Warn("failed to start order", "orderId", id, "err", err)
		}
	}
}

// isCurrent reports whether this attempt should keep publishing: its workflow
// run is still Running, has not marked publishing as stopped and has not moved
// on to another attempt. A run that does not exist any more counts as not
// current.
func (p *Publisher) isCurrent(ctx context.Context, info activity.Info) (bool, error) {
	// Describe the run by its run ID, not just the workflow ID: after a Resume a
	// new run holds the same workflow ID, and this attempt must not keep
	// publishing alongside the new run's.
	resp, err := p.c.DescribeWorkflowExecution(ctx, info.WorkflowExecution.ID, info.WorkflowExecution.RunID)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if resp.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return false, nil
	}
	// A pause cancels this activity, but the cancellation only reaches it
	// through a heartbeat response; the memo mark stops it before the next order.
	if _, stopped := stopReason(resp.GetWorkflowExecutionInfo()); stopped {
		return false, nil
	}
	for _, pending := range resp.GetPendingActivities() {
		// Another attempt number means this one was given up on (its process
		// froze past the heartbeat timeout, say) and retried elsewhere.
		if pending.GetActivityType().GetName() == PublishOrdersActivityName && pending.GetAttempt() != info.Attempt {
			return false, nil
		}
	}
	return true, nil
}

// CloseOrders terminates the pizza orders started before startedBefore that are
// still running. The run calls it a while after publishing stopped, when only
// stuck orders remain: v3 drone retries, or orders pinned to a version that has
// no workers any more.
//
// Orders started from startedBefore on are left alone: they belong to a newer
// publishing run. Terminating an order that has closed in the meantime is not
// an error, so a retry after a partial failure picks up where it left off.
func (p *Publisher) CloseOrders(ctx context.Context, startedBefore time.Time) error {
	orders, err := p.openOrdersBefore(ctx, startedBefore)
	if err != nil {
		return err
	}
	closed := 0
	for _, order := range orders {
		err := p.c.TerminateWorkflow(ctx, order.GetWorkflowId(), order.GetRunId(), "order publishing stopped")
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("terminate order %s: %w", order.GetWorkflowId(), err)
		}
		closed++
	}
	p.logger.Info("closed the orders left open after publishing stopped", "count", closed)
	return nil
}

// openOrdersBefore lists every running PizzaOrder started before startedBefore.
//
// All pages are read before any order is terminated: terminating orders while
// paging through a query on ExecutionStatus could shift the later pages.
func (p *Publisher) openOrdersBefore(ctx context.Context, startedBefore time.Time,
) ([]*commonpb.WorkflowExecution, error) {
	query := fmt.Sprintf("WorkflowType = '%s' AND ExecutionStatus = 'Running' AND StartTime < '%s'",
		pizza.WorkflowTypeName, startedBefore.UTC().Format(time.RFC3339Nano))
	var orders []*commonpb.WorkflowExecution
	var pageToken []byte
	for {
		// Namespace is left empty: the SDK fills it from the client's configuration.
		resp, err := p.c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         query,
			NextPageToken: pageToken,
		})
		if err != nil {
			return nil, fmt.Errorf("list open orders: %w", err)
		}
		for _, execution := range resp.GetExecutions() {
			orders = append(orders, execution.GetExecution())
		}
		pageToken = resp.GetNextPageToken()
		if len(pageToken) == 0 {
			return orders, nil
		}
	}
}
