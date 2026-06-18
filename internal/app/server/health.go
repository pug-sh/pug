package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pug-sh/pug/internal/deps/telemetry"
)

var errNATSNotConnected = errors.New("nats connection not established")

// readinessTimeout bounds the total time spent pinging dependencies so a hung
// dependency can't make the probe itself hang. Keep it ≤ the readiness probe's
// timeoutSeconds in the Deployment manifest (pug-sh/gitops) so the two deadlines
// don't drift apart.
const readinessTimeout = 2 * time.Second

// readinessFailureAlertThreshold is the number of consecutive failing probes at
// which a readiness failure stops being treated as a transient blip and is
// escalated to ERROR-severity logging. Wall-clock to escalation is this count
// times the probe's periodSeconds in that Deployment manifest.
const readinessFailureAlertThreshold = 5

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

	ready, failures := writeReadiness(ctx, w, map[string]func(context.Context) error{
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

	d.recordReadiness(ctx, ready, failures)
}

// writeReadiness runs every dependency check and writes the probe response: 200
// with all deps "ok", or 503 if any fails. Checks run concurrently against a
// single shared deadline (the ctx passed in), so total probe latency is bounded
// by the slowest dependency rather than the sum of all of them — a slow check
// can't serialize ahead of the others and starve them of their budget. It returns
// the overall readiness and the per-dependency failures (the underlying error
// strings, operator-only) so the caller can log/escalate without re-running the
// checks.
func writeReadiness(ctx context.Context, w http.ResponseWriter, checks map[string]func(context.Context) error) (bool, map[string]string) {
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
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":        ready,
		"dependencies": statuses,
	})

	return ready, failures
}

// recordReadiness logs the outcome of a readiness probe and escalates a sustained
// outage. A one-off failure is a routine operational signal (WARN) so a transient
// dependency blip doesn't pollute error telemetry. Once failures persist for
// readinessFailureAlertThreshold consecutive probes the outage is no longer a
// blip: it escalates to slog.ErrorContext — exported at ERROR severity via the
// otelslog bridge — so a fleet stuck NotReady can't drain from its Service with
// only WARN noise to show for it. It stays escalated for the duration (not just
// at the transition) so the signal can't age out while the outage continues; a
// single healthy probe resets the counter.
//
// RecordError is paired with the ErrorContext per the house convention, but
// /readyz is mounted outside the RPC interceptor chain and so runs under no OTel
// span: RecordError is a no-op on this route today (the exported ERROR log is the
// live signal) and only starts contributing if the route is ever given a span.
// Its message omits the failing dependency names so errors would group cleanly;
// the per-dependency detail rides on the structured log instead.
func (d *deps) recordReadiness(ctx context.Context, ready bool, failures map[string]string) {
	if ready {
		d.readyFailures.Store(0)
		return
	}

	n := d.readyFailures.Add(1)
	if n >= readinessFailureAlertThreshold {
		err := fmt.Errorf("readiness probe failing for %d consecutive checks", n)
		telemetry.RecordError(ctx, err)
		slog.ErrorContext(ctx, "readiness probe failing persistently",
			slog.Int64("consecutive_failures", n), slog.Any("dependencies", failures))
		return
	}

	slog.WarnContext(ctx, "readiness probe failed", slog.Any("dependencies", failures))
}
