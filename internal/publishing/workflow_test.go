package publishing_test

import (
	"context"
	"testing"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/publishing"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

const testTimeout = 15 * time.Minute

func TestOrderPublishing(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		// activity stands in for PublishOrders.
		activity func(ctx context.Context) error
		wantErr  bool
	}{
		{
			"schedule-to-close timeout completes the workflow",
			testTimeout,
			func(context.Context) error {
				return temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil)
			},
			false,
		},
		{
			// The test environment reports an attempt that runs into its
			// deadline with the retry state TIMEOUT, as the server does for an
			// attempt that fails too late to be retried.
			"attempt cut off by the window completes the workflow",
			50 * time.Millisecond,
			func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			false,
		},
		{
			"another failure fails the workflow",
			testTimeout,
			func(context.Context) error {
				return temporal.NewNonRetryableApplicationError("boom", "test", nil)
			},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			var info activity.Info
			env.RegisterActivityWithOptions(func(ctx context.Context) error {
				info = activity.GetInfo(ctx)
				return tt.activity(ctx)
			}, activity.RegisterOptions{Name: publishing.PublishOrdersActivityName})

			env.ExecuteWorkflow(publishing.OrderPublishing, tt.timeout)

			if !env.IsWorkflowCompleted() {
				t.Fatal("workflow did not complete")
			}
			if err := env.GetWorkflowError(); (err != nil) != tt.wantErr {
				t.Fatalf("workflow error = %v, wantErr %v", err, tt.wantErr)
			}
			// The workflow schedules the activity with its window.
			if info.ScheduleToCloseTimeout != tt.timeout {
				t.Errorf("schedule-to-close = %v, want %v", info.ScheduleToCloseTimeout, tt.timeout)
			}
		})
	}
}

func TestActivityOptions(t *testing.T) {
	opts := publishing.ActivityOptions(testTimeout)

	if opts.ScheduleToCloseTimeout != testTimeout {
		t.Errorf("schedule-to-close = %v, want %v", opts.ScheduleToCloseTimeout, testTimeout)
	}
	if opts.HeartbeatTimeout != time.Minute {
		t.Errorf("heartbeat timeout = %v, want %v", opts.HeartbeatTimeout, time.Minute)
	}
	p := opts.RetryPolicy
	if p == nil {
		t.Fatal("no retry policy, want an explicit one")
	}
	if p.InitialInterval != time.Second || p.MaximumInterval != 5*time.Second {
		t.Errorf("retry intervals = %v..%v, want 1s..5s", p.InitialInterval, p.MaximumInterval)
	}
	if p.MaximumAttempts != 0 {
		t.Errorf("maximum attempts = %d, want unlimited (0)", p.MaximumAttempts)
	}
}
