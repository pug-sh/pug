package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"connectrpc.com/grpcreflect"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/rpc/auth"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/joho/godotenv"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		logger.Log.Error("error loading .env file", slog.Any("err", err))
		os.Exit(1)
	}

	deps, err := newDependencies(ctx)
	if err != nil {
		logger.Log.Error("error while initializing dependencies", slog.Any("err", err))
		os.Exit(1)
	}

	defer deps.Close(ctx)

	authServer := auth.NewServer(deps.pgRo, deps.pgW, deps.jwtKey)

	// Register auth service
	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		authServer,
		// Add any interceptors if needed
		// connect.WithInterceptors(yourInterceptor),
	)

	// Create a handler that combines auth and reflection
	handler := http.NewServeMux()
	handler.Handle(authPath, authHandler)

	// Optional: Add reflection for debugging/development
	services := []string{authv1connect.AuthServiceName}
	reflector := grpcreflect.NewStaticReflector(services...)
	handler.Handle(grpcreflect.NewHandlerV1(reflector))
	handler.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	// Start the server
	logger.Log.Info("Starting server", slog.String("addr", ":8080"))
	if err := http.ListenAndServe(":8080", h2c.NewHandler(handler, &http2.Server{})); err != nil {
		logger.Log.Error("failed to serve", slog.Any("err", err))
		os.Exit(1)
	}
}
