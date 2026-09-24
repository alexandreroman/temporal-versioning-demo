package dashboard

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"strconv"

	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// stepNode is the per-stepper-node view model: its CSS state class, the sprite
// icon shown inside the dot, and the step label.
type stepNode struct {
	Class string // "" | "done" | "cur" | "err"
	// Icon is the sprite symbol id (e.g. "step-cook"). The template also adds it
	// to the node as a modifier class, so the CSS can animate each step's icon.
	Icon  string
	Label string
}

// pizzaIcons maps each pizza.Menu name to the symbol id of its illustration in
// the SVG sprite of frontend/index.html.
var pizzaIcons = map[string]string{
	"Margherita":       "pizza-margherita",
	"Pepperoni":        "pizza-pepperoni",
	"Quattro Formaggi": "pizza-quattro-formaggi",
	"Marinara":         "pizza-marinara",
	"Capricciosa":      "pizza-capricciosa",
	"Diavola":          "pizza-diavola",
	"Hawaiian":         "pizza-hawaiian",
	"Veggie Supreme":   "pizza-veggie-supreme",
}

// stepIcons maps each stepper label to the symbol id of its icon in the SVG
// sprite of frontend/index.html.
var stepIcons = map[pizza.StepLabel]string{
	pizza.StepReceived:       "step-new",
	pizza.StepCooking:        "step-cook",
	pizza.StepQualityCheck:   "step-qc",
	pizza.StepOutForDelivery: "step-out",
	pizza.StepDroneDelivery:  "step-drone",
	pizza.StepDelivered:      "step-done",
}

// Fallback sprite symbols for names the maps above do not know.
const (
	defaultPizzaIcon = "pizza-margherita"
	defaultStepIcon  = "step-generic"
)

// toastView is the view model for the toast template.
type toastView struct {
	Message string
}

// rampStops mirrors the client slider stops; the slider value is the stop index.
var rampStops = []int{25, 50, 100}

// Ramp percentages with special meaning: the start of a new ramp and a full
// promotion.
const (
	rampDefaultPct = 25
	rampFullPct    = 100
)

// rampView is the view model for the ramp section (slider position + label).
type rampView struct {
	Pct     int // 25/50/100
	StopIdx int // 0..2, slider position matching Pct
}

// deployVersionOption is one selectable radio in the Deploy modal.
type deployVersionOption struct {
	Version string // v1/v2/v3
	Checked bool
}

// deployModalView is the view model for the Deploy modal fragment.
type deployModalView struct {
	Versions []deployVersionOption
	Ramp     rampView
}

// stopIndex returns the index of pct in rampStops, or 0 when pct is not a stop.
func stopIndex(pct int) int {
	for i, stop := range rampStops {
		if stop == pct {
			return i
		}
	}
	return 0
}

// parseStop maps a slider stop index (as a string) to its ramp percentage. ok is
// false when the value is missing, non-numeric, or outside the rampStops range.
func parseStop(s string) (idx, pct int, ok bool) {
	idx, err := strconv.Atoi(s)
	if err != nil || idx < 0 || idx >= len(rampStops) {
		return 0, 0, false
	}
	return idx, rampStops[idx], true
}

// rampViewForStop returns the ramp slider state for an explicitly chosen stop index
// (the slider's own value), clamping any invalid input to the first stop so a
// malformed query never escapes the 25/50/100 set.
func rampViewForStop(stop string) rampView {
	idx, pct, ok := parseStop(stop)
	if !ok {
		idx, pct = 0, rampStops[0]
	}
	return rampView{Pct: pct, StopIdx: idx}
}

// rampViewFor returns the ramp slider state for the selected version, derived
// from its deployment-card status: a Ramping version keeps its in-progress
// percentage, the Current version is 100%, and any other version defaults to
// 25% (the start of a new ramp).
func rampViewFor(state DashboardState, selected string) rampView {
	pct := rampDefaultPct
	for _, c := range state.Versions {
		if c.Version != selected {
			continue
		}
		switch c.Status {
		case StatusRamping:
			pct = c.TrafficPct
		case StatusCurrent:
			pct = rampFullPct
		}
		break
	}
	return rampView{Pct: pct, StopIdx: stopIndex(pct)}
}

// defaultDeploySelection picks the version pre-selected when the modal opens:
// the Ramping version if a ramp is in progress, else the Current version, else
// the first card ("" when there are no versions).
func defaultDeploySelection(state DashboardState) string {
	var current, first string
	for i, c := range state.Versions {
		if i == 0 {
			first = c.Version
		}
		switch c.Status {
		case StatusRamping:
			return c.Version
		case StatusCurrent:
			current = c.Version
		}
	}
	if current != "" {
		return current
	}
	return first
}

// buildDeployModalView builds the Deploy modal view model from the live state.
// The pre-selected version follows defaultDeploySelection, and the ramp slider
// reflects that version's status via rampViewFor.
func buildDeployModalView(state DashboardState) deployModalView {
	selected := defaultDeploySelection(state)
	options := make([]deployVersionOption, 0, len(state.Versions))
	for _, c := range state.Versions {
		options = append(options, deployVersionOption{Version: c.Version, Checked: c.Version == selected})
	}
	return deployModalView{Versions: options, Ramp: rampViewFor(state, selected)}
}

// Renderer renders named dashboard regions to HTML from a DashboardState.
type Renderer struct {
	tmpl *template.Template
}

// NewRenderer parses the embedded templates and wires the helper FuncMap.
func NewRenderer() (*Renderer, error) {
	tmpl := template.New("dashboard").Funcs(funcMap())
	tmpl, err := tmpl.ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard templates: %w", err)
	}
	return &Renderer{tmpl: tmpl}, nil
}

// Region renders a single named region (see sseRegions) to w using the given
// state.
func (r *Renderer) Region(w io.Writer, name string, state DashboardState) error {
	if err := r.tmpl.ExecuteTemplate(w, name, state); err != nil {
		return fmt.Errorf("render region %q: %w", name, err)
	}
	return nil
}

// Toast renders the toast fragment with the given message.
func (r *Renderer) Toast(w io.Writer, message string) error {
	if err := r.tmpl.ExecuteTemplate(w, "toast", toastView{Message: message}); err != nil {
		return fmt.Errorf("render toast: %w", err)
	}
	return nil
}

// DeployModal renders the full Deploy modal fragment for the given view.
func (r *Renderer) DeployModal(w io.Writer, view deployModalView) error {
	if err := r.tmpl.ExecuteTemplate(w, "deploy-modal", view); err != nil {
		return fmt.Errorf("render deploy modal: %w", err)
	}
	return nil
}

// RollbackModal renders the rollback confirmation modal scrim (no data).
func (r *Renderer) RollbackModal(w io.Writer) error {
	if err := r.tmpl.ExecuteTemplate(w, "rollback-modal", nil); err != nil {
		return fmt.Errorf("render rollback modal: %w", err)
	}
	return nil
}

// DeployRamp renders just the ramp section, for re-rendering on version change.
func (r *Renderer) DeployRamp(w io.Writer, view rampView) error {
	if err := r.tmpl.ExecuteTemplate(w, "deploy-ramp", view); err != nil {
		return fmt.Errorf("render deploy ramp: %w", err)
	}
	return nil
}

// funcMap registers the template rendering helpers.
func funcMap() template.FuncMap {
	return template.FuncMap{
		"versionClass":        versionClass,
		"elapsed":             formatElapsed,
		"stepNodes":           stepNodes,
		"pizzaIcon":           pizzaIcon,
		"stepperStyle":        stepperStyle,
		"barWidth":            func(pct int) int { return max(0, min(100, pct)) },
		"hasRamping":          hasRamping,
		"hasMultipleVersions": hasMultipleVersions,
	}
}

// hasRamping reports whether any version card is currently ramping. The controls
// template uses it to enable the Rollback button only while a ramp is in flight.
func hasRamping(versions []VersionCard) bool {
	for _, c := range versions {
		if c.Status == StatusRamping {
			return true
		}
	}
	return false
}

// hasMultipleVersions reports whether at least two worker versions are known.
// The controls template uses it to disable the Deploy button when only one
// version exists (deploying/ramping needs a second version to target).
func hasMultipleVersions(versions []VersionCard) bool {
	return len(versions) > 1
}

// versionClass maps a friendly version to its badge color class (b-v1/b-v2/b-v3),
// defaulting to b-v1.
func versionClass(version string) string {
	switch version {
	case "v2":
		return "b-v2"
	case "v3":
		return "b-v3"
	default:
		return "b-v1"
	}
}

// formatElapsed renders seconds as m:ss.
func formatElapsed(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// pizzaIcon returns the sprite symbol id of the pizza's illustration, falling
// back to the Margherita for an unknown or empty name.
func pizzaIcon(name string) string {
	if id, ok := pizzaIcons[name]; ok {
		return id
	}
	return defaultPizzaIcon
}

// stepIcon returns the sprite symbol id of the step's icon, falling back to a
// generic dot for an unknown label.
func stepIcon(label pizza.StepLabel) string {
	if id, ok := stepIcons[label]; ok {
		return id
	}
	return defaultStepIcon
}

// stepNodes computes the per-node stepper state for an order: the done/current/
// error class and the step icon.
func stepNodes(o Order) []stepNode {
	nodes := make([]stepNode, len(o.Steps))
	for i, label := range o.Steps {
		n := stepNode{Icon: stepIcon(label), Label: string(label)}
		switch {
		case o.Done || i < o.CurrentStep:
			n.Class = "done"
		case i == o.CurrentStep:
			if o.Failing {
				n.Class = "err"
			} else {
				n.Class = "cur"
			}
		}
		nodes[i] = n
	}
	return nodes
}

// stepperStyle returns the inline style that drives the stepper's progress
// fill: a "--fill:N" custom property where N is the percentage of connector
// track that is complete (0 when the order has fewer than two steps).
func stepperStyle(o Order) template.CSS {
	return template.CSS(fmt.Sprintf("--fill:%d", stepperFillPct(o))) //nolint:gosec // value is an integer, not user input
}

// stepperFillPct is the percentage of the connector track to paint green:
// 100% when the order is done, otherwise CurrentStep/(steps-1).
func stepperFillPct(o Order) int {
	n := len(o.Steps)
	if n < 2 {
		return 0
	}
	filled := o.CurrentStep
	if o.Done {
		filled = n - 1
	}
	filled = max(0, min(n-1, filled))
	return filled * 100 / (n - 1)
}
