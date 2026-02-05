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
	"github.com/fivebitsio/cotton/internal/deps/logger"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/campaigns/v1/campaignsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/delivery/v1/deliveryv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/projects/v1/projectsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1/subscriptionsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/users/v1/usersv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	cottonrpc "github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/dashboard/projects"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/public/auth"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/subscriptions"
	usersrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/users"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/campaigns"
	"github.com/fivebitsio/cotton/internal/app/server/rpc/shared/delivery"
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
	usersPath, usersHandler := usersv1connect.NewUsersServiceHandler(
		usersrpc.NewHandler(d.pgRo, d.pgW), handlerOpts)

	subscriptionsServer, err := subscriptions.NewServer(d.nats.GetJetStream())
	if err != nil {
		return err
	}
	subscriptionsPath, subscriptionsHandler := subscriptionsv1connect.NewSubscriptionsServiceHandler(
		subscriptionsServer, handlerOpts)

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
	mux.Handle(usersPath, sdkMW.Wrap(usersHandler))

	// Reflection
	services := []string{
		authv1connect.AuthServiceName,
		projectsv1connect.ProjectsServiceName,
		campaignsv1connect.CampaignServiceName,
		deliveryv1connect.DeliveryServiceName,
		subscriptionsv1connect.SubscriptionsServiceName,
		usersv1connect.UsersServiceName,
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
			logger.Log.Error("server shutdown error", slog.Any("error", err))
		}
	}()

	logger.Log.Info("Starting server", slog.String("addr", server.Addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Log.Error("failed to serve", slog.Any("err", err))
		return err
	}

	return nil
}
