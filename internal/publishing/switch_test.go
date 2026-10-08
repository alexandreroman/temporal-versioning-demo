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
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// fakeClient records the starts and signals the Switch issues and answers
// describes with a canned run ID, status and stop reason. Embedding the interface
// satisfies client.Client; any other method would panic if called.
type fakeClient struct {
	client.Client

	startErr       error
	signalErr      error
	describeRunID  string
	describeStatus enumspb.WorkflowExecutionStatus
	// describeStopReason, when set, is the publishing-stopped mark in the memo.
	describeStopReason string
	describeErr        error

	starts    []client.StartWorkflowOptions
	workflows []any
	args      [][]any
	signals   [][3]string // workflow ID, run ID, signal name
}

func (c *fakeClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, workflow any,
	args ...any,
) (client.WorkflowRun, error) {
	c.starts = append(c.starts, opts)
	c.workflows = append(c.workflows, workflow)
	c.args = append(c.args, args)
	return nil, c.startErr
}

func (c *fakeClient) SignalWorkflow(_ context.Context, workflowID, runID, signalName string, _ any) error {
	c.signals = append(c.signals, [3]string{workflowID, runID, signalName})
	return c.signalErr
}

func (c *fakeClient) DescribeWorkflowExecution(_ context.Context, _, _ string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if c.describeErr != nil {
		return nil, c.describeErr
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{WorkflowId: publishing.WorkflowID, RunId: c.describeRunID},
			Status:    c.describeStatus,
			Memo:      stoppedMemo(c.describeStopReason),
		},
	}, nil
}

// newRunningClient is a fakeClient whose publishing run is Running and still
// publishing, as right after a start.
func newRunningClient() *fakeClient {
	return &fakeClient{describeRunID: "run-1", describeStatus: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
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
			c := newRunningClient()
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
			// A pause can come at the very end of the window; the run then
			// waits and closes the orders left open.
			wantExecTimeout := testTimeout + publishing.CloseDelay + publishing.CloseOrdersTimeout +
				publishing.ExecutionTimeoutMargin
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
	pauseRun1 := [][3]string{{publishing.WorkflowID, "run-1", publishing.PauseSignalName}}
	tests := []struct {
		name        string
		describeErr error
		signalErr   error
		wantErr     error
		wantSignals [][3]string
		wantPaused  bool
	}{
		{"signals the publishing run", nil, nil, nil, pauseRun1, true},
		{"ignores an ended run", nil, serviceerror.NewNotFound("workflow not found"), nil, pauseRun1, true},
		{"returns other signal errors", nil, unavailable, unavailable, pauseRun1, false},
		{"has nothing to signal without a run", serviceerror.NewNotFound("workflow not found"), nil, nil, nil, true},
		{"returns describe errors", unavailable, nil, unavailable, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRunningClient()
			s := publishing.NewSwitch(c, testTimeout, discardLogger())
			if err := s.Ensure(t.Context()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			c.describeErr = tt.describeErr
			c.signalErr = tt.signalErr

			err := s.Pause(t.Context())

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(c.signals, tt.wantSignals) {
				t.Errorf("signals %v, want %v", c.signals, tt.wantSignals)
			}
			if got := s.Paused(); got != tt.wantPaused {
				t.Errorf("Paused() = %v, want %v", got, tt.wantPaused)
			}
		})
	}
}

// TestSwitchRefreshAfterPause checks that a Refresh landing before the paused
// run has processed the signal (Running, no stop mark yet) does not read
// publishing as open again, while a newer run still does.
func TestSwitchRefreshAfterPause(t *testing.T) {
	tests := []struct {
		name string
		// resume, when set, resumes publishing between the pause and the refresh.
		resume   bool
		runID    string
		wantOpen bool
	}{
		{"paused run without its mark yet stays paused", false, "run-1", false},
		{"newer run opens", false, "run-2", true},
		{"resumed run opens", true, "run-1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRunningClient()
			s := publishing.NewSwitch(c, testTimeout, discardLogger())
			if err := s.Ensure(t.Context()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if err := s.Pause(t.Context()); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			if tt.resume {
				if err := s.Resume(t.Context()); err != nil {
					t.Fatalf("Resume: %v", err)
				}
			}
			c.describeRunID = tt.runID

			s.Refresh(t.Context())

			if got := s.Paused(); got != !tt.wantOpen {
				t.Errorf("Paused() = %v, want %v", got, !tt.wantOpen)
			}
		})
	}
}

func TestSwitchRefresh(t *testing.T) {
	running := enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	tests := []struct {
		name        string
		startOpen   bool
		status      enumspb.WorkflowExecutionStatus
		stopReason  string
		describeErr error
		wantOpen    bool
	}{
		{"running opens", false, running, "", nil, true},
		{"running after a timeout closes", true, running, "timeout", nil, false},
		{"running after a pause closes", true, running, "paused", nil, false},
		{"running after a pause stays closed", false, running, "paused", nil, false},
		{"completed closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "timeout", nil, false},
		{"canceled closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, "", nil, false},
		{"timed out closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, "", nil, false},
		{"terminated closes", true, enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, "", nil, false},
		{"not found closes", true, 0, "", serviceerror.NewNotFound("workflow not found"), false},
		{"other error keeps open", true, 0, "", serviceerror.NewUnavailable("temporal down"), true},
		{"other error keeps closed", false, 0, "", serviceerror.NewUnavailable("temporal down"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRunningClient()
			s := publishing.NewSwitch(c, testTimeout, discardLogger())
			if tt.startOpen {
				if err := s.Ensure(t.Context()); err != nil {
					t.Fatalf("Ensure: %v", err)
				}
			}
			c.describeStatus = tt.status
			c.describeStopReason = tt.stopReason
			c.describeErr = tt.describeErr

			s.Refresh(t.Context())

			if got := s.Paused(); got != !tt.wantOpen {
				t.Errorf("Paused() = %v, want %v", got, !tt.wantOpen)
			}
		})
	}
}

// TestSwitchEnsureKeepsStoppedRun checks that Ensure reads publishing as
// paused when the run it keeps has already stopped publishing and is only
// waiting to close the orders left open.
func TestSwitchEnsureKeepsStoppedRun(t *testing.T) {
	c := newRunningClient()
	c.describeStopReason = "paused"
	s := publishing.NewSwitch(c, testTimeout, discardLogger())

	if err := s.Ensure(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(c.starts) != 1 {
		t.Fatalf("started %d workflows, want 1", len(c.starts))
	}
	if !s.Paused() {
		t.Error("Paused() = false with a run that stopped publishing, want true")
	}
}

func TestSwitchEnsureDescribeFailureKeepsPaused(t *testing.T) {
	describeErr := serviceerror.NewUnavailable("temporal down")
	c := newRunningClient()
	c.describeErr = describeErr
	s := publishing.NewSwitch(c, testTimeout, discardLogger())

	if err := s.Ensure(t.Context()); !errors.Is(err, describeErr) {
		t.Fatalf("err = %v, want %v", err, describeErr)
	}
	if !s.Paused() {
		t.Error("Paused() = false after a failed describe, want true")
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
		c := newRunningClient()
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
