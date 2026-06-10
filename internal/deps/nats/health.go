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

// HealthAddrEnv configures the worker liveness endpoint. Unset → DefaultHealthAddr;
// set to "off" → disabled; otherwise a listen address like ":8090". A worker
// process serves GET /healthz, returning 200 while every worker in the process
// reports healthy and 503 (with the failing worker's reason) otherwise, so an
// orchestrator can liveness-probe the process and restart a wedged worker.
//
// `pug dev` forces this to "off": it runs every worker in one local process
// where liveness probing isn't needed, which also sidesteps a single-process
// bind. It is exported so that command can set it.
const HealthAddrEnv = "PUG_WORKER_HEALTH_ADDR"

// DefaultHealthAddr is the address the health endpoint binds when
// PUG_WORKER_HEALTH_ADDR is unset.
const DefaultHealthAddr = ":8090"

var (
	healthMu         sync.Mutex
	healthWorkers    []Worker
	healthServerOnce sync.Once
)

// registerHealth adds w to the process-wide set consulted by /healthz and starts
// the health server exactly once per process. Several workers sharing a process
// (e.g. `pug dev`) aggregate into a single endpoint: the first registration binds
// the listener, the rest just join the set.
func registerHealth(ctx context.Context, w Worker) {
	healthMu.Lock()
	healthWorkers = append(healthWorkers, w)
	healthMu.Unlock()
	healthServerOnce.Do(func() { startHealthServer(ctx) })
}

func startHealthServer(ctx context.Context) {
	addr := strings.TrimSpace(os.Getenv(HealthAddrEnv))
	if addr == "" {
		addr = DefaultHealthAddr
	}
	if strings.EqualFold(addr, "off") {
		slog.InfoContext(ctx, "worker health endpoint disabled", slog.String("env", HealthAddrEnv))
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(wr http.ResponseWriter, _ *http.Request) {
		if err := healthSnapshot(); err != nil {
			wr.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(wr, err.Error())
			return
		}
		fmt.Fprintln(wr, "ok")
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.WarnContext(shutdownCtx, "worker health server shutdown error", slogx.Error(err))
		}
	}()

	go func() {
		slog.InfoContext(ctx, "worker health endpoint listening",
			slog.String("addr", addr), slog.String("path", "/healthz"))
		// Bind failure is non-fatal: on a host running several worker processes
		// the first to start owns the port and the rest log here and keep
		// processing. Health visibility is degraded for those, not correctness.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.ErrorContext(ctx, "worker health server failed to serve",
				slog.String("addr", addr), slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()
}

// healthSnapshot returns the first unhealthy worker's error, or nil if every
// worker registered in this process is healthy.
func healthSnapshot() error {
	healthMu.Lock()
	defer healthMu.Unlock()
	for _, w := range healthWorkers {
		if ok, err := w.HealthCheck(); !ok {
			return err
		}
	}
	return nil
}
