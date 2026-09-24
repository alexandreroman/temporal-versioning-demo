package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
	"go.temporal.io/sdk/client"
)

// Generator continuously starts pizza orders so there are always in-flight workflows.
// It can be paused from the dashboard; it satisfies OrderSwitch.
type Generator struct {
	c         client.Client
	taskQueue string
	interval  time.Duration
	logger    *slog.Logger
	startID   int
	paused    atomic.Bool // read by Run, written by HTTP handlers
}

var _ OrderSwitch = (*Generator)(nil)

// NewGenerator builds a Generator that starts one order per interval, numbering
// orders from startID+1.
func NewGenerator(c client.Client, taskQueue string, interval time.Duration, startID int,
	logger *slog.Logger,
) *Generator {
	return &Generator{c: c, taskQueue: taskQueue, interval: interval, startID: startID, logger: logger}
}

// Pause stops Run from starting new orders; in-flight orders are unaffected.
func (g *Generator) Pause() { g.paused.Store(true) }

// Resume lets Run start orders again from its next tick.
func (g *Generator) Resume() { g.paused.Store(false) }

// Paused reports whether starting new orders is paused.
func (g *Generator) Paused() bool { return g.paused.Load() }

// Run starts one order per interval until ctx is cancelled. Orders are started
// WITHOUT an explicit version so Temporal routes them by the deployment's
// Current/Ramping config; Pinned behaviour then locks each to its start version.
// While paused, ticks are skipped but the ticker keeps running, so resuming
// simply picks up on the next tick.
func (g *Generator) Run(ctx context.Context) {
	id := g.startID
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Skip before incrementing so a pause does not burn order IDs.
			if g.Paused() {
				continue
			}
			id++
			in := pizza.OrderInput{OrderID: id, Pizza: pizza.Menu[id%len(pizza.Menu)]}
			opts := client.StartWorkflowOptions{
				ID:        fmt.Sprintf("order-%d", id),
				TaskQueue: g.taskQueue,
			}
			if _, err := g.c.ExecuteWorkflow(ctx, opts, pizza.WorkflowTypeName, in); err != nil {
				g.logger.Warn("failed to start order", "orderId", id, "err", err)
			}
		}
	}
}
