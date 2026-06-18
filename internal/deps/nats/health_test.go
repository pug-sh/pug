package nats

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resolveHealthAddr is the single decision point for whether the endpoints bind
// and where: unset/blank → default address, "off" (any case) → disabled, anything
// else → that address, trimmed. The "off" path is what `pug dev` relies on to keep
// all-in-one-process runs from binding a listener.
func TestResolveHealthAddr(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		wantAddr string
		wantOn   bool
	}{
		{"blank uses default", "", DefaultHealthAddr, true},
		{"off disables", "off", "", false},
		{"OFF is case-insensitive", "OFF", "", false},
		{"explicit address", ":9000", ":9000", true},
		{"address is trimmed", "  :9000  ", ":9000", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(HealthAddrEnv, tt.env)
			addr, on := resolveHealthAddr()
			if addr != tt.wantAddr || on != tt.wantOn {
				t.Fatalf("resolveHealthAddr() = (%q, %v), want (%q, %v)", addr, on, tt.wantAddr, tt.wantOn)
			}
		})
	}
}

// healthHandler maps a snapshot function to the HTTP contract an orchestrator
// probes: no failing worker → 200, a failing worker → 503 with its reason in the
// body. Inverting this mapping would make a wedged worker report healthy and
// never get restarted, so the 200/503 contract is pinned directly.
func TestHealthHandler(t *testing.T) {
	t.Run("no failure returns 200 ok", func(t *testing.T) {
		rec := httptest.NewRecorder()
		healthHandler(func() error { return nil })(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if strings.TrimSpace(rec.Body.String()) != "ok" {
			t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
		}
	})

	t.Run("failure returns 503 with reason", func(t *testing.T) {
		rec := httptest.NewRecorder()
		snapshot := func() error { return errors.New("worker s/c is unhealthy") }
		healthHandler(snapshot)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		if !strings.Contains(rec.Body.String(), "worker s/c is unhealthy") {
			t.Fatalf("body = %q, want it to contain the failure reason", rec.Body.String())
		}
	})
}

// The mux must route /healthz to the liveness snapshot and /readyz to the
// readiness snapshot — transposing the two would make a wedged worker pass
// readiness or a disconnected worker fail liveness. A worker that is healthy
// (liveness OK) but disconnected (readiness fails) distinguishes the two paths:
// /healthz must be 200 while /readyz is 503.
func TestHealthRegistryRoutesPathsToDistinctSnapshots(t *testing.T) {
	w := &natsWorker{
		config:    WorkerConfig{StreamName: "s", ConsumerName: "c"},
		connected: func() bool { return false }, // healthy but not ready
	}
	w.healthy.Store(true)
	w.started.Store(true)
	handler := (&healthRegistry{workers: []Worker{w}}).handler()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}
