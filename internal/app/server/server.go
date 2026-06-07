package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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

	// Interceptor order matters. ErrorInterceptor must wrap validate.NewInterceptor():
	// validate short-circuits on a bad request (returns the error without calling the
	// inner chain), so ErrorInterceptor has to be OUTSIDE it to attach error details
	// (reason + correlation id) to validation failures. It stays inside otel so a span
	// is present for trace_id. CorrelationInterceptor is first so the id is in context
	// for every downstream interceptor and handler. LoggingInterceptor must stay
	// OUTSIDE ErrorInterceptor so it observes the final *connect.Error with its
	// resolved code for client-vs-server log-level classification (isClientError also
	// reads *apperr.Error directly as a safety net, but order keeps that path moot).
	handlerOpts := connect.WithInterceptors(
		pogrpc.CorrelationInterceptor(),
		d.otelInterceptor,
		pogrpc.LoggingInterceptor(),
		pogrpc.ErrorInterceptor(),
		validate.NewInterceptor(validate.WithoutErrorDetails()),
		pogrpc.PrincipalInterceptor(),
	)

	projectsRepo := coreprojects.NewRepo(queriesRo, d.redis.Unwrap())
	projectsSvc := coreprojects.NewService(d.pgRo, d.pgW, projectsRepo)
	dashboardsSvc := coredashboards.NewService(d.pgRo, d.pgW)
	orgsSvc := coreorgs.NewService(d.pgRo, d.pgW, d.nats)
	insightsExecutor := coreinsights.NewExecutor(d.ch)
	insightsSvc := coreinsights.NewService(insightsExecutor, d.redis.Unwrap())

	// Middleware
	// - Dashboard: JWT auth only (for dashboard-only services)
	// - SDK: API key auth (public or private, no JWT fallback) for SDK-only services
	// - Shared: Dual auth - private API key or JWT fallback (for services accessible from both)
	dashboardMW := authn.NewMiddleware(pogrpc.WithJWTAuth(d.jwtKey, queriesRo))
	sdkMW := authn.NewMiddleware(pogrpc.WithSDKAuth(projectsRepo))
	sharedMW := authn.NewMiddleware(pogrpc.WithDualAuth(d.jwtKey, queriesRo, projectsRepo))

	// Handlers — grouped by auth boundary

	// Public
	authServer, err := auth.NewServer(ctx, d.pgRo, d.pgW, d.jwtKey, d.nats, d.redis.Unwrap())
	if err != nil {
		return fmt.Errorf("auth server: %w", err)
	}
	authPath, authHandler := authv1connect.NewAuthServiceHandler(authServer, handlerOpts)

	// Dashboard
	orgsPath, orgsHandler := orgsv1connect.NewOrgsServiceHandler(
		orgsrpc.NewServer(orgsSvc), handlerOpts)
	projectsPath, projectsHandler := projectsv1connect.NewProjectsServiceHandler(
		projects.NewServer(projectsSvc, orgsSvc), handlerOpts)
	dashboardsPath, dashboardsHandler := dashboardsv1connect.NewDashboardsServiceHandler(
		dashboardsrpc.NewServer(dashboardsSvc, insightsExecutor), handlerOpts)

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
		orgemailproviders.NewServer(orgsSvc, queriesRo, dbwrite.New(d.pgW), emailCipher, orgEmailRepo, emailMailer),
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

	// Public (CORS, no auth)
	mux.Handle(authPath, pogrpc.WithCORS(d.corsOrigins, authHandler))

	// Dashboard only (CORS + JWT auth)
	mux.Handle(orgsPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(orgsHandler)))
	mux.Handle(projectsPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(projectsHandler)))
	mux.Handle(dashboardsPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(dashboardsHandler)))
	mux.Handle(orgEmailProvidersPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(orgEmailProvidersHandler)))
	mux.Handle(customersPath, pogrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(customersHandler)))

	// Shared: Dashboard + private API key (CORS + dual auth)
	mux.Handle(insightsPath, pogrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(insightsHandler)))
	mux.Handle(activityPath, pogrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(activityHandler)))
	mux.Handle(sharedProfilesPath, pogrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(sharedProfilesHandler)))

	// SDK only (API key auth). CORS is wildcard with credentials disabled because
	// customer sites embedding the SDK have arbitrary origins; auth lives entirely
	// in the x-api-key header, so there are no ambient credentials to protect.
	mux.Handle(sdkProfilesPath, pogrpc.WithSDKCORS(sdkMW.Wrap(sdkProfilesHandler)))
	mux.Handle(eventsPath, pogrpc.WithSDKCORS(sdkMW.Wrap(eventsHandler)))

	// Reflection
	services := []string{
		// Public
		authv1connect.AuthServiceName,
		// Dashboard
		orgsv1connect.OrgsServiceName,
		projectsv1connect.ProjectsServiceName,
		dashboardsv1connect.DashboardsServiceName,
		orgemailprovidersv1connect.OrgEmailProvidersServiceName,
		customersv1connect.CustomersServiceName,
		// Shared
		insightsv1connect.InsightsServiceName,
		activityv1connect.ActivityServiceName,
		profilesv1connect.ProfilesServiceName,
		// SDK
		sdkprofilesv1connect.ProfilesSDKServiceName,
		eventsv1connect.EventsServiceName,
	}
	reflector := grpcreflect.NewStaticReflector(services...)
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
