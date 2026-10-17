package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	tests := map[string]struct {
		natsClosed bool
		draining   bool
		wantLive   int
		wantReady  int
	}{
		"healthy":     {wantLive: http.StatusOK, wantReady: http.StatusOK},
		"nats closed": {natsClosed: true, wantLive: http.StatusServiceUnavailable, wantReady: http.StatusOK},
		"draining":    {draining: true, wantLive: http.StatusOK, wantReady: http.StatusServiceUnavailable},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := &health{natsClosed: func() bool { return tc.natsClosed }}
			h.draining.Store(tc.draining)

			rec := httptest.NewRecorder()
			h.live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tc.wantLive {
				t.Errorf("/healthz = %d, want %d", rec.Code, tc.wantLive)
			}

			rec = httptest.NewRecorder()
			h.ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.wantReady {
				t.Errorf("/readyz = %d, want %d", rec.Code, tc.wantReady)
			}
		})
	}
}
