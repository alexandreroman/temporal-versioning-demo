package publishing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/client"
)

// Switch opens order publishing by starting the OrderPublishing workflow and
// closes it by signalling that workflow to pause. It caches the last known
// state so Paused stays a cheap read; Refresh updates that cache from Temporal.
// Errors are returned as Temporal reports them: callers add their own context.
type Switch struct {
	c       client.Client
	timeout time.Duration
	logger  *slog.Logger

	// mu is held across each Temporal call and the cache update that follows,
	// so a Refresh whose Describe was in flight during a Pause or Resume cannot
	// land afterwards and overwrite the newer state.
	mu   sync.Mutex
	open atomic.Bool // false until a publishing run is known to be running; read lock-free by Paused
	// pausedRunID is the run Pause last signalled, guarded by mu. That run
	// reads as paused even before it processes the signal and marks itself.
	pausedRunID string
}

// NewSwitch builds a Switch whose publishing runs last timeout. Publishing
// reads as paused until Ensure, Resume or Refresh finds a run in progress.
func NewSwitch(c client.Client, timeout time.Duration, logger *slog.Logger) *Switch {
	return &Switch{c: c, timeout: timeout, logger: logger}
}

// Ensure opens publishing at startup. A publishing run that is still going (the
// backend restarted, or Air hot-reloaded it) is kept as is; otherwise a fresh
// one starts.
//
// The kept run may have stopped publishing already and be waiting to close the
// orders left open, so the cached state comes from a Describe rather than from
// the start: publishing then reads as paused. A run started just now has no
// stop mark yet, so it reads as open. Should that Describe fail, publishing
// reads as paused until the next Refresh.
func (s *Switch) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(ctx, enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING); err != nil {
		return err
	}
	info, err := s.describe(ctx)
	if err != nil {
		return err
	}
	s.pausedRunID = ""
	s.open.Store(publishingOpen(info))
	return nil
}

// Resume opens publishing with a full window. A run that is still going is
// terminated and replaced; a run waiting to close the orders left open ends
// with it, so those orders are kept.
func (s *Switch) Resume(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(ctx, enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING); err != nil {
		return err
	}
	s.pausedRunID = ""
	s.open.Store(true)
	return nil
}

// Pause closes publishing by signalling the publishing run, which stops
// publishing and, closeDelay later, closes the orders left open. A run that has
// already ended is not an error.
//
// A signal rather than a termination: the run must outlive the pause to close
// the stuck orders later, and a Resume within that delay must be able to call
// it off by terminating the run.
//
// The run is described first and signalled by its run ID, so Pause knows which
// run it paused: until that run processes the signal, it is Running without a
// stop mark, and Refresh must not read it as open again.
func (s *Switch) Pause(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.describe(ctx)
	if err != nil {
		return err
	}
	if info != nil {
		runID := info.GetExecution().GetRunId()
		err := s.c.SignalWorkflow(ctx, WorkflowID, runID, PauseSignalName, nil)
		var notFound *serviceerror.NotFound
		if err != nil && !errors.As(err, &notFound) {
			return err
		}
		s.pausedRunID = runID
	}
	s.open.Store(false)
	return nil
}

// Refresh reads the publishing run's state from Temporal and caches it for
// Paused. On a lookup failure it keeps the last known state.
func (s *Switch) Refresh(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.describe(ctx)
	if err != nil {
		s.logger.Warn("failed to refresh order publishing state, keeping the last known one", "err", err)
		return
	}
	open := publishingOpen(info)
	if open && s.pausedRunID != "" && info.GetExecution().GetRunId() == s.pausedRunID {
		// The run this Switch paused has not processed the signal yet.
		open = false
	}
	wasOpen := s.open.Swap(open)
	if wasOpen && !open {
		s.logStopped(info)
	}
}

// logStopped logs why the publishing run described by info stopped publishing.
// Only the window running out gets the timeout line: the run's stop mark says
// so, or, without a mark, the run completed. Anything else (such as another
// backend's Pause, or a stuck run timed out by the server) gets the generic
// line with its status and reason.
func (s *Switch) logStopped(info *workflowpb.WorkflowExecutionInfo) {
	reason, stopped := stopReason(info)
	status := info.GetStatus()
	if reason == stopReasonTimeout || (!stopped && status == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED) {
		s.logger.Info("order publishing stopped: timeout reached")
		return
	}
	s.logger.Info("order publishing stopped", "status", status.String(), "reason", reason)
}

// Run refreshes the cached state every interval until ctx is done, so Paused
// follows a publishing run that ends on its own.
func (s *Switch) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshWithin(ctx, interval)
		}
	}
}

// refreshWithin runs one Refresh bounded by timeout, so a slow Temporal holds
// the mutex (and blocks Pause/Resume) for at most one interval rather than the
// SDK's default RPC timeout.
func (s *Switch) refreshWithin(ctx context.Context, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.Refresh(ctx)
}

// Paused reports whether publishing is closed, from the cached state only.
func (s *Switch) Paused() bool { return !s.open.Load() }

// executionTimeoutMargin is how long a publishing run may outlive its window and
// closing phase before the server times it out. A run normally completes first,
// once CloseOrders is done, so the margin only bites on stuck runs.
const executionTimeoutMargin = time.Minute

// start starts a publishing run. conflict decides what happens when one is
// already running: USE_EXISTING keeps it (no error), TERMINATE_EXISTING
// replaces it.
//
// The execution timeout is a server-side safety net. The worker is unversioned
// and Ensure keeps a running run, so a deploy that changes OrderPublishing can
// leave the kept run stuck on a non-determinism error, Running with no orders.
// The server closes such a run (TimedOut) shortly after its window and closing
// phase, whatever the worker or code state; the same bounds a run whose window
// expired while the backend was down. The closing phase counts in full because
// a pause can come at any point of the window, even at its very end.
func (s *Switch) start(ctx context.Context, conflict enumspb.WorkflowIdConflictPolicy) error {
	opts := client.StartWorkflowOptions{
		ID:                       WorkflowID,
		TaskQueue:                TaskQueue,
		WorkflowIDConflictPolicy: conflict,
		WorkflowExecutionTimeout: s.timeout + closeDelay + closeOrdersTimeout + executionTimeoutMargin,
	}
	_, err := s.c.ExecuteWorkflow(ctx, opts, WorkflowTypeName, s.timeout)
	return err
}

// describe returns the latest publishing run's execution info. A run that does
// not exist (never started, or removed after retention) is reported as nil,
// which reads as closed.
func (s *Switch) describe(ctx context.Context) (*workflowpb.WorkflowExecutionInfo, error) {
	resp, err := s.c.DescribeWorkflowExecution(ctx, WorkflowID, "")
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return resp.GetWorkflowExecutionInfo(), nil
}

// publishingOpen reports whether the run described by info publishes orders:
// it is Running and has not marked publishing as stopped. A Running run with
// that mark is only waiting to close the orders left open.
func publishingOpen(info *workflowpb.WorkflowExecutionInfo) bool {
	_, stopped := stopReason(info)
	return info.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING && !stopped
}
