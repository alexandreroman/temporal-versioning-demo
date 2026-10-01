package publishing_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/publishing"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// fakeClient records the starts and terminations the Switch issues and answers
// describes with a canned status. Embedding the interface satisfies
// client.Client; any other method would panic if called.
type fakeClient struct {
	client.Client

	startErr       error
	terminateErr   error
	describeStatus enumspb.WorkflowExecutionStatus
	describeErr    error

	starts     []client.StartWorkflowOptions
	workflows  []any
	args       [][]any
	terminated []string
}

func (c *fakeClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, workflow any,
	args ...any,
) (client.WorkflowRun, error) {
	c.starts = append(c.starts, opts)
	c.workflows = append(c.workflows, workflow)
	c.args = append(c.args, args)
	return nil, c.startErr
}

func (c *fakeClient) TerminateWorkflow(_ context.Context, workflowID, _, _ string, _ ...any) error {
	c.terminated = append(c.terminated, workflowID)
	return c.terminateErr
}

func (c *fakeClient) DescribeWorkflowExecution(_ context.Context, _, _ string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if c.describeErr != nil {
		return nil, c.describeErr
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: c.describeStatus},
	}, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSwitchStartPolicies(t *testing.T) {
	tests := []struct {
		name         string
		start        func(*publishing.Switch, context.Context) error
		wantConflict enumspb.WorkflowIdConflictPolicy
	}{
		// Ensure keeps a running countdown; Resume always restarts it in full.
		{"Ensure", (*publishing.Switch).Ensure, enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING},
		{"Resume", (*publishing.Switch).Resume, enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{}
			s := publishing.NewSwitch(c, testTimeout, discardLogger())

			if err := tt.start(s, t.Context()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(c.starts) != 1 {
				t.Fatalf("started %d workflows, want 1", len(c.starts))
			}
			opts := c.starts[0]
			if opts.ID != publishing.WorkflowID || opts.TaskQueue != publishing.TaskQueue {
				t.Errorf("ID/TaskQueue = %q/%q, want %q/%q",
					opts.ID, opts.TaskQueue, publishing.WorkflowID, publishing.TaskQueue)
			}
			if opts.WorkflowIDConflictPolicy != tt.wantConflict {
				t.Errorf("conflict policy = %v, want %v", opts.WorkflowIDConflictPolicy, tt.wantConflict)
			}
			wantExecTimeout := testTimeout + publishing.ExecutionTimeoutMargin
			if opts.WorkflowExecutionTimeout != wantExecTimeout {
				t.Errorf("execution timeout = %v, want %v", opts.WorkflowExecutionTimeout, wantExecTimeout)
			}
			if c.workflows[0] != publishing.WorkflowTypeName {
				t.Errorf("workflow = %v, want %q", c.workflows[0], publishing.WorkflowTypeName)
			}
			if !slices.Equal(c.args[0], []any{testTimeout}) {
				t.Errorf("args = %v, want [%v]", c.args[0], testTimeout)
			}
			if s.Paused() {
				t.Error("Paused() = true after a successful start, want false")
			}
		})
	}
}

func TestSwitchStartFailureKeepsPaused(t *testing.T) {
	tests := []struct {
		name  string
		start func(*publishing.Switch, context.Context) error
	}{
		{"Ensure", (*publishing.Switch).Ensure},
		{"Resume", (*publishing.Switch).Resume},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			startErr := serviceerror.NewUnavailable("temporal down")
			s := publishing.NewSwitch(&fakeClient{startErr: startErr}, testTimeout, discardLogger())

			if err := tt.start(s, t.Context()); !errors.Is(err, startErr) {
				t.Fatalf("err = %v, want %v", err, startErr)
			}
			if !s.Paused() {
				t.Error("Paused() = false after a failed start, want true")
			}
		})
	}
}

func TestSwitchPause(t *testing.T) {
	unavailable := serviceerror.NewUnavailable("temporal down")
	tests := []struct {
		name         string
		terminateErr error
		wantErr      error
		wantPaused   bool
	}{
		{"terminates the countdown", nil, nil, true},
		{"ignores an ended countdown", serviceerror.NewNotFound("workflow not found"), nil, true},
		{"returns other errors", unavailable, unavailable, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{}
			s := publishing.NewSwitch(c, testTimeout, discardLogger())
			if err := s.Ensure(t.Context()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			c.terminateErr = tt.terminateErr

			err := s.Pause(t.Context())

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(c.terminated, []string{publishing.WorkflowID}) {
				t.Errorf("terminated %v, want [%s]", c.terminated, publishing.WorkflowID)
			}
			if got := s.Paused(); got != tt.wantPaused {
				t.Errorf("Paused() = %v, want %v", got, tt.wantPaused)
			}
		})
	}
}

func TestSwitchRefresh(t *testing.T) {
	tests := []struct {
		name        string
		startOpen   bool
		status      enumspb.WorkflowExecutionStatus
		describeErr error
		wantOpen    bool
	}{
		{"running opens", false, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil, true},
		{"completed closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, nil, false},
		{"canceled closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, nil, false},
		{"timed out closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, nil, false},
		{"terminated closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, nil, false},
		{"not found closes", true, 0, serviceerror.NewNotFound("workflow not found"), false},
		{"other error keeps open", true, 0, serviceerror.NewUnavailable("temporal down"), true},
		{"other error keeps closed", false, 0, serviceerror.NewUnavailable("temporal down"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{}
			s := publishing.NewSwitch(c, testTimeout, discardLogger())
			if tt.startOpen {
				if err := s.Ensure(t.Context()); err != nil {
					t.Fatalf("Ensure: %v", err)
				}
			}
			c.describeStatus = tt.status
			c.describeErr = tt.describeErr

			s.Refresh(t.Context())

			if got := s.Paused(); got != !tt.wantOpen {
				t.Errorf("Paused() = %v, want %v", got, !tt.wantOpen)
			}
		})
	}
}

func TestSwitchStartsPaused(t *testing.T) {
	s := publishing.NewSwitch(&fakeClient{}, testTimeout, discardLogger())
	if !s.Paused() {
		t.Error("Paused() = false before any countdown is known, want true")
	}
}

// TestSwitchRunRefreshes checks that Run picks up a run that ended on its own
// and returns once its context is done (synctest fails the test otherwise).
func TestSwitchRunRefreshes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Second
		c := &fakeClient{}
		s := publishing.NewSwitch(c, testTimeout, discardLogger())
		if err := s.Ensure(t.Context()); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		c.describeStatus = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED

		ctx, cancel := context.WithCancel(t.Context())
		go s.Run(ctx, interval)

		synctest.Wait()
		if s.Paused() {
			t.Fatal("Paused() = true before the first refresh, want false")
		}
		time.Sleep(interval)
		synctest.Wait()
		if !s.Paused() {
			t.Error("Paused() = false after a refresh saw the run completed, want true")
		}
		cancel()
	})
}
