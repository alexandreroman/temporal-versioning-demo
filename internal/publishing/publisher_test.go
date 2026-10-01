package publishing_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
	"github.com/alexandreroman/temporal-versioning-demo/internal/publishing"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// testAttempt is the activity attempt the publish loop runs as.
const testAttempt = 3

// describeResult is one scripted answer to DescribeWorkflowExecution.
type describeResult struct {
	status enumspb.WorkflowExecutionStatus
	// pendingAttempt, when set, is the attempt of the pending PublishOrders.
	pendingAttempt int32
	err            error
}

var (
	running       = describeResult{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, pendingAttempt: testAttempt}
	superseded    = describeResult{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, pendingAttempt: testAttempt + 1}
	completed     = describeResult{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	terminated    = describeResult{status: enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED}
	notFound      = describeResult{err: serviceerror.NewNotFound("workflow not found")}
	describeError = describeResult{err: serviceerror.NewUnavailable("temporal down")}
)

// publisherClient answers describes from a script (Running once it runs out)
// and records every describe and order start. The publish loop runs on the
// test goroutine, so no locking is needed.
type publisherClient struct {
	client.Client

	script    []describeResult
	describes [][2]string // workflow ID, run ID
	orders    []client.StartWorkflowOptions
	workflows []any
	inputs    []pizza.OrderInput
}

func (c *publisherClient) DescribeWorkflowExecution(_ context.Context, workflowID, runID string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	c.describes = append(c.describes, [2]string{workflowID, runID})
	result := running
	if len(c.script) > 0 {
		result, c.script = c.script[0], c.script[1:]
	}
	if result.err != nil {
		return nil, result.err
	}
	resp := &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: result.status},
	}
	if result.pendingAttempt > 0 {
		resp.PendingActivities = []*workflowpb.PendingActivityInfo{{
			ActivityType: &commonpb.ActivityType{Name: publishing.PublishOrdersActivityName},
			Attempt:      result.pendingAttempt,
		}}
	}
	return resp, nil
}

func (c *publisherClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, wf any,
	args ...any,
) (client.WorkflowRun, error) {
	c.orders = append(c.orders, opts)
	c.workflows = append(c.workflows, wf)
	c.inputs = append(c.inputs, args[0].(pizza.OrderInput))
	return nil, nil
}

func TestPublisherPublish(t *testing.T) {
	const interval = time.Second
	tests := []struct {
		name string
		// script drives the run-state check made on every tick.
		script []describeResult
		// deadline, when set, bounds the loop like a schedule-to-close timeout.
		deadline time.Duration
		// workerStop, when set, is when the worker starts stopping.
		workerStop time.Duration
		wantErr    error
		wantOrders int
	}{
		{"stops once the run completes", []describeResult{running, running, completed}, 0, 0, nil, 2},
		{"stops at once when paused", []describeResult{terminated}, 0, 0, nil, 0},
		{"stops when the run is gone", []describeResult{running, notFound}, 0, 0, nil, 1},
		{
			"skips a tick on a describe error",
			[]describeResult{running, describeError, running, terminated},
			0, 0, nil, 2,
		},
		{"returns when its context is done", nil, 3*interval + interval/2, 0, context.DeadlineExceeded, 3},
		{"fails when the worker stops", nil, 0, interval / 2, publishing.ErrWorkerStopping, 0},
		{"stops once another attempt took over", []describeResult{running, superseded}, 0, 0, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &publisherClient{script: tt.script}
				p := publishing.NewPublisher(c, "q", interval, discardLogger())
				ctx := t.Context()
				if tt.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.deadline)
					defer cancel()
				}
				workerStop := make(chan struct{})
				if tt.workerStop > 0 {
					time.AfterFunc(tt.workerStop, func() { close(workerStop) })
				}
				// The bubble's clock stands still until the loop sleeps, so
				// this matches the seed the loop takes from it.
				seed := int(time.Now().Unix())
				heartbeats := 0

				info := activity.Info{
					WorkflowExecution: workflow.Execution{ID: "wf", RunID: "run-1"},
					Attempt:           testAttempt,
				}

				err := p.Publish(ctx, info, workerStop, func() { heartbeats++ })

				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				var wantIDs, gotIDs []string
				for i := range tt.wantOrders {
					wantIDs = append(wantIDs, fmt.Sprintf("order-%d", seed+1+i))
				}
				for _, opts := range c.orders {
					gotIDs = append(gotIDs, opts.ID)
				}
				if !slices.Equal(gotIDs, wantIDs) {
					t.Errorf("started %v, want %v", gotIDs, wantIDs)
				}
				for i, in := range c.inputs {
					wantPizza := pizza.Menu[in.OrderID%len(pizza.Menu)]
					if in.OrderID != seed+1+i || in.Pizza != wantPizza {
						t.Errorf("order %d input = %+v, want ID %d with %q", i, in, seed+1+i, wantPizza)
					}
					if c.orders[i].TaskQueue != "q" || c.workflows[i] != pizza.WorkflowTypeName {
						t.Errorf("order %d started %v on %q, want %q on q",
							i, c.workflows[i], c.orders[i].TaskQueue, pizza.WorkflowTypeName)
					}
				}
				for _, d := range c.describes {
					if d != [2]string{"wf", "run-1"} {
						t.Errorf("described %v, want its own run [wf run-1]", d)
					}
				}
				if heartbeats != len(c.describes) {
					t.Errorf("heartbeats = %d, want one per tick (%d)", heartbeats, len(c.describes))
				}
			})
		})
	}
}

// TestErrWorkerStoppingIsRetryable checks that a worker shutdown fails the
// attempt in a way Temporal retries, so publishing carries on on the next worker.
func TestErrWorkerStoppingIsRetryable(t *testing.T) {
	var appErr *temporal.ApplicationError
	if !errors.As(publishing.ErrWorkerStopping, &appErr) {
		t.Fatalf("ErrWorkerStopping = %T, want a *temporal.ApplicationError", publishing.ErrWorkerStopping)
	}
	if appErr.NonRetryable() {
		t.Error("ErrWorkerStopping is non-retryable, want retryable")
	}
}
