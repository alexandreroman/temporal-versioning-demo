package pizza

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
)

// This file holds the pizza step activities. Their per-step wait times are hardcoded
// here (each activity owns its own pacing); a worker registers only the subset its
// version runs (see RegisterVN in workflow_vN.go). Simulating the waits inside the
// activities (instead of the workflow sleeping) keeps timers out of the workflow history.
//
// The durations are package vars rather than consts so unit tests can set them to zero
// and keep the suite fast (see workflow_test.go); in production they pace the demo.
var (
	// stepDwell is the work time of each ordinary step, sized so a full order lasts ~60-90s.
	stepDwell = 15 * time.Second
	// deliveredDwell is the (shorter) wait of the final Deliver step: how long the
	// completed (all-green) order stays on the dashboard before its workflow closes and
	// the card leaves the board.
	deliveredDwell = 7 * time.Second
	// droneAttempt is how long each (failing) drone delivery attempt takes; it paces the
	// v3 retry cadence from the activity side so no workflow timer is needed.
	droneAttempt = 5 * time.Second
)

// dwell simulates the time a real step takes. It is context-aware so a cancelled
// activity returns promptly. A zero or negative duration (e.g. in unit tests) is a no-op.
func dwell(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Receive acknowledges a new order.
func Receive(ctx context.Context, in OrderInput) error {
	activity.GetLogger(ctx).Info("order received", "orderId", in.OrderID, "pizza", in.Pizza)
	return dwell(ctx, stepDwell)
}

// Cook prepares the pizza.
func Cook(ctx context.Context, in OrderInput) error {
	activity.GetLogger(ctx).Info("cooking", "orderId", in.OrderID)
	return dwell(ctx, stepDwell)
}

// QualityCheck inspects the pizza before delivery (added in v2).
func QualityCheck(ctx context.Context, in OrderInput) error {
	activity.GetLogger(ctx).Info("quality check", "orderId", in.OrderID)
	return dwell(ctx, stepDwell)
}

// OutForDelivery dispatches the pizza to a courier.
func OutForDelivery(ctx context.Context, in OrderInput) error {
	activity.GetLogger(ctx).Info("out for delivery", "orderId", in.OrderID)
	return dwell(ctx, stepDwell)
}

// Deliver marks the order as delivered.
func Deliver(ctx context.Context, in OrderInput) error {
	activity.GetLogger(ctx).Info("delivered", "orderId", in.OrderID)
	return dwell(ctx, deliveredDwell)
}

// DroneDelivery is the buggy v3 step: each attempt spends droneAttempt simulating the
// flight, then always fails, so v3 orders stall and go red until they are recovered.
func DroneDelivery(ctx context.Context, in OrderInput) error {
	if err := dwell(ctx, droneAttempt); err != nil {
		return err
	}
	activity.GetLogger(ctx).Warn("drone delivery failed", "orderId", in.OrderID)
	return errors.New("drone delivery failed: navigation system offline")
}
