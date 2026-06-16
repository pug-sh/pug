package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			if tc.wantNoErrorStr && bodyLeaksInternals(rec.Body.String()) {
				t.Errorf("response body leaked internal error detail: %s", rec.Body.String())
			}
		})
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
