// Command backend serves the Pizza Tracker SPA and its API.
//
// It polls Temporal for the worker-deployment routing state and the live
// orders, streams updates to the browser over SSE, and translates rollout
// actions (ramp, promote, rollback, recover) into Temporal API calls. It also
// hosts a small unversioned worker whose workflow publishes a steady stream of
// orders and switches itself off after PIZZA_PUBLISHING_TIMEOUT.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/frontend"
	"github.com/alexandreroman/temporal-versioning-demo/internal/dashboard"
	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
	"github.com/alexandreroman/temporal-versioning-demo/internal/publishing"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("backend failed", "err", err)
		os.Exit(1)
	}
}

// run wires and runs the backend until the process is signalled. It owns all
// deferred cleanup so main can exit non-zero without skipping it.
func run(logger *slog.Logger) error {
	addr := "0.0.0.0:" + cmp.Or(os.Getenv("PORT"), "8080")
	temporalAddress := cmp.Or(os.Getenv("TEMPORAL_ADDRESS"), "127.0.0.1:7233")
	namespace := cmp.Or(os.Getenv("TEMPORAL_NAMESPACE"), "default")
	deploymentName := cmp.Or(os.Getenv("TEMPORAL_DEPLOYMENT_NAME"), "pizza")
	taskQueue := cmp.Or(os.Getenv("PIZZA_TASK_QUEUE"), pizza.TaskQueue)
	pollInterval := durEnv("PIZZA_POLL_INTERVAL", time.Second, logger)
	orderInterval := durEnv("PIZZA_ORDER_INTERVAL", 6*time.Second, logger)
	publishingTimeout := durEnv("PIZZA_PUBLISHING_TIMEOUT", 15*time.Minute, logger)

	// Build the renderer before dialing Temporal: it only parses embedded
	// templates, so a broken build fails fast without any external dependency.
	renderer, err := dashboard.NewRenderer()
	if err != nil {
		return fmt.Errorf("build renderer: %w", err)
	}

	c, err := client.Dial(client.Options{HostPort: temporalAddress, Namespace: namespace})
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer c.Close()

	// The publishing workflow is backend plumbing, not part of the versioned
	// pizza rollout, so its worker deliberately has no deployment options.
	// WorkerStopTimeout gives PublishOrders time to see the worker stopping and
	// to have its failure reported, so the next worker retries it at once. It
	// stays well below Kubernetes' 30 s grace period, HTTP shutdown included.
	publisher := publishing.NewPublisher(c, taskQueue, orderInterval, logger)
	publishingWorker := worker.New(c, publishing.TaskQueue, worker.Options{WorkerStopTimeout: 5 * time.Second})
	publishing.Register(publishingWorker, publisher)
	if err := publishingWorker.Start(); err != nil {
		return fmt.Errorf("start publishing worker: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	publishingSwitch := publishing.NewSwitch(c, publishingTimeout, logger)
	// Not fatal: publishing then shows as paused and "Resume orders" retries.
	if err := publishingSwitch.Ensure(ctx); err != nil {
		logger.Error("failed to open order publishing", "err", err)
	}

	hub := dashboard.NewHub()
	// One shared label resolver so the buildID→label cache is not duplicated
	// between the reader and the actions.
	labels := dashboard.NewLabelResolver(c, deploymentName, logger)
	reader := dashboard.NewSDKReader(c, deploymentName, labels, logger)
	poller := dashboard.NewPoller(reader, pollInterval, logger, hub.Publish)
	actions := dashboard.NewActions(c, deploymentName, namespace, labels, logger)
	dashboardServer := dashboard.NewServer(hub, actions, publishingSwitch, renderer, frontend.Assets, logger)
	srv := &http.Server{
		Addr:              addr,
		Handler:           dashboardServer.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go poller.Run(ctx)
	go publishingSwitch.Run(ctx, pollInterval)
	go actions.EnsureCurrentVersion(ctx, pollInterval)
	go func() {
		logger.Info("backend listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down backend")

	// Stop the publishing worker before the HTTP server: open SSE streams can
	// hold srv.Shutdown for its whole timeout, and the hand-over of the running
	// PublishOrders attempt must not wait behind them.
	publishingWorker.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	return nil
}

func durEnv(key string, fallback time.Duration, logger *slog.Logger) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("invalid duration env, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	if d <= 0 {
		// A non-positive interval would panic time.NewTicker; treat it as invalid.
		logger.Warn("non-positive duration env, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return d
}
