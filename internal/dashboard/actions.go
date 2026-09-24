package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ErrUnknownVersion is returned when the UI asks to act on a version label that
// is not registered in the deployment.
var ErrUnknownVersion = errors.New("dashboard: unknown version")

// bootstrapTimeout bounds the EnsureCurrentVersion startup loop: if no version is
// registered within this window we give up and leave promotion to the operator.
const bootstrapTimeout = 2 * time.Minute

// Actions turns operator intents into Temporal API calls.
type Actions struct {
	c              client.Client
	deploymentName string
	namespace      string
	logger         *slog.Logger
	labels         *LabelResolver
}

// handle returns a handle to this Actions' worker deployment.
func (a *Actions) handle() client.WorkerDeploymentHandle {
	return a.c.WorkerDeploymentClient().GetHandle(a.deploymentName)
}

// describe wraps the worker-deployment Describe call with a consistent error.
func (a *Actions) describe(ctx context.Context) (client.WorkerDeploymentDescribeResponse, error) {
	resp, err := a.handle().Describe(ctx, client.WorkerDeploymentDescribeOptions{})
	if err != nil {
		return client.WorkerDeploymentDescribeResponse{}, fmt.Errorf("describe worker deployment %q: %w", a.deploymentName, err)
	}
	return resp, nil
}

// setCurrent makes buildID the Current version. The lenient options are required
// because a freshly-shipped version's pollers may not be registered yet, and its
// task queues may differ from any prior version.
func (a *Actions) setCurrent(ctx context.Context, buildID string) error {
	if _, err := a.handle().SetCurrentVersion(ctx, client.WorkerDeploymentSetCurrentVersionOptions{
		BuildID:                 buildID,
		AllowNoPollers:          true,
		IgnoreMissingTaskQueues: true,
	}); err != nil {
		return fmt.Errorf("set current version %q: %w", buildID, err)
	}
	return nil
}

// NewActions builds an Actions over the given client and deployment. The label
// resolver is shared with the reader so the buildID→label cache is not duplicated.
func NewActions(c client.Client, deploymentName, namespace string, labels *LabelResolver, logger *slog.Logger) *Actions {
	return &Actions{
		c:              c,
		deploymentName: deploymentName,
		namespace:      namespace,
		logger:         logger,
		labels:         labels,
	}
}

// Ramp routes pct% of new orders to the named version (v1/v2/v3).
func (a *Actions) Ramp(ctx context.Context, version string, pct float32) error {
	labelToBuild, err := a.versionIndex(ctx)
	if err != nil {
		return err
	}
	buildID, ok := labelToBuild[version]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownVersion, version)
	}
	if _, err := a.handle().SetRampingVersion(ctx, client.WorkerDeploymentSetRampingVersionOptions{
		BuildID:                 buildID,
		Percentage:              pct,
		AllowNoPollers:          true,
		IgnoreMissingTaskQueues: true,
	}); err != nil {
		return fmt.Errorf("set ramping version %q to %.0f%%: %w", buildID, pct, err)
	}
	return nil
}

// Promote makes the named version Current (full cutover).
func (a *Actions) Promote(ctx context.Context, version string) error {
	labelToBuild, err := a.versionIndex(ctx)
	if err != nil {
		return err
	}
	buildID, ok := labelToBuild[version]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownVersion, version)
	}
	return a.setCurrent(ctx, buildID)
}

// versionIndex resolves the live label↔Build ID mapping (metadata-first, with a
// CreateTime fallback).
func (a *Actions) versionIndex(ctx context.Context) (map[string]string, error) {
	resp, err := a.describe(ctx)
	if err != nil {
		return nil, err
	}

	buildToLabel := a.metadataLabelMap(ctx, resp.Info.VersionSummaries)

	sorted := slices.Clone(resp.Info.VersionSummaries)
	slices.SortFunc(sorted, func(a, b client.WorkerDeploymentVersionSummary) int {
		return a.CreateTime.Compare(b.CreateTime)
	})

	labelToBuild := map[string]string{}
	for i, s := range sorted {
		buildID := s.Version.BuildID
		labelVal := buildToLabel[buildID]
		if labelVal == "" {
			labelVal = friendly(i) // CreateTime fallback (oldest = v1)
		}
		labelToBuild[labelVal] = buildID
	}
	return labelToBuild, nil
}

// metadataLabels resolves a buildID→label map using version metadata only (no
// CreateTime fallback) plus the current Build ID. Bootstrap uses this so it waits
// for a worker to actually publish pizzaVersion=v1 instead of promoting an
// arbitrary (oldest-registered) build when all versions start together.
func (a *Actions) metadataLabels(ctx context.Context) (map[string]string, string, error) {
	resp, err := a.describe(ctx)
	if err != nil {
		return nil, "", err
	}
	current := currentBuildID(resp.Info.RoutingConfig)
	return a.metadataLabelMap(ctx, resp.Info.VersionSummaries), current, nil
}

// metadataLabelMap resolves each summary's metadata label (cache-or-fetch via the
// label resolver) and returns a buildID→label map, omitting builds with no published
// label. It is the shared loop behind versionIndex and metadataLabels; only the
// caller decides what to do with unlabelled builds (CreateTime fallback vs. omit).
func (a *Actions) metadataLabelMap(
	ctx context.Context,
	summaries []client.WorkerDeploymentVersionSummary,
) map[string]string {
	buildToLabel := map[string]string{}
	for _, s := range summaries {
		buildID := s.Version.BuildID
		if label := a.labels.label(ctx, buildID); label != "" {
			buildToLabel[buildID] = label
		}
	}
	return buildToLabel
}

// bootstrapBuildID returns the Build ID labelled v1 from a buildID→label map,
// or "" if no version carries the v1 label yet.
func bootstrapBuildID(buildToLabel map[string]string) string {
	for buildID, label := range buildToLabel {
		if label == "v1" {
			return buildID
		}
	}
	return ""
}

// bootstrapDecision is the outcome of evaluating whether to auto-promote a first
// Current version at startup.
type bootstrapDecision int

const (
	// bootstrapSkip means a Current version is already set; never re-promote.
	bootstrapSkip bootstrapDecision = iota
	// bootstrapPromote means no Current version is set and a target is ready.
	bootstrapPromote
	// bootstrapWait means nothing is ready yet; retry later.
	bootstrapWait
)

// decideBootstrap maps a resolved (target, current) pair to a startup action. It is
// pure (no I/O) so the bootstrap rules can be unit-tested in isolation.
//
// Rules:
//   - a Current version already exists -> skip (keeps the manual ramp/promote/
//     rollback flow untouched, even if a newer target also exists);
//   - no Current and a target is ready -> promote that target;
//   - otherwise (empty target) -> wait. A describe failure routes here too,
//     because the caller reports an empty target on failure.
func decideBootstrap(target, current string) (bootstrapDecision, string) {
	if current != "" {
		return bootstrapSkip, ""
	}
	if target != "" {
		return bootstrapPromote, target
	}
	return bootstrapWait, ""
}

// EnsureCurrentVersion makes sure the deployment has a Current version at startup
// by promoting the version a worker has labelled v1 in its metadata, once that
// label is published (it waits — no CreateTime fallback here — so concurrently
// started v1/v2/v3 workers cannot cause an arbitrary version to be promoted).
//
// It runs everywhere (local and K8s) with no feature flag. Because it fires only
// while Current is nil, it acts once at bootstrap and then leaves later
// ramp/promote/rollback entirely to the operator. The loop is bounded by
// bootstrapTimeout; on timeout it logs and returns, so the operator can still
// promote manually. It always honors ctx cancellation.
func (a *Actions) EnsureCurrentVersion(ctx context.Context, pollInterval time.Duration) {
	deadline := time.NewTimer(bootstrapTimeout)
	defer deadline.Stop()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		buildToLabel, current, err := a.metadataLabels(ctx)
		target := bootstrapBuildID(buildToLabel)
		switch decision, buildID := decideBootstrap(target, current); decision {
		case bootstrapSkip:
			a.logger.Info("current version already set, skipping bootstrap", "currentBuild", current)
			return
		case bootstrapPromote:
			if err := a.setCurrent(ctx, buildID); err != nil {
				// Treat as transient: keep retrying until the deadline.
				a.logger.Warn("bootstrap promote failed, will retry", "build", buildID, "err", err)
				break
			}
			a.logger.Info("bootstrapped current version", "build", buildID)
			return
		case bootstrapWait:
			// Nothing registered yet; fall through to wait for the next tick. A
			// non-nil err here means describe is failing (e.g. Temporal unreachable);
			// log it so the cause is diagnosable before the deadline fires. Stay
			// silent on the normal "no version yet" case (err nil) to avoid noise.
			if err != nil {
				a.logger.Debug("bootstrap waiting on describe failure, will retry", "err", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			a.logger.Warn("no version registered in time for bootstrap; operator can still promote manually",
				"timeout", bootstrapTimeout)
			return
		case <-ticker.C:
		}
	}
}

// Rollback removes the ramp: 100% of new orders snap back to Current.
func (a *Actions) Rollback(ctx context.Context) error {
	// Percentage:0 is what clears the ramping version: an empty BuildID alone
	// maps to "unversioned" rather than "no ramp". Clearing the ramp is safe here
	// only because Current is non-nil, so new orders snap back to Current.
	if _, err := a.handle().SetRampingVersion(ctx, client.WorkerDeploymentSetRampingVersionOptions{
		BuildID:    "",
		Percentage: 0,
	}); err != nil {
		return fmt.Errorf("remove ramping version: %w", err)
	}
	return nil
}

// RecoverOne resets a single in-error order onto the Current build (reset-with-move,
// rewinding to the workflow start). It is the per-card recover action; the caller
// (the UI) only offers it on a failing order.
func (a *Actions) RecoverOne(ctx context.Context, workflowID string) error {
	good, err := a.currentBuild(ctx)
	if err != nil {
		return err
	}
	if good == "" {
		return errors.New("dashboard: no current version to recover onto")
	}
	desc, err := a.c.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return fmt.Errorf("describe workflow %q: %w", workflowID, err)
	}
	exec := desc.GetWorkflowExecutionInfo().GetExecution()
	if err := a.resetWithMove(ctx, exec, good); err != nil {
		return fmt.Errorf("recover %q: %w", workflowID, err)
	}
	a.logger.Info("recovered order", "workflowId", workflowID, "goodBuild", good)
	return nil
}

// currentBuild returns the deployment's Current build ID from the live routing
// config, or "" if no Current version is set.
func (a *Actions) currentBuild(ctx context.Context) (string, error) {
	resp, err := a.describe(ctx)
	if err != nil {
		return "", err
	}
	return currentBuildID(resp.Info.RoutingConfig), nil
}

// resetWithMove resets the listed run to its first completed Workflow Task and
// pins the resulting new run to the good (current) build via a PostResetOperation.
// Assumes these are pinned, non-continue-as-new workflows: the first completed
// Workflow Task is a valid reset point and the run carries a single build ID.
func (a *Actions) resetWithMove(ctx context.Context, exec *commonpb.WorkflowExecution, goodBuild string) error {
	resetPoint, err := a.firstWorkflowTaskCompletedID(ctx, exec)
	if err != nil {
		return err
	}

	req := &workflowservice.ResetWorkflowExecutionRequest{
		Namespace:                 a.namespace,
		WorkflowExecution:         exec,
		Reason:                    "recover stuck orders: bad drone-delivery build",
		RequestId:                 uuid.NewString(),
		WorkflowTaskFinishEventId: resetPoint,
		// Reapply signals only: exclude every other reapplyable event type. CANCEL_REQUEST is left out because
		// it is deprecated and unimplemented.
		ResetReapplyExcludeTypes: []enumspb.ResetReapplyExcludeType{
			enumspb.RESET_REAPPLY_EXCLUDE_TYPE_UPDATE,
			enumspb.RESET_REAPPLY_EXCLUDE_TYPE_NEXUS,
		},
		PostResetOperations: []*workflowpb.PostResetOperation{{
			Variant: &workflowpb.PostResetOperation_UpdateWorkflowOptions_{
				UpdateWorkflowOptions: &workflowpb.PostResetOperation_UpdateWorkflowOptions{
					WorkflowExecutionOptions: &workflowpb.WorkflowExecutionOptions{
						VersioningOverride: &workflowpb.VersioningOverride{
							Override: &workflowpb.VersioningOverride_Pinned{
								Pinned: &workflowpb.VersioningOverride_PinnedOverride{
									Behavior: workflowpb.VersioningOverride_PINNED_OVERRIDE_BEHAVIOR_PINNED,
									Version: &deploymentpb.WorkerDeploymentVersion{
										DeploymentName: a.deploymentName,
										BuildId:        goodBuild,
									},
								},
							},
						},
					},
					UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"versioning_override"}},
				},
			},
		}},
	}
	if _, err := a.c.ResetWorkflowExecution(ctx, req); err != nil {
		return fmt.Errorf("reset workflow: %w", err)
	}
	return nil
}

// firstWorkflowTaskCompletedID scans history for the first WorkflowTaskCompleted
// event, the canonical reset point for a reset-with-move. Assumes a pinned,
// non-continue-as-new workflow, so the first such event reliably exists in this
// run's history.
func (a *Actions) firstWorkflowTaskCompletedID(ctx context.Context, exec *commonpb.WorkflowExecution) (int64, error) {
	iter := a.c.GetWorkflowHistory(ctx, exec.GetWorkflowId(), exec.GetRunId(), false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			return 0, fmt.Errorf("read history: %w", err)
		}
		if ev.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED {
			return ev.GetEventId(), nil
		}
	}
	return 0, fmt.Errorf("no WorkflowTaskCompleted event for %s", exec.GetWorkflowId())
}
