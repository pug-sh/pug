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
	"github.com/fivebitsio/cotton/internal/app/server/rpc/dashboard/projects"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/public/auth"
	eventsrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/events"
	profilesrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/profiles"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/subscriptions"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/campaigns"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/delivery"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/campaigns/v1/campaignsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/delivery/v1/deliveryv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/events/v1/eventsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1/profilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/projects/v1/projectsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1/subscriptionsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
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

	// Middleware
	// - Dashboard: JWT auth only (for dashboard-only services)
	// - SDK: API key auth only (for SDK-only services)
	// - Shared: Dual auth - accepts either JWT or API key (for services accessible from both)
	dashboardMW := authn.NewMiddleware(cottonrpc.WithJWTAuth(d.jwtKey, queriesRo))
	sdkMW := authn.NewMiddleware(cottonrpc.WithAPIKeyAuth(queriesRo))
	sharedMW := authn.NewMiddleware(cottonrpc.WithDualAuth(d.jwtKey, queriesRo))

	// Handlers
	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		auth.NewServer(d.pgRo, d.pgW, d.jwtKey), handlerOpts)
	projectsPath, projectsHandler := projectsv1connect.NewProjectsServiceHandler(
		projects.NewServer(d.pgRo, d.pgW), handlerOpts)
	campaignsPath, campaignsHandler := campaignsv1connect.NewCampaignServiceHandler(
		campaigns.NewServer(d.pgRo, d.pgW, d.campaignsProducer), handlerOpts)
	deliveryPath, deliveryHandler := deliveryv1connect.NewDeliveryServiceHandler(
		delivery.NewServer(d.deliveriesProducer), handlerOpts)
	profilesPath, profilesHandler := profilesv1connect.NewProfilesServiceHandler(
		profilesrpc.NewHandler(d.pgRo, d.pgW), handlerOpts)

	subscriptionsServer, err := subscriptions.NewServer(d.nats.GetJetStream())
	if err != nil {
		return err
	}
	subscriptionsPath, subscriptionsHandler := subscriptionsv1connect.NewSubscriptionsServiceHandler(
		subscriptionsServer, handlerOpts)

	eventsPath, eventsHandler := eventsv1connect.NewEventsServiceHandler(
		eventsrpc.NewServer(d.eventsProducer), handlerOpts)

	mux := http.NewServeMux()

	// Public (CORS, no auth)
	mux.Handle(authPath, cottonrpc.WithCORS(d.corsOrigins, authHandler))

	// Dashboard only (CORS + JWT auth)
	mux.Handle(projectsPath, cottonrpc.WithCORS(d.corsOrigins, dashboardMW.Wrap(projectsHandler)))

	// Shared: Dashboard + SDK (CORS + dual auth)
	mux.Handle(campaignsPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(campaignsHandler)))
	mux.Handle(deliveryPath, cottonrpc.WithCORS(d.corsOrigins, sharedMW.Wrap(deliveryHandler)))

	// SDK only (API key auth, no CORS)
	mux.Handle(subscriptionsPath, sdkMW.Wrap(subscriptionsHandler))
	mux.Handle(profilesPath, sdkMW.Wrap(profilesHandler))
	mux.Handle(eventsPath, sdkMW.Wrap(eventsHandler))

	// Reflection
	services := []string{
		authv1connect.AuthServiceName,
		projectsv1connect.ProjectsServiceName,
		campaignsv1connect.CampaignServiceName,
		deliveryv1connect.DeliveryServiceName,
		eventsv1connect.EventsServiceName,
		subscriptionsv1connect.SubscriptionsServiceName,
		profilesv1connect.ProfilesServiceName,
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
			slog.Error("server shutdown error", slog.Any("error", err))
		}
	}()

	slog.Info("Starting server", slog.String("addr", server.Addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("failed to serve", slog.Any("err", err))
		return err
	}

	return nil
}
