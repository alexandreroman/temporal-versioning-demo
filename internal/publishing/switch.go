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
	"go.temporal.io/sdk/client"
)

// Switch opens and closes order publishing by starting and terminating the
// OrderPublishing workflow. It caches the last known state so Paused stays a
// cheap read; Refresh updates that cache from Temporal. Errors are returned as
// Temporal reports them: callers add their own context.
type Switch struct {
	c       client.Client
	timeout time.Duration
	logger  *slog.Logger

	// mu is held across each Temporal call and the cache update that follows,
	// so a Refresh whose Describe was in flight during a Pause or Resume cannot
	// land afterwards and overwrite the newer state.
	mu   sync.Mutex
	open atomic.Bool // false until a publishing run is known to be running; read lock-free by Paused
}

// NewSwitch builds a Switch whose publishing runs last timeout. Publishing
// reads as paused until Ensure, Resume or Refresh finds a run in progress.
func NewSwitch(c client.Client, timeout time.Duration, logger *slog.Logger) *Switch {
	return &Switch{c: c, timeout: timeout, logger: logger}
}

// Ensure opens publishing at startup. A publishing run that is still going (the
// backend restarted, or Air hot-reloaded it) is kept as is; otherwise a fresh
// one starts.
func (s *Switch) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(ctx, enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING); err != nil {
		return err
	}
	s.open.Store(true)
	return nil
}

// Resume opens publishing with a full window. A run that is still going is
// terminated and replaced.
func (s *Switch) Resume(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(ctx, enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING); err != nil {
		return err
	}
	s.open.Store(true)
	return nil
}

// Pause closes publishing by terminating the publishing run. A run that has
// already ended is not an error.
//
// Termination rather than cancellation: the server closes the run at once,
// with no worker round-trip, so it also works while the publishing worker is
// down.
func (s *Switch) Pause(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.c.TerminateWorkflow(ctx, WorkflowID, "", "order publishing paused")
	var notFound *serviceerror.NotFound
	if err != nil && !errors.As(err, &notFound) {
		return err
	}
	s.open.Store(false)
	return nil
}

// Refresh reads the publishing run's state from Temporal and caches it for
// Paused. On a lookup failure it keeps the last known state.
func (s *Switch) Refresh(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, err := s.status(ctx)
	if err != nil {
		s.logger.Warn("failed to refresh order publishing state, keeping the last known one", "err", err)
		return
	}
	open := status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	wasOpen := s.open.Swap(open)
	if wasOpen && !open {
		// Only a completed run means the window ran out; anything else (such as
		// a termination from another backend's Pause, or a stuck run timed out
		// by the server) gets the generic line with its status.
		if status == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
			s.logger.Info("order publishing stopped: timeout reached")
		} else {
			s.logger.Info("order publishing stopped", "status", status.String())
		}
	}
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

// executionTimeoutMargin is how long a publishing run may outlive its window
// before the server times it out. Normal expiry happens first, when the
// activity's schedule-to-close fires, so the margin only bites on stuck runs.
const executionTimeoutMargin = time.Minute

// start starts a publishing run. conflict decides what happens when one is
// already running: USE_EXISTING keeps it (no error), TERMINATE_EXISTING
// replaces it.
//
// The execution timeout is a server-side safety net. The worker is unversioned
// and Ensure keeps a running run, so a deploy that changes OrderPublishing can
// leave the kept run stuck on a non-determinism error, Running with no orders.
// The server closes such a run (TimedOut) shortly after its window, whatever
// the worker or code state; the same bounds a run whose window expired while
// the backend was down.
func (s *Switch) start(ctx context.Context, conflict enumspb.WorkflowIdConflictPolicy) error {
	opts := client.StartWorkflowOptions{
		ID:                       WorkflowID,
		TaskQueue:                TaskQueue,
		WorkflowIDConflictPolicy: conflict,
		WorkflowExecutionTimeout: s.timeout + executionTimeoutMargin,
	}
	_, err := s.c.ExecuteWorkflow(ctx, opts, WorkflowTypeName, s.timeout)
	return err
}

// status returns the publishing run's execution status. A run that does not
// exist (never started, or removed after retention) is reported as unspecified,
// which reads as closed like any status other than Running.
func (s *Switch) status(ctx context.Context) (enumspb.WorkflowExecutionStatus, error) {
	resp, err := s.c.DescribeWorkflowExecution(ctx, WorkflowID, "")
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, nil
	}
	if err != nil {
		return enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, err
	}
	return resp.GetWorkflowExecutionInfo().GetStatus(), nil
}
