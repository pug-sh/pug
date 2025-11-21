package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/campaigns/v1/campaignsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/delivery/v1/deliveryv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/projects/v1/projectsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/proto/segments/v1/segmentsv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/rpc/auth"
	"github.com/fivebitsio/cotton/internal/rpc/campaigns"
	"github.com/fivebitsio/cotton/internal/rpc/delivery"
	"github.com/fivebitsio/cotton/internal/rpc/interceptors"
	"github.com/fivebitsio/cotton/internal/rpc/projects"
	"github.com/fivebitsio/cotton/internal/rpc/segments"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/fivebitsio/cotton/pkg/pulsar"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type serverDeps struct {
	pgRo               *pgxpool.Pool
	pgW                *pgxpool.Pool
	pulsar             *pulsar.Client
	campaignsProducer  *pulsar.Producer
	deliveriesProducer *pulsar.Producer
	jwtKey             []byte
}

func newServerDeps(ctx context.Context) (*serverDeps, error) {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}

	pgRo, err := postgres.NewReaderPool(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	pgW, err := postgres.NewWriterPool(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	pulsarClient, err := pulsar.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	jwtKey := []byte("your-jwt-secret-key-here")

	campaignsProducer, err := pulsarClient.CreateProducer("campaigns")
	if err != nil {
		return nil, fmt.Errorf("failed to create campaigns pulsar producer: %w", err)
	}

	deliveriesProducer, err := pulsarClient.CreateProducer("deliveries")
	if err != nil {
		return nil, fmt.Errorf("failed to create deliveries pulsar producer: %w", err)
	}

	return &serverDeps{
		pgRo:               pgRo,
		pgW:                pgW,
		pulsar:             pulsarClient,
		campaignsProducer:  campaignsProducer,
		deliveriesProducer: deliveriesProducer,
		jwtKey:             jwtKey,
	}, nil
}

// StartServer starts the Cotton HTTP/gRPC server with the given dependencies
func StartServer(ctx context.Context, deps *serverDeps) error {
	queriesRo := dbread.New(deps.pgRo)

	commonHandlerOptions := func() connect.HandlerOption {
		return connect.WithInterceptors(interceptors.ErrorInterceptor())
	}

	authServer := auth.NewServer(deps.pgRo, deps.pgW, deps.jwtKey)
	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		authServer,
		commonHandlerOptions(),
	)

	projectsServer := projects.NewServer(deps.pgRo, deps.pgW)
	projectsPath, projectsHandler := projectsv1connect.NewProjectsServiceHandler(
		projectsServer,
		commonHandlerOptions(),
	)
	projectsHandler = authn.NewMiddleware(interceptors.JwtAuth(deps.jwtKey, queriesRo)).Wrap(projectsHandler)

	campaignsServer := campaigns.NewServer(deps.pgRo, deps.pgW, deps.campaignsProducer)
	campaignsPath, campaignsHandler := campaignsv1connect.NewCampaignServiceHandler(
		campaignsServer,
		commonHandlerOptions(),
	)
	campaignsHandler = authn.NewMiddleware(interceptors.JwtAuth(deps.jwtKey, queriesRo)).Wrap(campaignsHandler)

	segmentsServer := segments.NewServer(queriesRo, dbwrite.New(deps.pgW))
	segmentsHandlerObj := segments.NewHandler(segmentsServer.Service())
	segmentsPath, segmentsHandler := segmentsv1connect.NewSegmentsServiceHandler(
		segmentsHandlerObj,
		commonHandlerOptions(),
	)
	segmentsHandler = authn.NewMiddleware(interceptors.JwtAuth(deps.jwtKey, queriesRo)).Wrap(segmentsHandler)

	deliveryServer := delivery.NewServer(deps.deliveriesProducer)
	deliveryPath, deliveryHandler := deliveryv1connect.NewDeliveryServiceHandler(
		deliveryServer,
		commonHandlerOptions(),
	)
	deliveryHandler = authn.NewMiddleware(interceptors.JwtAuth(deps.jwtKey, queriesRo)).Wrap(deliveryHandler)

	handler := http.NewServeMux()
	handler.Handle(authPath, authHandler)
	handler.Handle(projectsPath, projectsHandler)
	handler.Handle(campaignsPath, campaignsHandler)
	handler.Handle(segmentsPath, segmentsHandler)
	handler.Handle(deliveryPath, deliveryHandler)

	services := []string{
		authv1connect.AuthServiceName,
		projectsv1connect.ProjectsServiceName,
		campaignsv1connect.CampaignServiceName,
		segmentsv1connect.SegmentsServiceName,
		deliveryv1connect.DeliveryServiceName,
	}

	reflector := grpcreflect.NewStaticReflector(services...)
	handler.Handle(grpcreflect.NewHandlerV1(reflector))
	handler.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	handlerWithCORS := interceptors.WithCORS(handler)

	h2cHandler := h2c.NewHandler(handlerWithCORS, &http2.Server{})

	port := os.Getenv("SERVER_PORT")
	if port == "" {
		port = "3000" // Default port
	}
	addr := ":" + port

	server := &http.Server{
		Addr:    addr,
		Handler: h2cHandler,
	}

	logger.Log.Info("Starting server", slog.String("addr", addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Log.Error("failed to serve", slog.Any("err", err))
		return err
	}

	return nil
}

func (deps *serverDeps) Close(ctx context.Context) {
	deps.pgRo.Close()
	deps.pgW.Close()
	if deps.campaignsProducer != nil {
		deps.campaignsProducer.Close()
	}
	if deps.deliveriesProducer != nil {
		deps.deliveriesProducer.Close()
	}
	if deps.pulsar != nil {
		deps.pulsar.Close()
	}
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

		serverErrChan := make(chan error, 1)
		go func() {
			serverErrChan <- StartServer(ctx, deps)
		}()

		<-ctx.Done()
		logger.Log.Info("Shutting down server...")
	},
}
