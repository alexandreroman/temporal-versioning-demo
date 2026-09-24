package dashboard_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/dashboard"
	"go.temporal.io/sdk/client"
)

// recordingClient records the workflow IDs the Generator starts. Embedding the
// interface satisfies client.Client; any other method would panic if called.
type recordingClient struct {
	client.Client

	mu  sync.Mutex
	ids []string
}

func (c *recordingClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any,
	_ ...any,
) (client.WorkflowRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, opts.ID)
	return nil, nil
}

func (c *recordingClient) startedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGeneratorPauseToggle(t *testing.T) {
	tests := []struct {
		name string
		ops  []func(*dashboard.Generator)
		want bool
	}{
		{"starts running", nil, false},
		{"pause", []func(*dashboard.Generator){(*dashboard.Generator).Pause}, true},
		{"pause is idempotent", []func(*dashboard.Generator){
			(*dashboard.Generator).Pause, (*dashboard.Generator).Pause,
		}, true},
		{"resume after pause", []func(*dashboard.Generator){
			(*dashboard.Generator).Pause, (*dashboard.Generator).Resume,
		}, false},
		{"resume while running", []func(*dashboard.Generator){(*dashboard.Generator).Resume}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := dashboard.NewGenerator(nil, "q", time.Second, 0, discardLogger())
			for _, op := range tt.ops {
				op(g)
			}
			if got := g.Paused(); got != tt.want {
				t.Errorf("Paused() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGeneratorRunSkipsTicksWhilePaused checks that paused ticks start nothing
// and consume no order ID, so the first order after Resume is order-1.
func TestGeneratorRunSkipsTicksWhilePaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Second
		c := &recordingClient{}
		g := dashboard.NewGenerator(c, "q", interval, 0, discardLogger())
		g.Pause()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go g.Run(ctx)

		time.Sleep(3 * interval)
		synctest.Wait()
		if ids := c.startedIDs(); len(ids) != 0 {
			t.Fatalf("started %v while paused, want none", ids)
		}

		g.Resume()
		time.Sleep(interval)
		synctest.Wait()
		if ids := c.startedIDs(); !slices.Equal(ids, []string{"order-1"}) {
			t.Errorf("started %v after resume, want [order-1]", ids)
		}
	})
}
