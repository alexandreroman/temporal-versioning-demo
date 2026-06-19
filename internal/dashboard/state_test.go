package dashboard_test

import (
	"testing"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/dashboard"
	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
)

// v2Steps and v3Steps are the step lists the dashboard fixtures render. They mirror
// the inline lists in the v2/v3 workflows; the dashboard only needs a plausibly
// shaped stepper, not the workflow's own source of truth.
var (
	v2Steps = []pizza.StepLabel{
		pizza.StepReceived, pizza.StepCooking, pizza.StepQualityCheck,
		pizza.StepOutForDelivery, pizza.StepDelivered,
	}
	v3Steps = []pizza.StepLabel{
		pizza.StepReceived, pizza.StepCooking, pizza.StepQualityCheck,
		pizza.StepDroneDelivery, pizza.StepDelivered,
	}
)

func TestBuildStateLabelsByCreateTimeAndCountsPinned(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	summaries := []dashboard.VersionSummary{
		{BuildID: "b2", CreateTime: base.Add(2 * time.Minute)},
		{BuildID: "b1", CreateTime: base}, // oldest -> v1
		{BuildID: "b3", CreateTime: base.Add(4 * time.Minute), Draining: true},
	}
	routing := dashboard.Routing{CurrentBuildID: "b2", RampingBuildID: "b3", RampingPct: 10}
	orders := []dashboard.LiveOrder{
		{WorkflowID: "order-1", BuildID: "b2", State: pizza.OrderState{
			Version: "v2", Pizza: "Pepperoni",
			Steps: v2Steps, CurrentStep: 1,
		}},
		{WorkflowID: "order-2", BuildID: "b3", State: pizza.OrderState{
			Version: "v3", Pizza: "Diavola",
			Steps: v3Steps, CurrentStep: 3, Failing: true,
		}},
		{WorkflowID: "order-3", BuildID: "b2", State: pizza.OrderState{
			Version: "v2", Pizza: "Marinara",
			Steps: v2Steps, CurrentStep: 0,
		}},
	}

	st := dashboard.BuildState(routing, summaries, orders)

	wantOrder := []string{"v1", "v2", "v3"}
	for i, want := range wantOrder {
		if st.Versions[i].Version != want {
			t.Errorf("Versions[%d] = %q, want %q (cards must be ordered v1, v2, v3)",
				i, st.Versions[i].Version, want)
		}
	}
	byVer := map[string]dashboard.VersionCard{}
	for _, c := range st.Versions {
		byVer[c.Version] = c
	}
	if byVer["v1"].Status != dashboard.StatusInactive {
		t.Errorf("v1 status = %s, want INACTIVE", byVer["v1"].Status)
	}
	if byVer["v2"].Status != dashboard.StatusCurrent || byVer["v2"].TrafficPct != 90 || byVer["v2"].PinnedCount != 2 {
		t.Errorf("v2 card = %+v", byVer["v2"])
	}
	if byVer["v3"].Status != dashboard.StatusRamping || byVer["v3"].TrafficPct != 10 || byVer["v3"].PinnedCount != 1 {
		t.Errorf("v3 card = %+v", byVer["v3"])
	}
}

// Regression for the "0 in flight" bug: in production, ListWorkflowExecutions
// (visibility) never populates VersioningInfo, so every LiveOrder.BuildID is "".
// Counting by BuildID then buckets all orders under the empty key, leaving every
// card at PinnedCount 0. Counts must instead key off the resolved friendly label
// (which falls back to the workflow's self-reported State.Version).
func TestBuildStateCountsPinnedByLabelWhenVisibilityOmitsBuildID(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	summaries := []dashboard.VersionSummary{
		{BuildID: "b1", CreateTime: base, PizzaVersion: "v1"},
		{BuildID: "b2", CreateTime: base.Add(2 * time.Minute), PizzaVersion: "v2"},
		{BuildID: "b3", CreateTime: base.Add(4 * time.Minute), PizzaVersion: "v3", Draining: true},
	}
	routing := dashboard.Routing{CurrentBuildID: "b2", RampingBuildID: "b3", RampingPct: 10}
	// Visibility omits the build ID, but each order self-reports its version.
	orders := []dashboard.LiveOrder{
		{WorkflowID: "order-1", State: pizza.OrderState{
			Version: "v2", Pizza: "Pepperoni", Steps: v2Steps, CurrentStep: 1,
		}},
		{WorkflowID: "order-2", State: pizza.OrderState{
			Version: "v2", Pizza: "Marinara", Steps: v2Steps, CurrentStep: 0,
		}},
		{WorkflowID: "order-3", State: pizza.OrderState{
			Version: "v3", Pizza: "Diavola", Steps: v3Steps, CurrentStep: 3, Failing: true,
		}},
	}

	st := dashboard.BuildState(routing, summaries, orders)

	byVer := map[string]dashboard.VersionCard{}
	for _, c := range st.Versions {
		byVer[c.Version] = c
	}
	if byVer["v1"].PinnedCount != 0 {
		t.Errorf("v1 PinnedCount = %d, want 0 (no orders)", byVer["v1"].PinnedCount)
	}
	if byVer["v2"].PinnedCount != 2 {
		t.Errorf("v2 PinnedCount = %d, want 2", byVer["v2"].PinnedCount)
	}
	if byVer["v3"].PinnedCount != 1 {
		t.Errorf("v3 PinnedCount = %d, want 1", byVer["v3"].PinnedCount)
	}
}

func TestBuildStateLabelsByMetadataWhenPresent(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	// CreateTime order would label b_new=v1, but metadata says it is v3.
	summaries := []dashboard.VersionSummary{
		{BuildID: "b_new", CreateTime: base, PizzaVersion: "v3"},
		{BuildID: "b_old", CreateTime: base.Add(2 * time.Minute), PizzaVersion: "v1"},
	}
	routing := dashboard.Routing{CurrentBuildID: "b_old"}
	st := dashboard.BuildState(routing, summaries, nil)

	byVer := map[string]dashboard.VersionCard{}
	for _, c := range st.Versions {
		byVer[c.Version] = c
	}
	// b_old is the CurrentBuildID and is labelled v1 via metadata.
	if byVer["v1"].Status != dashboard.StatusCurrent {
		t.Errorf("v1 card = %+v, want status CURRENT (b_old labelled v1 via metadata)", byVer["v1"])
	}
	if _, ok := byVer["v3"]; !ok {
		t.Errorf("expected a v3 card from metadata, got %+v", st.Versions)
	}
}
