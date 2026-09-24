package dashboard

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
)

// VersionStatus is the lifecycle chip shown on a deployment card.
type VersionStatus string

// Deployment version lifecycle states.
const (
	StatusCurrent  VersionStatus = "CURRENT"
	StatusRamping  VersionStatus = "RAMPING"
	StatusDraining VersionStatus = "DRAINING"
	StatusInactive VersionStatus = "INACTIVE"
)

// Order is one live order card.
type Order struct {
	ID          string
	Version     string // friendly: v1/v2/v3
	Pizza       string
	Steps       []pizza.StepLabel
	CurrentStep int
	Failing     bool
	Done        bool
	ElapsedSec  int
}

// VersionCard is one deployment-panel card.
type VersionCard struct {
	Version     string // friendly: v1/v2/v3
	Status      VersionStatus
	TrafficPct  int
	PinnedCount int
}

// DashboardState is the full SSE payload.
type DashboardState struct {
	Orders   []Order
	Versions []VersionCard
	// OrdersPaused is stamped by the server at render time from its pause
	// switch (see Server.writeFrame); the poller never sets it.
	OrdersPaused bool
}

// VersionSummary mirrors the fields BuildState needs from a Temporal version summary.
type VersionSummary struct {
	BuildID      string
	PizzaVersion string // friendly label from version metadata; "" => fall back to CreateTime
	CreateTime   time.Time
	Draining     bool // true while the version is draining or drained (both map to DRAINING)
}

// Routing mirrors the routing config BuildState needs.
type Routing struct {
	CurrentBuildID string
	RampingBuildID string
	RampingPct     int
}

// LiveOrder is one open workflow's state plus the build it is pinned to.
type LiveOrder struct {
	WorkflowID string
	BuildID    string // Worker Deployment Version Build ID this run is pinned to
	State      pizza.OrderState
	ElapsedSec int
}

// BuildState maps Temporal data to the SPA payload. Friendly version labels are
// assigned from version metadata when present; otherwise they fall back to
// CreateTime order (oldest summary = v1). Per-version override keeps mixed
// states correct while workers publish their metadata.
func BuildState(routing Routing, summaries []VersionSummary, orders []LiveOrder) DashboardState {
	sorted := slices.Clone(summaries)
	slices.SortFunc(sorted, func(a, b VersionSummary) int { return a.CreateTime.Compare(b.CreateTime) })

	// Label by version metadata when present; otherwise fall back to CreateTime
	// order (oldest = v1). Per-version override keeps mixed states correct while
	// the three workers publish their metadata.
	label := make(map[string]string, len(sorted)) // buildID -> v1/v2/v3
	for i, s := range sorted {
		if s.PizzaVersion != "" {
			label[s.BuildID] = s.PizzaVersion
		} else {
			label[s.BuildID] = friendly(i)
		}
	}

	// Count open orders by their resolved friendly label, not by BuildID:
	// visibility (ListWorkflowExecutions) never populates VersioningInfo, so
	// o.BuildID is "" in production and keying on it buckets every order under
	// the empty key. The label falls back to the workflow's self-reported
	// State.Version, which is the same value the cards resolve to.
	pinned := make(map[string]int, len(sorted)) // friendly label -> open order count
	outOrders := make([]Order, 0, len(orders))
	for _, o := range orders {
		version := pickLabel(label, o.BuildID, o.State.Version)
		pinned[version]++
		outOrders = append(outOrders, Order{
			ID:          o.WorkflowID,
			Version:     version,
			Pizza:       o.State.Pizza,
			Steps:       o.State.Steps,
			CurrentStep: o.State.CurrentStep,
			Failing:     o.State.Failing,
			Done:        o.State.Done,
			ElapsedSec:  o.ElapsedSec,
		})
	}

	cards := make([]VersionCard, 0, len(sorted))
	for _, s := range sorted {
		card := VersionCard{
			Version:     label[s.BuildID],
			PinnedCount: pinned[label[s.BuildID]],
			Status:      StatusInactive,
		}
		switch {
		case s.BuildID == routing.CurrentBuildID:
			card.Status = StatusCurrent
			card.TrafficPct = 100 - routing.RampingPct
		case s.BuildID == routing.RampingBuildID:
			card.Status = StatusRamping
			card.TrafficPct = routing.RampingPct
		case s.Draining:
			card.Status = StatusDraining
		}
		cards = append(cards, card)
	}

	// Display cards in friendly-label order (v1, v2, v3); a stable sort keeps the
	// CreateTime order for ties. See compareVersion for the ordering rationale.
	slices.SortStableFunc(cards, func(a, b VersionCard) int {
		return compareVersion(a.Version, b.Version)
	})

	return DashboardState{Orders: outOrders, Versions: cards}
}

// compareVersion orders friendly version labels (v1 < v2 < v3) by the integer
// after the leading "v". If either label does not parse, it falls back to a plain
// string comparison so the order stays deterministic.
func compareVersion(a, b string) int {
	na, errA := strconv.Atoi(strings.TrimPrefix(a, "v"))
	nb, errB := strconv.Atoi(strings.TrimPrefix(b, "v"))
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	return cmp.Compare(na, nb)
}

func friendly(i int) string {
	return "v" + strconv.Itoa(i+1)
}

// pickLabel prefers the deployment-ordering label; falls back to the order's
// self-reported version when the build is not (yet) in the summaries.
func pickLabel(label map[string]string, buildID, reported string) string {
	if l, ok := label[buildID]; ok && l != "" {
		return l
	}
	return reported
}
