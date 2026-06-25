package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/validate"
	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/customers"
	dashboardsrpc "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/dashboards"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgemailproviders"
	orgsrpc "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/projects"
	"github.com/pug-sh/pug/internal/app/server/rpc/public/auth"
	publicdashboardsrpc "github.com/pug-sh/pug/internal/app/server/rpc/public/dashboards"
	eventsrpc "github.com/pug-sh/pug/internal/app/server/rpc/sdk/events"
	sdkprofilesrpc "github.com/pug-sh/pug/internal/app/server/rpc/sdk/profiles"
	activityrpc "github.com/pug-sh/pug/internal/app/server/rpc/shared/activity"
	"github.com/pug-sh/pug/internal/app/server/rpc/shared/insights"
	sharedprofilesrpc "github.com/pug-sh/pug/internal/app/server/rpc/shared/profiles"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/fallback"
	"github.com/pug-sh/pug/internal/core/email/secret"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1/customersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1/orgemailprovidersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/public/auth/v1/authv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/public/dashboards/v1/publicdashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/pug-sh/pug/internal/useragent"
	"github.com/sethvargo/go-envconfig"
	"golang.org/x/net/http2"
)

func Run(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return start(ctx, d)
}

func start(ctx context.Context, d *deps) error {
	queriesRo := dbread.New(d.pgRo)

	projectsRepo := coreprojects.NewRepo(queriesRo, d.redis.Unwrap())
	projectsSvc := coreprojects.NewService(d.pgRo, d.pgW, projectsRepo)
	dashboardsSvc := coredashboards.NewService(d.pgRo, d.pgW)
	orgsSvc := coreorgs.NewServiceWithRoleCache(d.pgRo, d.pgW, d.nats, d.redis.Unwrap())
	insightsExecutor := coreinsights.NewExecutor(d.ch)
	insightsSvc := coreinsights.NewService(insightsExecutor, d.redis.Unwrap())

	// Interceptor order matters. ErrorInterceptor must wrap validate.NewInterceptor():
	// validate short-circuits on a bad request (returns the error without calling the
	// inner chain), so ErrorInterceptor has to be OUTSIDE it to attach error details
	// (reason + correlation id) to validation failures. It stays inside otel so a span
	// is present for trace_id. CorrelationInterceptor is first so the id is in context
	// for every downstream interceptor and handler. LoggingInterceptor must stay
	// OUTSIDE ErrorInterceptor so it observes the final *connect.Error with its
	// resolved code for client-vs-server log-level classification (isClientError also
	// reads *apperr.Error directly as a safety net, but order keeps that path moot).
	// AuthzInterceptor is innermost — the last gate before the handler — and is the
	// single authorization gate: it enforces the org role recorded in the permission
	// registry for every role-gated RPC (orgsSvc resolves the caller's role), and is
	// a no-op for public/self/SDK procedures and the API-key path. Handlers carry no
	// authorization of their own.
	handlerOpts := connect.WithInterceptors(
		pogrpc.CorrelationInterceptor(),
		d.otelInterceptor,
		pogrpc.LoggingInterceptor(),
		pogrpc.ErrorInterceptor(),
		validate.NewInterceptor(validate.WithoutErrorDetails()),
		pogrpc.PrincipalInterceptor(),
		pogrpc.AuthzInterceptor(d.authz, orgsSvc),
	)

	// Middleware
	// - Dashboard: JWT auth only (for dashboard-only services)
	// - SDK: API key auth (public or private, no JWT fallback) for SDK-only services
	// - Shared: Dual auth - private API key or JWT fallback (for services accessible from both)
	dashboardMW := authn.NewMiddleware(pogrpc.WithJWTAuth(d.jwtKey, queriesRo))
	sdkMW := authn.NewMiddleware(pogrpc.WithSDKAuth(projectsRepo))
	sharedMW := authn.NewMiddleware(pogrpc.WithDualAuth(d.jwtKey, queriesRo, projectsRepo))

	// Handlers — grouped by auth boundary

	// Public
	authServer, err := auth.NewServer(ctx, d.pgRo, d.pgW, d.jwtKey, d.nats)
	if err != nil {
		return fmt.Errorf("auth server: %w", err)
	}
	authPath, authHandler := authv1connect.NewAuthServiceHandler(authServer, handlerOpts)

	// Dashboard
	orgsPath, orgsHandler := orgsv1connect.NewOrgsServiceHandler(
		orgsrpc.NewServer(orgsSvc), handlerOpts)
	projectsPath, projectsHandler := projectsv1connect.NewProjectsServiceHandler(
		projects.NewServer(projectsSvc), handlerOpts)
	dashboardsPath, dashboardsHandler := dashboardsv1connect.NewDashboardsServiceHandler(
		dashboardsrpc.NewServer(dashboardsSvc, insightsExecutor), handlerOpts)
	sharedDashboardsPath, sharedDashboardsHandler := publicdashboardsv1connect.NewSharedDashboardsServiceHandler(
		publicdashboardsrpc.NewServer(dashboardsSvc, insightsExecutor), handlerOpts)

	// Email providers — JWT + admin gate. Cipher and OrgProviderRepo are only
	// present when PUG_EMAIL_PROVIDER_SECRET_KEY is configured; otherwise the
	// handler's requireCipher gate returns CodeFailedPrecondition with a clear
	// "not configured" message and SendTest returns the same on nil mailer.
	var emailKeyCfg struct {
		KeyB64 string `env:"PUG_EMAIL_PROVIDER_SECRET_KEY"`
	}
	if err := envconfig.Process(ctx, &emailKeyCfg); err != nil {
		return err
	}

	var (
		emailCipher  *secret.Cipher
		orgEmailRepo *coreemail.OrgProviderRepo
		emailMailer  *coreemail.Service
	)
	if emailKeyCfg.KeyB64 != "" {
		c, err := secret.NewCipher(emailKeyCfg.KeyB64)
		if err != nil {
			return fmt.Errorf("server: init email cipher: %w", err)
		}
		emailCipher = c

		orgEmailRepo = coreemail.NewOrgProviderRepo(queriesRo, d.redis.Unwrap())

		var emailCfg coreemail.Config
		if err := envconfig.Process(ctx, &emailCfg); err != nil {
			return err
		}
		fallbackProvider, err := fallback.NewProvider(ctx)
		if err != nil {
			return err
		}
		resolver, err := coreemail.NewTenantAwareResolver(orgEmailRepo, emailCipher, fallbackProvider, emailCfg.From, emailCfg.ReplyTo)
		if err != nil {
			return err
		}
		emailMailer, err = coreemail.NewServiceWithResolver(emailCfg, resolver)
		if err != nil {
			return err
		}
	}

	orgEmailProvidersPath, orgEmailProvidersHandler := orgemailprovidersv1connect.NewOrgEmailProvidersServiceHandler(
		orgemailproviders.NewServer(queriesRo, dbwrite.New(d.pgW), emailCipher, orgEmailRepo, emailMailer),
		handlerOpts)

	customersPath, customersHandler := customersv1connect.NewCustomersServiceHandler(
		customers.NewServer(corecustomers.NewService(d.pgW)), handlerOpts)

	// Shared
	insightsPath, insightsHandler := insightsv1connect.NewInsightsServiceHandler(
		insights.NewServer(insightsSvc, insightsExecutor), handlerOpts)
	activityPath, activityHandler := activityv1connect.NewActivityServiceHandler(
		activityrpc.NewServer(d.ch, insightsSvc, dbread.New(d.pgRo)), handlerOpts)
	profilesSvc := coreprofiles.NewService(d.pgW, d.ch, d.nats)
	sharedProfilesPath, sharedProfilesHandler := profilesv1connect.NewProfilesServiceHandler(
		sharedprofilesrpc.NewServer(profilesSvc), handlerOpts)

	// SDK
	sdkProfilesPath, sdkProfilesHandler := sdkprofilesv1connect.NewProfilesSDKServiceHandler(
		sdkprofilesrpc.NewServer(d.nats.GetJetStream()), handlerOpts)
	geoProvider := geo.CloudflareProvider{}
	uaParser, err := useragent.NewParser()
	if err != nil {
		return err
	}
	eventsPath, eventsHandler := eventsv1connect.NewEventsServiceHandler(
		eventsrpc.NewServer(d.nats.GetJetStream(), geoProvider, uaParser), handlerOpts)

	mux := http.NewServeMux()

	// Health probes (no auth, no CORS — infra endpoints). Plain paths do not
	// collide with the RPC routes, which are all /<package>.<Service>/...
	mux.HandleFunc("/healthz", livenessHandler)
	mux.HandleFunc("/readyz", d.readinessHandler)

	// AUTHZ CONTRACT: mount every RPC service through handle(), which records the
	// service name. assertServedServicesMatch (below) then fails startup unless the
	// mounted set exactly equals the authz permission registry — so no RPC service
	// can ship mounted-but-unauthorized (or authorized-but-unmounted) — and
	// AssertRegistryMatchesServedProcedures tightens that to the PROCEDURE level, so
	// a new method on an already-mounted service (invisible to the service-level
	// check) also fails fast. Always mount RPC routes via handle(), never mux.Handle
	// directly.
	mounted := map[string]bool{}
	handle := func(path string, h http.Handler) {
		mounted[strings.Trim(path, "/")] = true
		mux.Handle(path, h)
	}

	// Public (CORS, no auth)
	handle(authPath, pogrpc.WithCORS(d.corsOrigins, authHandler))
	handle(sharedDashboardsPath, pogrpc.WithCORS(d.corsOrigins, sharedDashboardsHandler))

	// Dashboard only (CORS + JWT auth)
	handle(orgsPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(orgsHandler)))
	handle(projectsPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(projectsHandler)))
	handle(dashboardsPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(dashboardsHandler)))
	handle(orgEmailProvidersPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(orgEmailProvidersHandler)))
	handle(customersPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(customersHandler)))

	// Shared: Dashboard + private API key (CORS + dual auth)
	handle(insightsPath, pogrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(insightsHandler)))
	handle(activityPath, pogrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(activityHandler)))
	handle(sharedProfilesPath, pogrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(sharedProfilesHandler)))

	// SDK only (API key auth). CORS is wildcard with credentials disabled because
	// customer sites embedding the SDK have arbitrary origins; auth lives entirely
	// in the x-api-key header, so there are no ambient credentials to protect.
	handle(sdkProfilesPath, pogrpc.WithSDKCORS(sdkMW.Wrap(sdkProfilesHandler)))
	handle(eventsPath, pogrpc.WithSDKCORS(sdkMW.Wrap(eventsHandler)))

	if err := assertServedServicesMatch(mounted); err != nil {
		return err
	}
	// Procedure-level half of the contract: every served RPC method has an authz
	// decision (and no entry is stale). Catches a method added to an already-mounted
	// service, which assertServedServicesMatch (service-level) cannot see.
	if err := pogrpc.AssertRegistryMatchesServedProcedures(); err != nil {
		return err
	}

	// Reflection advertises exactly the authorized services — same source
	// (pogrpc.ServedServiceNames) as the AUTHZ CONTRACT check above.
	reflector := grpcreflect.NewStaticReflector(pogrpc.ServedServiceNames()...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	// WithCorrelationID wraps the whole mux so a correlation id exists before the
	// authn middleware runs on any route — auth rejections happen outside the
	// Connect interceptor chain, and this lets them carry an error_id too.
	server := &http.Server{
		Addr:    ":" + d.port,
		Handler: pogrpc.WithCorrelationID(mux),
	}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "server shutdown error", slogx.Error(err))
		}
	}()

	slog.InfoContext(ctx, "Starting server", slog.String("addr", server.Addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.ErrorContext(ctx, "failed to serve", slogx.Error(err))
		return err
	}

	return nil
}
