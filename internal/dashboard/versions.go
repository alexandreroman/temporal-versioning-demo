// Helpers for Worker Deployment Version metadata.
package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// metaKeyPizzaVersion is the version-metadata key each worker upserts at startup
// to publish its friendly shape label (v1/v2/v3) for its Build ID.
const metaKeyPizzaVersion = "pizzaVersion"

// decodeVersionLabel extracts the friendly label from a version's metadata map.
// Returns "" when the key is absent or cannot be decoded (caller falls back to
// CreateTime ordering).
func decodeVersionLabel(metadata map[string]*commonpb.Payload) string {
	p, ok := metadata[metaKeyPizzaVersion]
	if !ok {
		return ""
	}
	var label string
	if err := converter.GetDefaultDataConverter().FromPayload(p, &label); err != nil {
		return ""
	}
	return label
}

// fetchVersionLabel describes one version and decodes its pizzaVersion label.
// Returns "" (no error) when the version has no such metadata yet.
func fetchVersionLabel(ctx context.Context, c client.Client, deploymentName, buildID string) (string, error) {
	h := c.WorkerDeploymentClient().GetHandle(deploymentName)
	desc, err := h.DescribeVersion(ctx, client.WorkerDeploymentDescribeVersionOptions{BuildID: buildID})
	if err != nil {
		return "", fmt.Errorf("describe version %q: %w", buildID, err)
	}
	return decodeVersionLabel(desc.Info.Metadata), nil
}

// currentBuildID returns the Current version's Build ID from a routing config, or
// "" when no Current version is set. It centralizes the CurrentVersion nil-guard
// shared by the reader and the actions.
func currentBuildID(rc client.WorkerDeploymentRoutingConfig) string {
	if rc.CurrentVersion != nil {
		return rc.CurrentVersion.BuildID
	}
	return ""
}

// LabelResolver caches each build's friendly pizzaVersion label, fetching it from
// version metadata on first use. Safe for concurrent use. It is shared between the
// reader and the actions so the buildID→label cache lives in one place.
type LabelResolver struct {
	c              client.Client
	deploymentName string
	logger         *slog.Logger
	mu             sync.Mutex
	cache          map[string]string // buildID -> label; only non-empty labels are cached
}

// NewLabelResolver builds a LabelResolver over the given client and deployment.
func NewLabelResolver(c client.Client, deploymentName string, logger *slog.Logger) *LabelResolver {
	return &LabelResolver{c: c, deploymentName: deploymentName, logger: logger, cache: map[string]string{}}
}

// label returns the build's friendly metadata label, or "" when none is published
// yet. Resolved labels are cached; "" is not, so a build is re-fetched until its
// worker publishes the metadata. The caller decides any CreateTime fallback.
func (r *LabelResolver) label(ctx context.Context, buildID string) string {
	r.mu.Lock()
	if v, ok := r.cache[buildID]; ok {
		r.mu.Unlock()
		return v
	}
	r.mu.Unlock()

	v, err := fetchVersionLabel(ctx, r.c, r.deploymentName, buildID)
	if err != nil {
		r.logger.Debug("version label fetch failed, using CreateTime fallback", "buildId", buildID, "err", err)
		return ""
	}
	if v == "" {
		return ""
	}
	r.mu.Lock()
	r.cache[buildID] = v
	r.mu.Unlock()
	return v
}
