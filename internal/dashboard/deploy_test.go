package dashboard

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOrderSwitch is an in-memory OrderSwitch standing in for the Generator.
type fakeOrderSwitch struct{ paused bool }

func (f *fakeOrderSwitch) Pause()       { f.paused = true }
func (f *fakeOrderSwitch) Resume()      { f.paused = false }
func (f *fakeOrderSwitch) Paused() bool { return f.paused }

// newTestServer builds a Server with a live hub, renderer and a fake order
// switch but no Temporal client. It is suitable for handlers that only render
// fragments (deploy/ramp, rollback, modal) or toggle the switch, and never touch
// s.actions. The frontend is a 404 stub so Routes() can be exercised.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return &Server{
		hub:         NewHub(),
		orderSwitch: &fakeOrderSwitch{},
		renderer:    renderer,
		frontend:    http.NotFoundHandler(),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func versionsState(currentLabel string, labels ...string) DashboardState {
	cards := make([]VersionCard, 0, len(labels))
	for _, l := range labels {
		card := VersionCard{Version: l, Status: StatusInactive}
		if l == currentLabel {
			card.Status = StatusCurrent
			card.TrafficPct = 100
		}
		cards = append(cards, card)
	}
	return DashboardState{Versions: cards}
}

// rampingState returns a state where one version is Ramping at rampPct and
// another is Current taking the remaining traffic, mirroring BuildState.
func rampingState(rampingLabel string, rampPct int, currentLabel string, labels ...string) DashboardState {
	cards := make([]VersionCard, 0, len(labels))
	for _, l := range labels {
		card := VersionCard{Version: l, Status: StatusInactive}
		switch l {
		case rampingLabel:
			card.Status = StatusRamping
			card.TrafficPct = rampPct
		case currentLabel:
			card.Status = StatusCurrent
			card.TrafficPct = 100 - rampPct
		}
		cards = append(cards, card)
	}
	return DashboardState{Versions: cards}
}

// renderRegion renders a named SSE region to a string, failing the test on error.
func renderRegion(t *testing.T, r *Renderer, region string, state DashboardState) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Region(&buf, region, state); err != nil {
		t.Fatalf("render %q: %v", region, err)
	}
	return buf.String()
}

func TestHasRamping(t *testing.T) {
	tests := []struct {
		name  string
		state DashboardState
		want  bool
	}{
		{"true when a version is ramping", rampingState("v3", 25, "v2", "v1", "v2", "v3"), true},
		{"false when only current and inactive", versionsState("v2", "v1", "v2", "v3"), false},
		{"false for empty slice", DashboardState{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasRamping(tt.state.Versions); got != tt.want {
				t.Errorf("hasRamping() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRendererControls(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	// Deploy and Rollback always render; each carries a disabled attribute
	// immediately before its label, so assert on the precise `disabled>Deploy`
	// and `disabled>Rollback` markup. Recover is no longer a control here — it is
	// now a per-card action.
	always := []string{
		`hx-get="/deploy"`,   // Deploy opens the modal host
		`hx-get="/rollback"`, // Rollback opens the modal host
	}

	tests := []struct {
		name             string
		state            DashboardState
		deployDisabled   bool
		rollbackDisabled bool
	}{
		{
			name:             "no ramp disables rollback",
			state:            versionsState("v2", "v1", "v2", "v3"),
			deployDisabled:   false,
			rollbackDisabled: true,
		},
		{
			name:             "ramping enables rollback",
			state:            rampingState("v3", 25, "v2", "v1", "v2", "v3"),
			deployDisabled:   false,
			rollbackDisabled: false,
		},
		{
			name:             "single version disables deploy",
			state:            versionsState("v1", "v1"),
			deployDisabled:   true,
			rollbackDisabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := renderRegion(t, r, "controls", tt.state)
			for _, w := range always {
				if !strings.Contains(out, w) {
					t.Errorf("controls output missing %q\n--- output ---\n%s", w, out)
				}
			}
			if got := strings.Contains(out, `disabled>Deploy`); got != tt.deployDisabled {
				t.Errorf("Deploy disabled = %v, want %v\n--- output ---\n%s", got, tt.deployDisabled, out)
			}
			if got := strings.Contains(out, `disabled>Rollback`); got != tt.rollbackDisabled {
				t.Errorf("Rollback disabled = %v, want %v\n--- output ---\n%s", got, tt.rollbackDisabled, out)
			}
			if strings.Contains(out, `/recover`) {
				t.Errorf("controls must not carry a recover button anymore\n--- output ---\n%s", out)
			}
		})
	}
}

func TestOrderRecoverButton(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	st := DashboardState{Orders: []Order{
		{ID: "order-7", Version: "v3", Failing: true},
		{ID: "order-8", Version: "v1", Failing: false},
	}}
	out := renderRegion(t, r, "orders", st)

	// A failing order shows the per-card recover button targeting its own workflow.
	for _, w := range []string{
		`hx-post="/orders/order-7/recover"`,
		`class="recover-btn"`,
		`aria-label="Recover order-7"`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("orders output missing %q\n--- output ---\n%s", w, out)
		}
	}
	// A healthy order has no recover button.
	if strings.Contains(out, `hx-post="/orders/order-8/recover"`) {
		t.Errorf("healthy order must not carry a recover button\n--- output ---\n%s", out)
	}
}

func TestRampViewFor(t *testing.T) {
	tests := []struct {
		name        string
		state       DashboardState
		selected    string
		wantPct     int
		wantStopIdx int
	}{
		{"ramping keeps its 25% at idx 0", rampingState("v3", 25, "v2", "v1", "v2", "v3"), "v3", 25, 0},
		{"ramping at 50% at idx 1", rampingState("v3", 50, "v2", "v1", "v2", "v3"), "v3", 50, 1},
		{"current -> 100% at idx 2", versionsState("v2", "v1", "v2", "v3"), "v2", 100, 2},
		{"inactive -> 25% at idx 0", versionsState("v2", "v1", "v2", "v3"), "v1", 25, 0},
		{"unknown selection -> 25% at idx 0", versionsState("v2", "v1", "v2", "v3"), "v9", 25, 0},
		{"empty selection -> 25% at idx 0", versionsState("v2", "v1", "v2", "v3"), "", 25, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rampViewFor(tt.state, tt.selected)
			if got.Pct != tt.wantPct || got.StopIdx != tt.wantStopIdx {
				t.Errorf("rampViewFor(%q) = %+v, want Pct=%d StopIdx=%d",
					tt.selected, got, tt.wantPct, tt.wantStopIdx)
			}
		})
	}
}

func TestDefaultDeploySelection(t *testing.T) {
	tests := []struct {
		name  string
		state DashboardState
		want  string
	}{
		{"prefers ramping over current", rampingState("v3", 25, "v2", "v1", "v2", "v3"), "v3"},
		{"falls back to current without a ramp", versionsState("v2", "v1", "v2", "v3"), "v2"},
		{"falls back to first card when neither", versionsState("", "v1", "v2", "v3"), "v1"},
		{"empty when no versions", DashboardState{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultDeploySelection(tt.state); got != tt.want {
				t.Errorf("defaultDeploySelection() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildDeployModalView(t *testing.T) {
	t.Run("ramping pre-checked with its in-progress ramp", func(t *testing.T) {
		state := rampingState("v3", 25, "v2", "v1", "v2", "v3")
		view := buildDeployModalView(state)

		want := map[string]bool{"v1": false, "v2": false, "v3": true}
		if len(view.Versions) != len(want) {
			t.Fatalf("got %d options, want %d", len(view.Versions), len(want))
		}
		for _, opt := range view.Versions {
			if opt.Checked != want[opt.Version] {
				t.Errorf("option %q Checked = %v, want %v", opt.Version, opt.Checked, want[opt.Version])
			}
		}
		if view.Ramp.Pct != 25 {
			t.Errorf("Ramp.Pct = %d, want 25", view.Ramp.Pct)
		}
	})

	t.Run("current pre-checked with 100% ramp without a ramp in progress", func(t *testing.T) {
		state := versionsState("v2", "v1", "v2", "v3")
		view := buildDeployModalView(state)

		want := map[string]bool{"v1": false, "v2": true, "v3": false}
		for _, opt := range view.Versions {
			if opt.Checked != want[opt.Version] {
				t.Errorf("option %q Checked = %v, want %v", opt.Version, opt.Checked, want[opt.Version])
			}
		}
		if view.Ramp.Pct != 100 {
			t.Errorf("Ramp.Pct = %d, want 100", view.Ramp.Pct)
		}
	})

	t.Run("falls back to first version when none current", func(t *testing.T) {
		state := versionsState("", "v1", "v2", "v3")
		view := buildDeployModalView(state)

		if !view.Versions[0].Checked {
			t.Errorf("first option should be checked when no Current version")
		}
		for _, opt := range view.Versions[1:] {
			if opt.Checked {
				t.Errorf("option %q should not be checked", opt.Version)
			}
		}
		// The fallback selection is Inactive, so the ramp defaults to 25%.
		if view.Ramp.Pct != 25 {
			t.Errorf("Ramp.Pct = %d, want 25", view.Ramp.Pct)
		}
	})

	t.Run("no versions yields empty options at 25%", func(t *testing.T) {
		view := buildDeployModalView(DashboardState{})
		if len(view.Versions) != 0 {
			t.Errorf("got %d options, want 0", len(view.Versions))
		}
		if view.Ramp.Pct != 25 {
			t.Errorf("Ramp.Pct = %d, want 25", view.Ramp.Pct)
		}
	})
}

func TestRendererDeployModal(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	state := versionsState("v2", "v1", "v2", "v3")
	view := buildDeployModalView(state)

	var buf bytes.Buffer
	if err := r.DeployModal(&buf, view); err != nil {
		t.Fatalf("DeployModal: %v", err)
	}
	out := buf.String()

	want := []string{
		`id="deploy-ramp"`,
		`class="modal-scrim"`,   // full scrim is now part of the fragment
		`name="version"`,        // radios post the version field
		`value="v2" checked`,    // Current version pre-checked
		`hx-get="/deploy/ramp"`, // radios drive the ramp re-render (no ?version=)
		`hx-post="/deploy"`,     // the form submits to the unified deploy endpoint
		`type="submit"`,         // Apply is a real submit button
		`100%`,                  // Current => 100% ramp
		`value="2"`,             // slider at stop index 2
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("deploy modal output missing %q\n--- output ---\n%s", w, out)
		}
	}
	if strings.Contains(out, `value="v1" checked`) || strings.Contains(out, `value="v3" checked`) {
		t.Errorf("only the Current version should be checked\n--- output ---\n%s", out)
	}
	if strings.Contains(out, `?version=`) {
		t.Errorf("radio hx-get must not carry a version query string\n--- output ---\n%s", out)
	}
	for _, js := range []string{"onclick", "oninput", "hx-on"} {
		if strings.Contains(out, js) {
			t.Errorf("deploy modal must be JS-free but contains %q\n--- output ---\n%s", js, out)
		}
	}
}

func TestRendererDeployRamp(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	tests := []struct {
		name    string
		view    rampView
		wantPct string
		wantIdx string
	}{
		{"current at 100%", rampViewFor(versionsState("v2", "v1", "v2"), "v2"), "100%", `value="2"`},
		{"other at 25%", rampViewFor(versionsState("v2", "v1", "v2"), "v1"), "25%", `value="0"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := r.DeployRamp(&buf, tt.view); err != nil {
				t.Fatalf("DeployRamp: %v", err)
			}
			out := buf.String()
			// The ramp root must keep its id so the radio's outerHTML swap stays anchored.
			for _, w := range []string{`id="deploy-ramp"`, tt.wantPct, tt.wantIdx} {
				if !strings.Contains(out, w) {
					t.Errorf("deploy ramp output missing %q\n--- output ---\n%s", w, out)
				}
			}
		})
	}
}

func TestRampViewForStop(t *testing.T) {
	tests := []struct {
		name        string
		stop        string
		wantPct     int
		wantStopIdx int
	}{
		{"stop 0 -> 25% at idx 0", "0", 25, 0},
		{"stop 2 -> 100% at idx 2", "2", 100, 2},
		{"out of range -> 25% at idx 0", "9", 25, 0},
		{"empty -> 25% at idx 0", "", 25, 0},
		{"non-numeric -> 25% at idx 0", "x", 25, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rampViewForStop(tt.stop)
			if got.Pct != tt.wantPct || got.StopIdx != tt.wantStopIdx {
				t.Errorf("rampViewForStop(%q) = %+v, want Pct=%d StopIdx=%d",
					tt.stop, got, tt.wantPct, tt.wantStopIdx)
			}
		})
	}
}

func TestRendererRollbackModal(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	var buf bytes.Buffer
	if err := r.RollbackModal(&buf); err != nil {
		t.Fatalf("RollbackModal: %v", err)
	}
	out := buf.String()
	for _, w := range []string{`class="modal-scrim"`, `Roll back?`, `hx-post="/rollback"`} {
		if !strings.Contains(out, w) {
			t.Errorf("rollback modal output missing %q\n--- output ---\n%s", w, out)
		}
	}
}

func TestHandleClose(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleClose(rec, httptest.NewRequest(http.MethodDelete, "/modal", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty so #modal-host is cleared, got %q", rec.Body.String())
	}
}

func TestHandleRollbackModal(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleRollbackModal(rec, httptest.NewRequest(http.MethodGet, "/rollback", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Roll back?") {
		t.Errorf("rollback modal should contain the confirmation prompt\n--- body ---\n%s", rec.Body.String())
	}
}

// TestHandleDeployValidation covers the two 400 validation paths in handleDeploy.
// Both reject the request before touching s.actions, so a nil Temporal client is
// fine here. The error body is the toast fragment rendered by writeError, so we
// assert on its stable message substring.
func TestHandleDeployValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"invalid version", "version=v9&stop=3", "invalid version"},
		{"non-numeric stop", "version=v2&stop=x", "invalid ramp selection"},
		{"out-of-range stop", "version=v2&stop=99", "invalid ramp selection"},
		{"negative stop", "version=v2&stop=-1", "invalid ramp selection"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			req := httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()

			s.handleDeploy(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			out := rec.Body.String()
			if out == "" {
				t.Fatal("body should carry the toast fragment, got empty")
			}
			if !strings.Contains(out, tt.wantMsg) {
				t.Errorf("body missing %q\n--- body ---\n%s", tt.wantMsg, out)
			}
			if !strings.Contains(out, `class="toast`) {
				t.Errorf("body should be the toast fragment\n--- body ---\n%s", out)
			}
		})
	}
}

func TestHandleDeployRampWithStop(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleDeployRamp(rec, httptest.NewRequest(http.MethodGet, "/deploy/ramp?stop=2", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	out := rec.Body.String()
	for _, w := range []string{`id="deploy-ramp"`, `100%`, `value="2"`} {
		if !strings.Contains(out, w) {
			t.Errorf("deploy ramp output missing %q\n--- body ---\n%s", w, out)
		}
	}
}
