package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPauseRoutes(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		startPaused bool
		wantPaused  bool
	}{
		{"PUT pauses", http.MethodPut, false, true},
		{"PUT while paused stays paused", http.MethodPut, true, true},
		{"DELETE resumes", http.MethodDelete, true, false},
		{"DELETE while running stays running", http.MethodDelete, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			sw := &fakeOrderSwitch{paused: tt.startPaused}
			s.orderSwitch = sw
			rec := httptest.NewRecorder()

			s.Routes().ServeHTTP(rec, httptest.NewRequest(tt.method, "/pause", nil))

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			// Empty 200, not 204: the UI refresh comes from the SSE frame.
			if rec.Body.Len() != 0 {
				t.Errorf("body should be empty, got %q", rec.Body.String())
			}
			if sw.paused != tt.wantPaused {
				t.Errorf("paused = %v, want %v", sw.paused, tt.wantPaused)
			}
		})
	}
}

// TestPauseRepublishesLatestState checks that toggling pushes the latest state
// to subscribers right away, rather than waiting for the next poll tick.
func TestPauseRepublishesLatestState(t *testing.T) {
	s := newTestServer(t)
	s.hub.Publish(versionsState("v1", "v1"))
	frames, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()
	<-frames // drain the frame pre-loaded by Subscribe

	s.Routes().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/pause", nil))

	select {
	case state := <-frames:
		if len(state.Versions) != 1 || state.Versions[0].Version != "v1" {
			t.Errorf("republished state = %+v, want the latest state", state)
		}
	default:
		t.Fatal("PUT /pause did not republish the latest state")
	}
}

func TestWriteFrameStampsPausedState(t *testing.T) {
	tests := []struct {
		name   string
		paused bool
		want   string
	}{
		{"running", false, "Pause orders"},
		{"paused", true, "Resume orders"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			s.orderSwitch = &fakeOrderSwitch{paused: tt.paused}
			rec := httptest.NewRecorder()

			if !s.writeFrame(rec, DashboardState{}) {
				t.Fatal("writeFrame reported a write failure")
			}

			out := rec.Body.String()
			for _, w := range []string{"event: publishing\n", tt.want} {
				if !strings.Contains(out, w) {
					t.Errorf("frame missing %q\n--- frame ---\n%s", w, out)
				}
			}
		})
	}
}
