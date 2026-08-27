// Package ai wires the dashboard assistant role: a Connect server exposing
// ai.dashboards.v1.DashboardAssistantService with exactly two external
// dependencies — Redis (conversation history + debug traces) and the main pug
// API (insights callback with the caller's forwarded JWT). No Postgres, no
// ClickHouse, no NATS. See docs/architecture/assistant.md.
package ai

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"connectrpc.com/validate"
	"github.com/sethvargo/go-envconfig"
	"golang.org/x/net/http2"

	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/core/assistant"
	pugredis "github.com/pug-sh/pug/internal/deps/redis"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1/aidashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

// insightsCallTimeout bounds one insight tool call — generous because a cold
// ClickHouse query over a wide window is legitimately slow, but finite.
const insightsCallTimeout = 60 * time.Second

type deps struct {
	cfg             config
	closeOtel       func(context.Context) error
	otelInterceptor *otelconnect.Interceptor
	redis           *pugredis.Client
	svc             *assistant.Service
}

// close shuts down all deps. OTel must shut down last — it owns the slog
// backend, so earlier components' shutdown logs are still captured.
// Cancellation is stripped from ctx so cleanup isn't aborted by a cancelled
// signal context.
func (d *deps) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if d.redis != nil {
		d.redis.Close(ctx)
	}
	if d.closeOtel != nil {
		if err := d.closeOtel(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to shutdown telemetry", slogx.Error(err)) // puglint:exempt — nothing left to record it on
		}
	}
}

func Run(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return start(ctx, d)
}

// newDeps mirrors server.newDeps: ordered construction with a rollback closers
// stack. Any misconfiguration (bad MODEL_AGENT, missing key env, missing base
// URL, unreachable Redis) fails boot — an ai Deployment exists only to serve,
// so it crash-loops loudly instead of idling half-configured.
func newDeps(ctx context.Context) (*deps, error) {
	var closers []func()
	success := false
	defer func() {
		if !success {
			for _, closer := range slices.Backward(closers) {
				closer()
			}
		}
	}()

	otelInterceptor, closeOtel, err := telemetry.NewOtelInterceptor(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize telemetry", slogx.Error(err)) // puglint:exempt — nothing to record it on yet
		return nil, err
	}
	closers = append(closers, func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := closeOtel(rollbackCtx); err != nil {
			slog.ErrorContext(rollbackCtx, "failed to close otel during rollback", slogx.Error(err)) // puglint:exempt — nothing left to record it on
		}
	})

	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}

	// Pure config parsing before any network dial, so a misconfigured
	// MODEL_AGENT fails fast even when Redis is also unreachable.
	route, err := assistant.RouteForStage("agent")
	if err != nil {
		return nil, err
	}
	model, callOpts, err := assistant.NewModel(route)
	if err != nil {
		return nil, err
	}
	modelDesc, err := assistant.DescribeStageModels()
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "assistant models resolved", slog.String("stages", modelDesc))

	var redisCfg pugredis.Config
	if err := envconfig.Process(ctx, &redisCfg); err != nil {
		return nil, err
	}
	redisClient, err := pugredis.NewFromConfig(ctx, &redisCfg)
	if err != nil {
		return nil, err
	}
	closers = append(closers, func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		redisClient.Close(rollbackCtx)
	})

	// The turn's context carries no deadline, so without this a hung upstream
	// holds the turn open until the caller disconnects.
	insightsClient := insightsv1connect.NewInsightsServiceClient(
		&http.Client{Timeout: insightsCallTimeout}, cfg.APIBaseURL)

	success = true
	return &deps{
		cfg:             cfg,
		closeOtel:       closeOtel,
		otelInterceptor: otelInterceptor,
		redis:           redisClient,
		svc:             assistant.NewService(redisClient.Unwrap(), insightsClient, model, callOpts, modelDesc),
	}, nil
}

// buildMux assembles the ai role's HTTP surface. Shared by start() and the
// handler tests so the endpoint cannot be wired one way in tests and another
// in production.
//
// The interceptor chain is the main server's minus Principal/Authz: this
// service has its own auth boundary (WithAssistantAuth) and is deliberately
// NOT in the server's authz registry (those contract checks cover only the
// server's mux). NEVER add validate.WithValidateResponses() here — flagged
// TileOps are deliberately invalid protos the client must receive.
func buildMux(
	ctx context.Context,
	svc *assistant.Service,
	jwtKey []byte,
	corsOrigins []string,
	otelInterceptor *otelconnect.Interceptor,
	readyPing func(context.Context) error,
) *http.ServeMux {
	handlerOpts := connect.WithHandlerOptions(
		connect.WithInterceptors(
			pogrpc.CorrelationInterceptor(),
			otelInterceptor,
			pogrpc.LoggingInterceptor(),
			pogrpc.ErrorInterceptor(),
			validate.NewInterceptor(validate.WithoutErrorDetails()),
		),
		connect.WithRecover(pogrpc.RecoverHandlerPanic),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := readyPing(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("redis unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	path, connectHandler := aidashboardsv1connect.NewDashboardAssistantServiceHandler(&handler{svc: svc}, handlerOpts)
	middleware := authn.NewMiddleware(WithAssistantAuth(jwtKey))
	mux.Handle(path, pogrpc.WithCORS(ctx, corsOrigins, middleware.Wrap(connectHandler)))

	return mux
}

func start(ctx context.Context, d *deps) error {
	mux := buildMux(ctx, d.svc, []byte(d.cfg.JWTKey), strings.Split(d.cfg.CORSOrigins, ","), d.otelInterceptor, d.redis.Ping)

	// ReadTimeout/WriteTimeout stay unset: Turn is a long-lived server stream and
	// both would truncate a turn mid-flight. ReadHeaderTimeout and IdleTimeout
	// bound a half-open or idle connection without touching an active stream.
	server := &http.Server{
		Addr:              ":" + d.cfg.Port,
		Handler:           pogrpc.WithCorrelationID(mux),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "ai server shutdown error", slogx.Error(err)) // puglint:exempt — no span at shutdown
		}
	}()

	slog.InfoContext(ctx, "Starting ai service", slog.String("addr", server.Addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.ErrorContext(ctx, "failed to serve", slogx.Error(err)) // puglint:exempt — no span at startup
		return err
	}
	return nil
}

// DevStatus reports whether `pug dev` should run the ai service and a status
// line for the dev banner. Exported for cmd/pug. It checks everything Run
// would fail-boot on that a plain dev .env might not have — MODEL_AGENT (and
// its provider key env) plus PUG_API_BASE_URL — so an unconfigured dev
// environment prints a hint instead of crashing the whole dev process.
// (REDIS_URL and PUG_JWT_SECRET_KEY are in every dev .env already.)
func DevStatus(_ context.Context) (bool, string) {
	if os.Getenv("MODEL_AGENT") == "" {
		return false, "disabled (set MODEL_AGENT to enable)"
	}
	route, err := assistant.RouteForStage("agent")
	if err != nil {
		return false, fmt.Sprintf("disabled (%v)", err)
	}
	if _, _, err := assistant.NewModel(route); err != nil {
		return false, fmt.Sprintf("disabled (%v)", err)
	}
	if os.Getenv("PUG_API_BASE_URL") == "" {
		return false, "disabled (missing PUG_API_BASE_URL)"
	}
	return true, fmt.Sprintf("agent=%s:%s", route.Provider, route.Model)
}
