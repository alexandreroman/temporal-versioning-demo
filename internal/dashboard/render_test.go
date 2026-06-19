package dashboard_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/dashboard"
	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
)

// buildFixture mirrors the state_test.go scenario: v2 current at 90%, v3 ramping
// at 10% with one failing (drone-stuck) order.
func buildFixture() dashboard.DashboardState {
	base := time.Unix(1_000_000, 0)
	summaries := []dashboard.VersionSummary{
		{BuildID: "b1", CreateTime: base},
		{BuildID: "b2", CreateTime: base.Add(2 * time.Minute)},
		{BuildID: "b3", CreateTime: base.Add(4 * time.Minute), Draining: true},
	}
	routing := dashboard.Routing{CurrentBuildID: "b2", RampingBuildID: "b3", RampingPct: 10}
	orders := []dashboard.LiveOrder{
		{WorkflowID: "order-1", BuildID: "b2", ElapsedSec: 72, State: pizza.OrderState{
			Version: "v2", Pizza: "Pepperoni",
			Steps: v2Steps, CurrentStep: 1,
		}},
		{WorkflowID: "order-2", BuildID: "b3", ElapsedSec: 130, State: pizza.OrderState{
			Version: "v3", Pizza: "Diavola",
			Steps: v3Steps, CurrentStep: 3, Failing: true,
		}},
	}
	return dashboard.BuildState(routing, summaries, orders)
}

func render(t *testing.T, r *dashboard.Renderer, region string, state dashboard.DashboardState) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Region(&buf, region, state); err != nil {
		t.Fatalf("render %q: %v", region, err)
	}
	return buf.String()
}

func TestRendererRegions(t *testing.T) {
	r, err := dashboard.NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	state := buildFixture()

	tests := []struct {
		region string
		want   []string
	}{
		{"orders", []string{
			"#order-1", "#order-2",
			"Pepperoni", "Diavola",
			`vb b-v2`, `vb b-v3`,
			"1:12",               // order-1 elapsed (72s)
			`class="order fail"`, // failing card styling (the red card is the failing cue)
			`node err`,           // errored stepper node
		}},
		{"versions", []string{
			`vb b-v1`, `vb b-v2`, `vb b-v3`,
			`id="ver-v3"`,         // stable card id lets idiomorph morph the same node across SSE ticks
			"INACTIVE",            // v1 inactive
			`chip c-cur">CURRENT`, // v2 current
			"RAMPING 10%",         // v3 ramping with pct
			// Content-keyed chip id: it encodes status + traffic %, so it changes
			// when the chip's value changes, making idiomorph replace the node and
			// replay the chip-pulse entry animation (no JS).
			`id="chip-v2-CURRENT-90"`,
			`id="chip-v3-RAMPING-10"`,
			"1 in flight", // v3 has one pinned (in-flight) order
			// Traffic bar fill: color via class, width via the --bar-w custom
			// property (the rule itself lives in index.html, not the template).
			// The stable id="bar-v3" lets idiomorph morph the same span so its
			// CSS width transition runs when --bar-w changes.
			`<span id="bar-v3" class="b-v3"`, // v3 ramping bar node, stably identified
			`style="--bar-w:10%">`,           // v3 ramping bar at 10%
		}},
	}

	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			out := render(t, r, tt.region, state)
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("region %q output missing %q\n--- output ---\n%s", tt.region, want, out)
				}
			}
		})
	}
}

func TestRendererToast(t *testing.T) {
	r, err := dashboard.NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	var buf bytes.Buffer
	if err := r.Toast(&buf, "Recovered 3 stuck order(s)"); err != nil {
		t.Fatalf("Toast: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`class="toast show"`, "Recovered 3 stuck order(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("toast output missing %q\n--- output ---\n%s", want, out)
		}
	}
}
