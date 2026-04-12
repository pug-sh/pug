package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/validate"
	cottonrpc "github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/insights"
	orgsrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/dashboard/orgs"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/dashboard/projects"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/public/auth"
	devicesrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/devices"
	eventsrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/events"
	sdkprofilesrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/profiles"
	activityrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/shared/activity"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/campaigns"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/delivery"
	sharedprofilesrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/shared/profiles"
	coreinsights "github.com/fivebitsio/cotton/internal/core/insights"
	coreorgs "github.com/fivebitsio/cotton/internal/core/orgs"
	coreprojects "github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/public/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/sdk/devices/v1/devicesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/activity/v1/activityv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/campaigns/v1/campaignsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/delivery/v1/deliveryv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/geo"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/fivebitsio/cotton/internal/useragent"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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

	handlerOpts := connect.WithInterceptors(validate.NewInterceptor(), cottonrpc.ErrorInterceptor())

	projectsRepo := coreprojects.NewRepo(queriesRo, d.redis.Unwrap())
	projectsSvc := coreprojects.NewService(d.pgRo, d.pgW, projectsRepo)
	orgsSvc := coreorgs.NewService(d.pgRo, d.pgW)

	// Middleware
	// - Dashboard: JWT auth only (for dashboard-only services)
	// - SDK: API key auth (public or private, no JWT fallback) for SDK-only services
	// - Shared: Dual auth - private API key or JWT fallback (for services accessible from both)
	dashboardMW := authn.NewMiddleware(cottonrpc.WithJWTAuth(d.jwtKey, queriesRo))
	sdkMW := authn.NewMiddleware(cottonrpc.WithSDKAuth(projectsRepo))
	sharedMW := authn.NewMiddleware(cottonrpc.WithDualAuth(d.jwtKey, queriesRo, projectsRepo))

	// Handlers — grouped by auth boundary

	// Public
	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		auth.NewServer(d.pgRo, d.pgW, d.jwtKey), handlerOpts)

	// Dashboard
	orgsPath, orgsHandler := orgsv1connect.NewOrgsServiceHandler(
		orgsrpc.NewServer(orgsSvc), handlerOpts)
	projectsPath, projectsHandler := projectsv1connect.NewProjectsServiceHandler(
		projects.NewServer(projectsSvc, orgsSvc), handlerOpts)

	// Shared
	insightsExecutor := coreinsights.NewExecutor(d.ch)
	insightsSvc := coreinsights.NewService(insightsExecutor, d.redis.Unwrap())
	insightsPath, insightsHandler := insightsv1connect.NewInsightsServiceHandler(
		insights.NewServer(insightsSvc, insightsExecutor), handlerOpts)
	campaignsPath, campaignsHandler := campaignsv1connect.NewCampaignServiceHandler(
		campaigns.NewServer(d.pgRo, d.pgW, d.nats.GetJetStream()), handlerOpts)
	deliveryPath, deliveryHandler := deliveryv1connect.NewDeliveryServiceHandler(
		delivery.NewServer(d.nats.GetJetStream()), handlerOpts)
	activityPath, activityHandler := activityv1connect.NewActivityServiceHandler(
		activityrpc.NewServer(d.ch, insightsSvc), handlerOpts)
	sharedProfilesPath, sharedProfilesHandler := profilesv1connect.NewProfilesServiceHandler(
		sharedprofilesrpc.NewServer(d.pgRo, d.pgW, d.nats), handlerOpts)

	// SDK
	devicesPath, devicesHandler := devicesv1connect.NewDevicesServiceHandler(
		devicesrpc.NewServer(d.nats.GetJetStream()), handlerOpts)
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
	mux.Handle(authPath, cottonrpc.WithCORS(d.corsOrigins, authHandler))

	// Dashboard only (CORS + JWT auth)
	mux.Handle(orgsPath, cottonrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(orgsHandler)))
	mux.Handle(projectsPath, cottonrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(projectsHandler)))

	// Shared: Dashboard + private API key (CORS + dual auth)
	mux.Handle(insightsPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(insightsHandler)))
	mux.Handle(campaignsPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(campaignsHandler)))
	mux.Handle(deliveryPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(deliveryHandler)))
	mux.Handle(activityPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(activityHandler)))
	mux.Handle(sharedProfilesPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(sharedProfilesHandler)))

	// SDK only (API key auth, no CORS)
	mux.Handle(devicesPath, sdkMW.Wrap(devicesHandler))
	mux.Handle(sdkProfilesPath, sdkMW.Wrap(sdkProfilesHandler))
	mux.Handle(eventsPath, sdkMW.Wrap(eventsHandler))

	// Reflection
	services := []string{
		// Public
		authv1connect.AuthServiceName,
		// Dashboard
		orgsv1connect.OrgsServiceName,
		projectsv1connect.ProjectsServiceName,
		// Shared
		insightsv1connect.InsightsServiceName,
		campaignsv1connect.CampaignServiceName,
		deliveryv1connect.DeliveryServiceName,
		activityv1connect.ActivityServiceName,
		profilesv1connect.ProfilesServiceName,
		// SDK
		devicesv1connect.DevicesServiceName,
		sdkprofilesv1connect.ProfilesSDKServiceName,
		eventsv1connect.EventsServiceName,
	}
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	server := &http.Server{
		Addr:    ":" + d.port,
		Handler: h2c.NewHandler(mux, &http2.Server{}),
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
