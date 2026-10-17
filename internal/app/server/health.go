package server

import (
	"net/http"
	"sync/atomic"
)

// health backs the Kubernetes probes. Readiness ignores Postgres, ClickHouse, Redis
// and NATS: every replica shares them, so an outage would pull every pod at once.
type health struct {
	natsClosed func() bool
	draining   atomic.Bool
}

// live fails once this pod's NATS connection has closed for good, which nats.go
// never undoes, so the kubelet restarts the pod. A reconnecting connection stays live.
func (h *health) live(w http.ResponseWriter, _ *http.Request) {
	if h.natsClosed() {
		http.Error(w, "nats closed", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func (h *health) ready(w http.ResponseWriter, _ *http.Request) {
	if h.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}
