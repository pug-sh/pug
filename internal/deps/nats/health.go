package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// HealthAddrEnv configures the worker health/readiness endpoints. Unset →
// DefaultHealthAddr; set to "off" → disabled; otherwise a listen address like
// ":8090". A worker process serves two probes an orchestrator can use:
//   - GET /healthz (liveness): 200 while every worker's consume loop is running,
//     503 (with the failing worker's reason) otherwise → restart a wedged worker.
//   - GET /readyz (readiness): 200 while every worker has started consuming and
//     its NATS connection is live, 503 otherwise → gate rollouts / rotation.
//
// `pug dev` forces this to "off": it runs every worker in one local process where
// probing isn't needed. It is exported so that command can set it.
const HealthAddrEnv = "PUG_WORKER_HEALTH_ADDR"

// DefaultHealthAddr is the address the endpoints bind when PUG_WORKER_HEALTH_ADDR
// is unset.
const DefaultHealthAddr = ":8090"

// healthRegistry is the process-wide set of workers exposed on the health and
// readiness endpoints. Workers sharing a process aggregate into one server: the
// first registration binds the listener, the rest just join the set. workerHealth
// is the production singleton; tests construct their own instance for isolation.
type healthRegistry struct {
	mu         sync.Mutex
	workers    []Worker
	serverOnce sync.Once
}

var workerHealth = &healthRegistry{}

// registerHealth adds w to the process-wide registry and starts the health server
// exactly once per process.
func registerHealth(ctx context.Context, w Worker) {
	workerHealth.register(ctx, w)
}

func (r *healthRegistry) register(ctx context.Context, w Worker) {
	r.mu.Lock()
	r.workers = append(r.workers, w)
	r.mu.Unlock()
	r.serverOnce.Do(func() { r.startServer(ctx) })
}

// resolveHealthAddr decides, from HealthAddrEnv, whether the endpoints bind and
// at what address: unset/blank → DefaultHealthAddr; "off" (any case) → disabled;
// otherwise the configured address, whitespace-trimmed.
func resolveHealthAddr() (addr string, enabled bool) {
	addr = strings.TrimSpace(os.Getenv(HealthAddrEnv))
	if addr == "" {
		return DefaultHealthAddr, true
	}
	if strings.EqualFold(addr, "off") {
		return "", false
	}
	return addr, true
}

func (r *healthRegistry) startServer(ctx context.Context) {
	addr, enabled := resolveHealthAddr()
	if !enabled {
		slog.InfoContext(ctx, "worker health endpoint disabled", slog.String("env", HealthAddrEnv))
		return
	}

	srv := &http.Server{Addr: addr, Handler: r.handler(), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.WarnContext(shutdownCtx, "worker health server shutdown error", slogx.Error(err))
		}
	}()

	go func() {
		slog.InfoContext(ctx, "worker health endpoints listening",
			slog.String("addr", addr), slog.String("liveness", "/healthz"), slog.String("readiness", "/readyz"))
		// Bind failure is non-fatal: a port collision (a stale worker process, or a
		// misconfigured PUG_WORKER_HEALTH_ADDR — each standalone worker binary needs
		// its own port) must not take down message processing. Correctness is
		// unaffected; only probe visibility is lost, so it is logged at Error and
		// recorded to make the lost visibility alertable rather than silent.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.ErrorContext(ctx, "worker health server failed to serve",
				slog.String("addr", addr), slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()
}

// handler builds the mux serving /healthz (liveness) and /readyz (readiness).
// Extracted from startServer so the path→snapshot routing is testable without
// binding a socket.
func (r *healthRegistry) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", healthHandler(r.livenessSnapshot))
	mux.Handle("/readyz", healthHandler(r.readinessSnapshot))
	return mux
}

// healthHandler turns a snapshot function into an HTTP handler: 200 + "ok" when
// the snapshot reports no failing worker, or 503 + the failing worker's reason.
func healthHandler(snapshot func() error) http.HandlerFunc {
	return func(wr http.ResponseWriter, _ *http.Request) {
		if err := snapshot(); err != nil {
			wr.WriteHeader(http.StatusServiceUnavailable)
			// Write errors here mean the prober hung up mid-response; the status
			// line is already sent and there is nothing useful to do, so ignore.
			_, _ = fmt.Fprintln(wr, err.Error())
			return
		}
		_, _ = fmt.Fprintln(wr, "ok")
	}
}

// snapshot returns the first worker for which check reports failure, or nil if
// every registered worker passes.
func (r *healthRegistry) snapshot(check func(Worker) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.workers {
		if ok, err := check(w); !ok {
			return err
		}
	}
	return nil
}

// livenessSnapshot backs /healthz; readinessSnapshot backs /readyz.
func (r *healthRegistry) livenessSnapshot() error  { return r.snapshot(Worker.HealthCheck) }
func (r *healthRegistry) readinessSnapshot() error { return r.snapshot(Worker.Ready) }
