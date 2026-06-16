package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

var errNATSNotConnected = errors.New("nats connection not established")

// readinessTimeout bounds the total time spent pinging dependencies so a hung
// dependency can't make the probe itself hang.
const readinessTimeout = 2 * time.Second

// livenessHandler answers Kubernetes liveness probes. It reports only that the
// process is up and serving — it deliberately does NOT check dependencies, so a
// transient dependency blip can't trigger a restart cascade across replicas.
func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readinessHandler answers Kubernetes readiness probes by pinging every backing
// dependency. A failure returns 503 so the pod is pulled from the Service
// endpoints (stops receiving traffic) without being killed.
func (d *deps) readinessHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	writeReadiness(ctx, w, map[string]func(context.Context) error{
		"postgres_writer": d.pgW.Ping,
		"postgres_reader": d.pgRo.Ping,
		"clickhouse":      d.ch.Ping,
		"redis":           d.redis.Ping,
		"nats": func(context.Context) error {
			if !d.nats.IsConnected() {
				return errNATSNotConnected
			}
			return nil
		},
	})
}

// writeReadiness runs every dependency check and writes the probe response: 200
// with all deps "ok", or 503 if any fails. Checks run concurrently so each gets
// the full timeout independently — one slow dependency must not consume the
// budget of the others and make healthy deps report a misleading deadline error.
func writeReadiness(ctx context.Context, w http.ResponseWriter, checks map[string]func(context.Context) error) {
	var mu sync.Mutex
	// statuses is the public response body ("ok"/"unavailable" only); failures
	// holds the underlying errors for the operator log. The error strings are
	// kept off the wire so /readyz can't leak internal hosts/ports to callers.
	statuses := make(map[string]string, len(checks))
	failures := make(map[string]string)
	ready := true

	var wg sync.WaitGroup
	for name, check := range checks {
		wg.Go(func() {
			err := check(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				ready = false
				statuses[name] = "unavailable"
				failures[name] = err.Error()
				return
			}
			statuses[name] = "ok"
		})
	}
	wg.Wait()

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
		// Routine operational signal, not an exception — log without
		// telemetry.RecordError to avoid polluting error metrics on every
		// dependency blip.
		slog.WarnContext(ctx, "readiness probe failed", slog.Any("dependencies", failures))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":        ready,
		"dependencies": statuses,
	})
}
