// Command backend serves the Pizza Tracker SPA and its API.
//
// It polls Temporal for the worker-deployment routing state and the live
// orders, streams updates to the browser over SSE, generates a steady stream of
// orders, and translates rollout actions (ramp, promote, rollback, recover)
// into Temporal API calls.
package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/frontend"
	"github.com/alexandreroman/temporal-versioning-demo/internal/dashboard"
	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
	"go.temporal.io/sdk/client"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := "0.0.0.0:" + cmp.Or(os.Getenv("PORT"), "8080")
	temporalAddress := cmp.Or(os.Getenv("TEMPORAL_ADDRESS"), "127.0.0.1:7233")
	namespace := cmp.Or(os.Getenv("TEMPORAL_NAMESPACE"), "default")
	deploymentName := cmp.Or(os.Getenv("TEMPORAL_DEPLOYMENT_NAME"), "pizza")
	taskQueue := cmp.Or(os.Getenv("PIZZA_TASK_QUEUE"), pizza.TaskQueue)
	pollInterval := durEnv("PIZZA_POLL_INTERVAL", time.Second, logger)
	orderInterval := durEnv("PIZZA_ORDER_INTERVAL", 6*time.Second, logger)

	// Build the renderer before dialing Temporal so this fail-fast path has no
	// pending defers (it only parses embedded templates, no external dependency).
	renderer, err := dashboard.NewRenderer()
	if err != nil {
		logger.Error("failed to build renderer", "err", err)
		os.Exit(1)
	}

	c, err := client.Dial(client.Options{HostPort: temporalAddress, Namespace: namespace})
	if err != nil {
		logger.Error("failed to connect to Temporal", "err", err)
		os.Exit(1)
	}
	defer c.Close()

	hub := dashboard.NewHub()
	// One shared label resolver so the buildID→label cache is not duplicated
	// between the reader and the actions.
	labels := dashboard.NewLabelResolver(c, deploymentName, logger)
	reader := dashboard.NewSDKReader(c, deploymentName, labels, logger)
	poller := dashboard.NewPoller(reader, pollInterval, logger, hub.Publish)
	actions := dashboard.NewActions(c, deploymentName, namespace, labels, logger)
	// Seed startID from the wall clock so order IDs do not reset to order-1 on
	// every restart and collide with a still-open order from the previous run.
	// This is backend/client code (not workflow code), so using the clock is fine.
	gen := dashboard.NewGenerator(c, taskQueue, orderInterval, int(time.Now().Unix()), logger)
	srv := &http.Server{
		Addr:              addr,
		Handler:           dashboard.NewServer(hub, actions, renderer, frontend.Assets, logger).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go poller.Run(ctx)
	go gen.Run(ctx)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
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
