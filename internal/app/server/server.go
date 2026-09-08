package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/validate"
	"github.com/pug-sh/pug/internal/app/server/mcp"
	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	billingrpc "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/billing"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/customers"
	dashboardsrpc "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/dashboards"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgemailproviders"
	orgsrpc "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/projects"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/usage"
	"github.com/pug-sh/pug/internal/app/server/rpc/public/auth"
	publicdashboardsrpc "github.com/pug-sh/pug/internal/app/server/rpc/public/dashboards"
	eventsrpc "github.com/pug-sh/pug/internal/app/server/rpc/sdk/events"
	sdkprofilesrpc "github.com/pug-sh/pug/internal/app/server/rpc/sdk/profiles"
	activityrpc "github.com/pug-sh/pug/internal/app/server/rpc/shared/activity"
	"github.com/pug-sh/pug/internal/app/server/rpc/shared/insights"
	sharedprofilesrpc "github.com/pug-sh/pug/internal/app/server/rpc/shared/profiles"
	"github.com/pug-sh/pug/internal/app/server/webhook"
	"github.com/pug-sh/pug/internal/cookieless"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1/billingv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1/customersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1/orgemailprovidersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/usage/v1/usagev1connect"
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
	"golang.org/x/net/http2"
)

func Run(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

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

	// Order is load-bearing. Correlation first so the id is in context downstream;
	// otel outside ErrorInterceptor so a span exists for trace_id; ErrorInterceptor
	// outside validate, which short-circuits a bad request without calling the inner
	// chain; Logging outside that, so it observes the final resolved code. Authz is
	// innermost and is the only authorization gate — handlers carry none of their own.
	//
	// WithRecover is required, not cosmetic: on the /mcp loopback the handler runs on
	// a jsonrpc2 goroutine that no net/http recover reaches, so an escaping panic
	// would kill the process for every tenant.
	handlerOpts := connect.WithHandlerOptions(
		connect.WithInterceptors(
			pogrpc.CorrelationInterceptor(),
			d.otelInterceptor,
			pogrpc.LoggingInterceptor(),
			pogrpc.ErrorInterceptor(),
			validate.NewInterceptor(validate.WithoutErrorDetails()),
			pogrpc.PrincipalInterceptor(),
			pogrpc.AuthzInterceptor(d.authz, orgsSvc),
		),
		connect.WithRecover(pogrpc.RecoverHandlerPanic),
		// The decompressed message — WithRequestLimits only sees the gzipped wire bytes.
		connect.WithReadMaxBytes(pogrpc.MaxRequestBytes),
	)

	dashboardMW := authn.NewMiddleware(pogrpc.WithJWTAuth(d.jwtKey, queriesRo))
	sdkMW := authn.NewMiddleware(pogrpc.WithSDKAuth(projectsRepo))
	sharedMW := authn.NewMiddleware(pogrpc.WithDualAuth(d.jwtKey, queriesRo, projectsRepo))

	// Public
	authServer, err := auth.NewServer(ctx, d.pgRo, d.pgW, d.jwtKey, d.nats, d.demoEnabled)
	if err != nil {
		return fmt.Errorf("auth server: %w", err)
	}
	// The likeliest demo misconfig is the server pod missing the flag the worker has,
	// and DemoSignIn's Unavailable leaves no server-side breadcrumb.
	slog.InfoContext(ctx, "demo sign-in", slog.Bool("enabled", d.demoEnabled))
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

	email, err := newEmail(ctx, queriesRo, d.redis.Unwrap())
	if err != nil {
		return fmt.Errorf("email providers: %w", err)
	}
	orgEmailProvidersPath, orgEmailProvidersHandler := orgemailprovidersv1connect.NewOrgEmailProvidersServiceHandler(
		orgemailproviders.NewServer(queriesRo, dbwrite.New(d.pgW), email.cipher, email.repo, email.mailer),
		handlerOpts)

	customersPath, customersHandler := customersv1connect.NewCustomersServiceHandler(
		customers.NewServer(corecustomers.NewService(d.pgW)), handlerOpts)

	// No ClickHouse: the server only reads what `pug cron usage` stored, and
	// MeterWindow is the one method that needs it.
	usagePath, usageHandler := usagev1connect.NewUsageServiceHandler(
		usage.NewServer(coreusage.NewService(d.pgRo, d.pgW)), handlerOpts)

	// Postgres only: an entitlement is a row plus the clock, and the quota it
	// carries enforces nothing, so no ingestion or ClickHouse path is involved.
	billingSvc, err := corebilling.NewService(d.pgRo, d.pgW, d.billingEnabled, d.payments)
	if err != nil {
		return fmt.Errorf("billing service: %w", err)
	}
	provider := ""
	if d.payments != nil {
		provider = d.payments.Provider.Name()
	}
	// The likeliest misconfig is a pod missing the flag: every org would then read as
	// having no quota, with nothing failing. Same for a missing provider key.
	slog.InfoContext(ctx, "billing", slog.Bool("enabled", d.billingEnabled), slog.String("provider", provider))
	billingPath, billingHandler := billingv1connect.NewBillingServiceHandler(
		billingrpc.NewServer(billingSvc), handlerOpts)

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
		eventsrpc.NewServer(d.nats.GetJetStream(), geoProvider, uaParser,
			cookieless.New(d.redis.Unwrap())), handlerOpts)

	mux := http.NewServeMux()

	// No auth, no CORS. Plain paths cannot collide with the RPC routes, which are
	// all /<package>.<Service>/...
	mux.HandleFunc("/healthz", livenessHandler)
	mux.HandleFunc("/readyz", d.readinessHandler)

	// AUTHZ CONTRACT: mount every RPC service through handle(), never mux.Handle — it
	// records the service name, and the assertions below fail startup unless the
	// mounted set exactly equals the authz registry.
	mounted := map[string]bool{}
	handle := func(path string, h http.Handler) {
		mounted[strings.Trim(path, "/")] = true
		mux.Handle(path, h)
	}

	// Public (CORS, no auth)
	handle(authPath, pogrpc.WithCORS(ctx, d.corsOrigins, authHandler))
	handle(sharedDashboardsPath, pogrpc.WithCORS(ctx, d.corsOrigins, sharedDashboardsHandler))

	// Dashboard only (CORS + JWT auth)
	handle(orgsPath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(orgsHandler)))
	handle(projectsPath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(projectsHandler)))
	handle(dashboardsPath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(dashboardsHandler)))
	handle(orgEmailProvidersPath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(orgEmailProvidersHandler)))
	handle(customersPath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(customersHandler)))
	handle(usagePath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(usageHandler)))
	handle(billingPath, pogrpc.WithCORS(ctx, d.corsOrigins, dashboardMW.Wrap(billingHandler)))

	// Shared: Dashboard + private API key (CORS + dual auth)
	handle(insightsPath, pogrpc.WithCORS(ctx, d.corsOrigins, sharedMW.Wrap(insightsHandler)))
	handle(activityPath, pogrpc.WithCORS(ctx, d.corsOrigins, sharedMW.Wrap(activityHandler)))
	handle(sharedProfilesPath, pogrpc.WithCORS(ctx, d.corsOrigins, sharedMW.Wrap(sharedProfilesHandler)))

	// SDK only (API key auth). CORS is wildcard with credentials disabled because
	// customer sites embedding the SDK have arbitrary origins; auth lives entirely
	// in the x-api-key header, so there are no ambient credentials to protect.
	handle(sdkProfilesPath, pogrpc.WithSDKCORS(sdkMW.Wrap(sdkProfilesHandler)))
	handle(eventsPath, pogrpc.WithSDKCORS(sdkMW.Wrap(eventsHandler)))

	if err := assertServedServicesMatch(mounted); err != nil {
		return err
	}
	// The procedure-level half: catches a method added to an already-mounted service,
	// which the service-level check cannot see.
	if err := pogrpc.AssertRegistryMatchesServedProcedures(); err != nil {
		return err
	}

	// Advertises exactly the authorized services — same source as the check above.
	reflector := grpcreflect.NewStaticReflector(pogrpc.ServedServiceNames()...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	// mcp.Mount owns the whole endpoint, including its own private-key-only auth
	// boundary built from projectsRepo, and replays each tool call through this same
	// mux so validation, auth and authz run as they would for an external request.
	// Mounted directly like reflection: not a Connect service, so the authz-registry
	// contract does not apply.
	if err := mcp.Mount(mux, mux, projectsRepo); err != nil {
		return fmt.Errorf("mount mcp: %w", err)
	}

	// Mounted directly for the same reason as /mcp. The route is unauthenticated in
	// the middleware sense: it authenticates by HMAC over the raw body.
	if d.payments != nil {
		if webhook.MountBilling(mux, billingSvc, d.payments.Provider) {
			slog.InfoContext(ctx, "mounted the payments webhook",
				slog.String("path", webhook.BillingPath(d.payments.Provider.Name())))
		}
	}

	// WithCorrelationID wraps the whole mux so auth rejections — which happen outside
	// the interceptor chain — carry an error_id too. ReadTimeout stays unset: it
	// would cap the body as tightly as the headers, and WithRequestLimits already
	// bounds the body's size and time.
	server := &http.Server{
		Addr:              ":" + d.port,
		Handler:           pogrpc.WithCorrelationID(pogrpc.WithRequestLimits(mux)),
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "server shutdown error", slogx.Error(err)) // puglint:exempt — no span at shutdown
		}
	}()

	slog.InfoContext(ctx, "Starting server", slog.String("addr", server.Addr))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.ErrorContext(ctx, "failed to serve", slogx.Error(err)) // puglint:exempt — no span at startup
		return err
	}

	return nil
}
