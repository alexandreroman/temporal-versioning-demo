package dashboard

import (
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/alexandreroman/temporal-versioning-demo/frontend"
	"github.com/alexandreroman/temporal-versioning-demo/internal/pizza"
)

func TestPizzaIcon(t *testing.T) {
	tests := []struct {
		name  string
		pizza string
		want  string
	}{
		{"menu item", "Margherita", "pizza-margherita"},
		{"multi-word menu item", "Veggie Supreme", "pizza-veggie-supreme"},
		{"unknown name falls back", "Calzone", defaultPizzaIcon},
		{"empty name falls back", "", defaultPizzaIcon},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pizzaIcon(tt.pizza); got != tt.want {
				t.Errorf("pizzaIcon(%q) = %q, want %q", tt.pizza, got, tt.want)
			}
		})
	}
}

// TestPizzaIconCoversMenu guards against a menu item silently falling back to
// the default illustration: every pizza must have its own symbol.
func TestPizzaIconCoversMenu(t *testing.T) {
	seen := map[string]string{}
	for _, name := range pizza.Menu {
		id, ok := pizzaIcons[name]
		if !ok {
			t.Errorf("menu pizza %q has no illustration in pizzaIcons", name)
			continue
		}
		if other, dup := seen[id]; dup {
			t.Errorf("pizzas %q and %q share the illustration %q", other, name, id)
		}
		seen[id] = name
	}
}

func TestStepIcon(t *testing.T) {
	tests := []struct {
		label pizza.StepLabel
		want  string
	}{
		{pizza.StepReceived, "step-new"},
		{pizza.StepCooking, "step-cook"},
		{pizza.StepQualityCheck, "step-qc"},
		{pizza.StepOutForDelivery, "step-out"},
		{pizza.StepDroneDelivery, "step-drone"},
		{pizza.StepDelivered, "step-done"},
		{"Unknown", defaultStepIcon},
		{"", defaultStepIcon},
	}
	for _, tt := range tests {
		t.Run(string(tt.label), func(t *testing.T) {
			if got := stepIcon(tt.label); got != tt.want {
				t.Errorf("stepIcon(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}

func TestStepNodes(t *testing.T) {
	steps := []pizza.StepLabel{
		pizza.StepReceived, pizza.StepCooking, pizza.StepDroneDelivery, pizza.StepDelivered,
	}
	tests := []struct {
		name  string
		order Order
		want  []stepNode
	}{
		{
			name:  "in progress",
			order: Order{Steps: steps, CurrentStep: 1},
			want: []stepNode{
				{Class: "done", Icon: "step-new", Label: "New"},
				{Class: "cur", Icon: "step-cook", Label: "Cook"},
				{Class: "", Icon: "step-drone", Label: "Drone"},
				{Class: "", Icon: "step-done", Label: "Done"},
			},
		},
		{
			name:  "failing on the drone step",
			order: Order{Steps: steps, CurrentStep: 2, Failing: true},
			want: []stepNode{
				{Class: "done", Icon: "step-new", Label: "New"},
				{Class: "done", Icon: "step-cook", Label: "Cook"},
				{Class: "err", Icon: "step-drone", Label: "Drone"},
				{Class: "", Icon: "step-done", Label: "Done"},
			},
		},
		{
			name:  "delivered",
			order: Order{Steps: steps, CurrentStep: 3, Done: true},
			want: []stepNode{
				{Class: "done", Icon: "step-new", Label: "New"},
				{Class: "done", Icon: "step-cook", Label: "Cook"},
				{Class: "done", Icon: "step-drone", Label: "Drone"},
				{Class: "done", Icon: "step-done", Label: "Done"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stepNodes(tt.order); !slices.Equal(got, tt.want) {
				t.Errorf("stepNodes() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSpriteDefinesEveryIcon checks that each symbol id the templates can
// reference exists in the SPA's inline SVG sprite; a missing one would render
// an empty image without any error.
func TestSpriteDefinesEveryIcon(t *testing.T) {
	page, err := fs.ReadFile(frontend.Assets, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(page)

	ids := []string{defaultPizzaIcon, defaultStepIcon, "pizza-box-sleeping"}
	for _, id := range pizzaIcons {
		ids = append(ids, id)
	}
	for _, id := range stepIcons {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if !strings.Contains(html, `<symbol id="`+id+`"`) {
			t.Errorf("index.html sprite has no <symbol id=%q>", id)
		}
	}
}
