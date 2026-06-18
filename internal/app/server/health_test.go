package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteReadiness(t *testing.T) {
	okCheck := func(context.Context) error { return nil }
	failCheck := func(context.Context) error { return errors.New("dial tcp 10.0.70.104:6432: connection refused") }

	tests := map[string]struct {
		checks         map[string]func(context.Context) error
		wantStatus     int
		wantReady      bool
		wantStatuses   map[string]string
		wantNoErrorStr bool // body must never carry the underlying error string
	}{
		"all healthy": {
			checks:       map[string]func(context.Context) error{"redis": okCheck, "nats": okCheck},
			wantStatus:   http.StatusOK,
			wantReady:    true,
			wantStatuses: map[string]string{"redis": "ok", "nats": "ok"},
		},
		"one dependency down": {
			checks:         map[string]func(context.Context) error{"redis": okCheck, "postgres_writer": failCheck},
			wantStatus:     http.StatusServiceUnavailable,
			wantReady:      false,
			wantStatuses:   map[string]string{"redis": "ok", "postgres_writer": "unavailable"},
			wantNoErrorStr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeReadiness(context.Background(), rec, tc.checks)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			var body struct {
				Ready        bool              `json:"ready"`
				Dependencies map[string]string `json:"dependencies"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Ready != tc.wantReady {
				t.Errorf("ready = %v, want %v", body.Ready, tc.wantReady)
			}
			for dep, want := range tc.wantStatuses {
				if got := body.Dependencies[dep]; got != want {
					t.Errorf("dependencies[%q] = %q, want %q", dep, got, want)
				}
			}
			// Exhaustiveness: every check must report exactly once. A subset
			// assertion would miss a dependency silently dropped from the body
			// (e.g. a check that returns before recording its status), which
			// would mean a backend is no longer probed while /readyz still 200s.
			if len(body.Dependencies) != len(tc.checks) {
				t.Errorf("dependencies count = %d, want %d (a check was dropped or duplicated)", len(body.Dependencies), len(tc.checks))
			}
			if tc.wantNoErrorStr && bodyLeaksInternals(rec.Body.String()) {
				t.Errorf("response body leaked internal error detail: %s", rec.Body.String())
			}
		})
	}
}

// TestWriteReadinessTimeout pins the core reason the probe fans out concurrently:
// a dependency that blocks until the deadline must not hang the probe or starve a
// healthy dependency running alongside it.
func TestWriteReadinessTimeout(t *testing.T) {
	hangCheck := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	okCheck := func(context.Context) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	ready, failures := writeReadiness(ctx, rec, map[string]func(context.Context) error{
		"slow":  hangCheck,
		"redis": okCheck,
	})

	if ready {
		t.Error("ready = true, want false when a dependency exceeds the deadline")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body struct {
		Ready        bool              `json:"ready"`
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body.Dependencies["slow"]; got != "unavailable" {
		t.Errorf("dependencies[slow] = %q, want unavailable", got)
	}
	if got := body.Dependencies["redis"]; got != "ok" {
		t.Errorf("dependencies[redis] = %q, want ok — healthy dep was starved by the slow one", got)
	}
	if _, ok := failures["slow"]; !ok {
		t.Error("failures must record the timed-out dependency for the operator log")
	}
}

// TestRecordReadiness covers the consecutive-failure state machine: failures
// increment the streak, crossing the threshold takes the escalation branch
// without panicking (RecordError is a no-op absent an active span), and a single
// healthy probe resets the streak. It does not assert that telemetry.RecordError
// fired — that would need a telemetry seam this package doesn't expose.
func TestRecordReadiness(t *testing.T) {
	d := &deps{}
	failures := map[string]string{"redis": "connection refused"}

	for i := 1; i < readinessFailureAlertThreshold; i++ {
		d.recordReadiness(context.Background(), false, failures)
		if got := d.readyFailures.Load(); got != int64(i) {
			t.Fatalf("after %d consecutive failures, counter = %d, want %d", i, got, i)
		}
	}

	// Crossing into the escalation branch must still just count.
	d.recordReadiness(context.Background(), false, failures)
	if got := d.readyFailures.Load(); got != readinessFailureAlertThreshold {
		t.Fatalf("counter = %d, want %d at the escalation threshold", got, readinessFailureAlertThreshold)
	}

	// The streak keeps climbing past the threshold: escalation persists for the
	// duration of the outage (>= threshold), it does not fire once and stall.
	d.recordReadiness(context.Background(), false, failures)
	if got := d.readyFailures.Load(); got != readinessFailureAlertThreshold+1 {
		t.Fatalf("counter = %d, want %d past the escalation threshold", got, readinessFailureAlertThreshold+1)
	}

	// A single healthy probe resets the streak.
	d.recordReadiness(context.Background(), true, nil)
	if got := d.readyFailures.Load(); got != 0 {
		t.Fatalf("counter after a healthy probe = %d, want 0", got)
	}
}

// bodyLeaksInternals guards the contract that /readyz never puts raw dependency
// error strings (hosts, ports) on the wire.
func bodyLeaksInternals(body string) bool {
	for _, leak := range []string{"10.0.70", "connection refused", "dial tcp"} {
		if strings.Contains(body, leak) {
			return true
		}
	}
	return false
}
