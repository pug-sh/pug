package commands

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/validate"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/campaigns/v1/campaignsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/delivery/v1/deliveryv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/projects/v1/projectsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/users/v1/usersv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	cottonrpc "github.com/fivebitsio/cotton/internal/rpc"
	"github.com/fivebitsio/cotton/internal/rpc/dashboard"
	"github.com/fivebitsio/cotton/internal/rpc/dashboard/campaigns"
	"github.com/fivebitsio/cotton/internal/rpc/dashboard/projects"
	"github.com/fivebitsio/cotton/internal/rpc/public/auth"
	"github.com/fivebitsio/cotton/internal/rpc/sdk"
	"github.com/fivebitsio/cotton/internal/rpc/sdk/delivery"
	usersrpc "github.com/fivebitsio/cotton/internal/rpc/sdk/users"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/nats"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type serverDeps struct {
	pgRo               *pgxpool.Pool
	pgW                *pgxpool.Pool
	nats               *nats.NATSClient
	campaignsProducer  jetstream.JetStream
	deliveriesProducer jetstream.JetStream
	jwtKey             []byte
	port               string
}

func (deps *serverDeps) Close(ctx context.Context) {
	deps.pgRo.Close()
	deps.pgW.Close()
	if deps.nats != nil {
		deps.nats.Close()
	}
}

func newServerDeps(ctx context.Context) (*serverDeps, error) {
	var serverCfg ServerConfig
	if err := envconfig.Process(ctx, &serverCfg); err != nil {
		return nil, err
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, err
	}

	pgRo, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return nil, err
	}

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return nil, err
	}

	natsClient, err := nats.New(ctx)
	if err != nil {
		return nil, err
	}

	return &serverDeps{
		pgRo:               pgRo,
		pgW:                pgW,
		nats:               natsClient,
		campaignsProducer:  natsClient.GetJetStream(),
		deliveriesProducer: natsClient.GetJetStream(),
		jwtKey:             []byte(serverCfg.JWTKey),
		port:               serverCfg.Port,
	}, nil
}

func StartServer(ctx context.Context, deps *serverDeps) error {
	queriesRo := dbread.New(deps.pgRo)

	handlerOpts := connect.WithInterceptors(validate.NewInterceptor(), cottonrpc.ErrorInterceptor())

	// Middleware
	dashboardMW := authn.NewMiddleware(dashboard.WithJWTAuth(deps.jwtKey, queriesRo))
	sdkMW := authn.NewMiddleware(sdk.WithAPIKeyAuth(queriesRo))

	// Handlers
	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		auth.NewServer(deps.pgRo, deps.pgW, deps.jwtKey), handlerOpts)
	projectsPath, projectsHandler := projectsv1connect.NewProjectsServiceHandler(
		projects.NewServer(deps.pgRo, deps.pgW), handlerOpts)
	campaignsPath, campaignsHandler := campaignsv1connect.NewCampaignServiceHandler(
		campaigns.NewServer(deps.pgRo, deps.pgW, deps.campaignsProducer), handlerOpts)
	deliveryPath, deliveryHandler := deliveryv1connect.NewDeliveryServiceHandler(
		delivery.NewServer(deps.deliveriesProducer), handlerOpts)
	usersPath, usersHandler := usersv1connect.NewUsersServiceHandler(
		usersrpc.NewHandler(deps.pgRo, deps.pgW), handlerOpts)

	mux := http.NewServeMux()

	// Public (CORS, no auth)
	mux.Handle(authPath, cottonrpc.WithCORS(authHandler))

	// Dashboard (CORS + JWT auth)
	mux.Handle(projectsPath, cottonrpc.WithCORS(dashboardMW.Wrap(projectsHandler)))
	mux.Handle(campaignsPath, cottonrpc.WithCORS(dashboardMW.Wrap(campaignsHandler)))

	// SDK (API key auth, no CORS)
	mux.Handle(deliveryPath, sdkMW.Wrap(deliveryHandler))
	mux.Handle(usersPath, sdkMW.Wrap(usersHandler))

	// Reflection
	services := []string{
		authv1connect.AuthServiceName,
		projectsv1connect.ProjectsServiceName,
		campaignsv1connect.CampaignServiceName,
		deliveryv1connect.DeliveryServiceName,
		usersv1connect.UsersServiceName,
	}
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	server := &http.Server{
		Addr:    ":" + deps.port,
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	logger.Log.Info("Starting server", slog.String("addr", server.Addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Log.Error("failed to serve", slog.Any("err", err))
		return err
	}

	return nil
}

var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Cotton server",
	Long:  `Start the Cotton server that handles API requests.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			logger.Log.Error("error loading .env file", slog.Any("err", err))
			os.Exit(1)
		}

		deps, err := newServerDeps(ctx)
		if err != nil {
			logger.Log.Error("error while initializing dependencies", slog.Any("err", err))
			os.Exit(1)
		}

		defer deps.Close(ctx)

		if err := StartServer(ctx, deps); err != nil {
			logger.Log.Error("server error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}
