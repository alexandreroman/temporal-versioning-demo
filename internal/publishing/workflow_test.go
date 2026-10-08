package publishing_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/publishing"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

const testTimeout = 15 * time.Minute

// closeOrdersCall records one CloseOrders run in the test environment.
type closeOrdersCall struct {
	startedBefore time.Time
	startedAt     time.Time // workflow clock when the activity ran
	info          activity.Info
}

func TestOrderPublishing(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		// publish stands in for PublishOrders. canceled is closed once the
		// workflow cancels it.
		publish func(ctx context.Context, env *testsuite.TestWorkflowEnvironment, canceled <-chan struct{}) error
		// pauseDuringWait sends a pause signal while the run waits to close orders.
		pauseDuringWait bool
		wantErr         bool
		// wantReason is the stop reason the run records before closing the
		// orders left open; empty when it must not get that far.
		wantReason string
	}{
		{
			name:    "schedule-to-close timeout closes the orders after the delay",
			timeout: testTimeout,
			publish: func(context.Context, *testsuite.TestWorkflowEnvironment, <-chan struct{}) error {
				return temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil)
			},
			wantReason: "timeout",
		},
		{
			// The test environment reports an attempt that runs into its
			// deadline with the retry state TIMEOUT, as the server does for an
			// attempt that fails too late to be retried.
			name:    "attempt cut off by the window closes the orders after the delay",
			timeout: 50 * time.Millisecond,
			publish: func(ctx context.Context, _ *testsuite.TestWorkflowEnvironment, _ <-chan struct{}) error {
				<-ctx.Done()
				return ctx.Err()
			},
			wantReason: "timeout",
		},
		{
			name:    "pause signal cancels publishing and closes the orders after the delay",
			timeout: testTimeout,
			publish: func(_ context.Context, env *testsuite.TestWorkflowEnvironment, canceled <-chan struct{}) error {
				env.SignalWorkflow(publishing.PauseSignalName, nil)
				// The test environment does not cancel the activity context, so
				// wait for the workflow's cancellation instead. The workflow has
				// moved on by then and ignores this result.
				<-canceled
				return errors.New("canceled")
			},
			wantReason: "paused",
		},
		{
			name:    "pause signal while waiting to close is ignored",
			timeout: testTimeout,
			publish: func(context.Context, *testsuite.TestWorkflowEnvironment, <-chan struct{}) error {
				return temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil)
			},
			pauseDuringWait: true,
			wantReason:      "timeout",
		},
		{
			name:    "another failure fails the workflow without closing orders",
			timeout: testTimeout,
			publish: func(context.Context, *testsuite.TestWorkflowEnvironment, <-chan struct{}) error {
				return temporal.NewNonRetryableApplicationError("boom", "test", nil)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()

			var publishInfo activity.Info
			canceled := make(chan struct{})
			env.SetOnActivityCanceledListener(func(info *activity.Info) {
				if info.ActivityType.Name == publishing.PublishOrdersActivityName {
					close(canceled)
				}
			})
			env.RegisterActivityWithOptions(func(ctx context.Context) error {
				publishInfo = activity.GetInfo(ctx)
				return tt.publish(ctx, env, canceled)
			}, activity.RegisterOptions{Name: publishing.PublishOrdersActivityName})

			var closeCalls []closeOrdersCall
			env.RegisterActivityWithOptions(func(ctx context.Context, startedBefore time.Time) error {
				closeCalls = append(closeCalls, closeOrdersCall{
					startedBefore: startedBefore,
					startedAt:     env.Now(),
					info:          activity.GetInfo(ctx),
				})
				return nil
			}, activity.RegisterOptions{Name: publishing.CloseOrdersActivityName})

			var timers []time.Duration
			env.SetOnTimerScheduledListener(func(_ string, d time.Duration) {
				timers = append(timers, d)
			})
			if tt.wantReason != "" {
				env.OnUpsertMemo(map[string]any{"publishingStopped": tt.wantReason}).Return(nil).Once()
			}
			if tt.pauseDuringWait {
				env.RegisterDelayedCallback(func() {
					env.SignalWorkflow(publishing.PauseSignalName, nil)
				}, publishing.CloseDelay/2)
			}
			start := env.Now()

			env.ExecuteWorkflow(publishing.OrderPublishing, tt.timeout)

			if !env.IsWorkflowCompleted() {
				t.Fatal("workflow did not complete")
			}
			if err := env.GetWorkflowError(); (err != nil) != tt.wantErr {
				t.Fatalf("workflow error = %v, wantErr %v", err, tt.wantErr)
			}
			// The workflow schedules PublishOrders with its window.
			if publishInfo.ScheduleToCloseTimeout != tt.timeout {
				t.Errorf("schedule-to-close = %v, want %v", publishInfo.ScheduleToCloseTimeout, tt.timeout)
			}
			env.AssertExpectations(t)

			if tt.wantReason == "" {
				if len(closeCalls) != 0 || len(timers) != 0 {
					t.Errorf("CloseOrders ran %d times after %v, want no closing phase", len(closeCalls), timers)
				}
				return
			}
			if !slices.Equal(timers, []time.Duration{publishing.CloseDelay}) {
				t.Errorf("timers = %v, want [%v]", timers, publishing.CloseDelay)
			}
			if len(closeCalls) != 1 {
				t.Fatalf("CloseOrders ran %d times, want 1", len(closeCalls))
			}
			call := closeCalls[0]
			// The bound is taken once the wait is over, when CloseOrders is
			// scheduled, so it covers every order this run started.
			if !call.startedBefore.Equal(call.startedAt) || call.startedBefore.Before(start.Add(publishing.CloseDelay)) {
				t.Errorf("CloseOrders(startedBefore %v) ran at %v, want the time it is scheduled, at least %v after %v",
					call.startedBefore, call.startedAt, publishing.CloseDelay, start)
			}
			if call.info.ScheduleToCloseTimeout != publishing.CloseOrdersTimeout {
				t.Errorf("CloseOrders schedule-to-close = %v, want %v",
					call.info.ScheduleToCloseTimeout, publishing.CloseOrdersTimeout)
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

func TestCloseOrdersActivityOptions(t *testing.T) {
	opts := publishing.CloseOrdersActivityOptions()

	if opts.ScheduleToCloseTimeout != publishing.CloseOrdersTimeout {
		t.Errorf("schedule-to-close = %v, want %v", opts.ScheduleToCloseTimeout, publishing.CloseOrdersTimeout)
	}
	p := opts.RetryPolicy
	if p == nil {
		t.Fatal("no retry policy, want an explicit one")
	}
	if p.InitialInterval != time.Second || p.MaximumInterval != 5*time.Second {
		t.Errorf("retry intervals = %v..%v, want 1s..5s", p.InitialInterval, p.MaximumInterval)
	}
}
