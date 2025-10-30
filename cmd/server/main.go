package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	connectcors "connectrpc.com/cors"
	"connectrpc.com/grpcreflect"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/rpc/auth"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/joho/godotenv"
	"github.com/rs/cors"
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

	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		authServer,
		// connect.WithInterceptors(yourInterceptor),
	)

	handler := http.NewServeMux()
	handler.Handle(authPath, authHandler)

	services := []string{authv1connect.AuthServiceName}
	reflector := grpcreflect.NewStaticReflector(services...)
	handler.Handle(grpcreflect.NewHandlerV1(reflector))
	handler.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	handlerWithCORS := withCORS(handler)

	h2cHandler := h2c.NewHandler(handlerWithCORS, &http2.Server{})

	logger.Log.Info("Starting server", slog.String("addr", ":8081"))
	if err := http.ListenAndServe(":8081", h2cHandler); err != nil {
		logger.Log.Error("failed to serve", slog.Any("err", err))
		os.Exit(1)
	}
}

func withCORS(connectHandler http.Handler) http.Handler {
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   append(connectcors.AllowedMethods(), http.MethodOptions),
		AllowedHeaders:   connectcors.AllowedHeaders(),
		ExposedHeaders:   connectcors.ExposedHeaders(),
		AllowCredentials: false,
		MaxAge:           7200,
	})
	return c.Handler(connectHandler)
}
