package dashboard

import (
	"context"
	"log/slog"
	"net/http"

	"connectrpc.com/authn"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/pkg/logger/slogx"
	"github.com/golang-jwt/jwt/v5"
)

func WithJWTAuth(jwtKey []byte, queries *dbread.Queries) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		authHeader := req.Header.Get("Authorization")
		if authHeader == "" {
			return nil, authn.Errorf("Authorization header not present")
		}

		const prefix = "Bearer "
		if len(authHeader) <= len(prefix) || authHeader[:len(prefix)] != prefix {
			return nil, authn.Errorf("Authorization header must start with Bearer")
		}

		jwtToken := authHeader[len(prefix):]
		if jwtToken == "" {
			return nil, authn.Errorf("Bearer token is empty")
		}

		jwt, err := jwt.Parse(jwtToken, func(token *jwt.Token) (any, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, authn.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtKey, nil
		})
		if err != nil {
			return nil, authn.Errorf("invalid authorization")
		}

		if !jwt.Valid {
			return nil, authn.Errorf("invalid authorization")
		}

		customerID, err := jwt.Claims.GetSubject()
		if err != nil {
			slog.ErrorContext(ctx, "unable to get subject", slogx.Error(err))
			return nil, authn.Errorf("customer claim not found or not a string")
		}

		customer, err := queries.GetCustomerByID(ctx, customerID)
		if err != nil {
			slog.ErrorContext(ctx, "unable to get customer", slogx.Error(err), slog.String("token", jwtToken))
			// todo - ensure sensistive data is not leaked
			return nil, err
		}

		return customer, nil
	}
}

func GetCustomerFromContext(ctx context.Context) (dbread.Customer, error) {
	customerCtx := authn.GetInfo(ctx)

	if customer, ok := customerCtx.(dbread.Customer); ok {
		return customer, nil
	}

	return dbread.Customer{}, authn.Errorf("context value is not a Customer type: %T", customerCtx)
}
