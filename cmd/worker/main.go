// Command worker runs the versioned Temporal worker for the Pizza Tracker demo.
//
// The worker is registered with Temporal as part of a Worker Deployment so the
// Temporal Worker Controller can manage its version and route traffic to it.
// The deployment name and build ID are injected by the controller through
// environment variables. The PIZZA_VERSION env var selects which workflow shape
// (v1/v2/v3) this pod registers under the shared "PizzaOrder" type; all shapes
// are Pinned so in-flight orders never switch versions mid-flight.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("pizza worker failed", "err", err)
		os.Exit(1)
	}
}

// run wires and runs the versioned worker until the process is signalled. It owns
// all deferred cleanup so main can exit non-zero without skipping it.
func run(logger *slog.Logger) error {
	temporalAddress := getenv("TEMPORAL_ADDRESS", "127.0.0.1:7233")
	namespace := getenv("TEMPORAL_NAMESPACE", "default")
	deploymentName := os.Getenv("TEMPORAL_DEPLOYMENT_NAME")
	buildID := os.Getenv("TEMPORAL_WORKER_BUILD_ID")
	taskQueue := getenv("PIZZA_TASK_QUEUE", pizza.TaskQueue)
	version := getenv("PIZZA_VERSION", string(pizza.V1))

	v, ok := pizza.ParseVersion(version)
	if !ok {
		return fmt.Errorf("invalid PIZZA_VERSION %q", version)
	}
	if deploymentName == "" || buildID == "" {
		return fmt.Errorf("missing controller-injected env vars: TEMPORAL_DEPLOYMENT_NAME=%q TEMPORAL_WORKER_BUILD_ID=%q",
			deploymentName, buildID)
	}

	logger.Info("starting pizza worker",
		"temporalAddress", temporalAddress, "namespace", namespace,
		"deploymentName", deploymentName, "buildID", buildID,
		"taskQueue", taskQueue, "pizzaVersion", version)

	c, err := client.Dial(client.Options{HostPort: temporalAddress, Namespace: namespace})
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{
		DeploymentOptions: worker.DeploymentOptions{
			UseVersioning: true,
			Version: worker.WorkerDeploymentVersion{
				DeploymentName: deploymentName,
				BuildID:        buildID,
			},
			DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
		},
	})
	switch v {
	case pizza.V1:
		pizza.RegisterV1(w)
	case pizza.V2:
		pizza.RegisterV2(w)
	case pizza.V3:
		pizza.RegisterV3(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	// Publish this worker's friendly shape label (v1/v2/v3) as Worker Deployment
	// Version metadata so the dashboard can label/route by it (CreateTime ordering
	// is racy when all versions start together). Best-effort: a freshly started
	// version may not be registered until its first poll, so retry briefly; never
	// fail the worker if this does not land.
	go publishVersionLabel(ctx, c, deploymentName, buildID, version, logger)

	<-ctx.Done()
	logger.Info("pizza worker stopped")
	return nil
}

// publishVersionLabel upserts the worker's friendly version label (v1/v2/v3) into
// its Worker Deployment Version metadata under the "pizzaVersion" key. Best-effort
// with a bounded retry because the version may not be registered until the first
// poll; it never blocks shutdown (honors ctx) and never fails the worker.
//
// The "pizzaVersion" key MUST match dashboard.metaKeyPizzaVersion (cmd cannot
// import that unexported const).
func publishVersionLabel(
	ctx context.Context,
	c client.Client,
	deploymentName, buildID, version string,
	logger *slog.Logger,
) {
	h := c.WorkerDeploymentClient().GetHandle(deploymentName)
	opts := client.WorkerDeploymentUpdateVersionMetadataOptions{
		Version: worker.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildID: buildID},
		MetadataUpdate: client.WorkerDeploymentMetadataUpdate{
			UpsertEntries: map[string]any{"pizzaVersion": version},
		},
	}
	for range 12 {
		_, err := h.UpdateVersionMetadata(ctx, opts)
		if err == nil {
			logger.Info("published version metadata", "buildID", buildID, "pizzaVersion", version)
			return
		}
		logger.Debug("version metadata not yet writable, will retry", "buildID", buildID, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	logger.Warn("gave up publishing version metadata", "buildID", buildID)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
