package interceptors

import (
	"context"
	"net/http"

	"connectrpc.com/authn"
	"github.com/golang-jwt/jwt/v5"
)

func JwtAuth(jwtKey []byte) authn.AuthFunc {
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

		token, err := jwt.Parse(jwtToken, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, authn.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtKey, nil
		})
		if err != nil {
			return nil, authn.Errorf("invalid authorization")
		}

		if !token.Valid {
			return nil, authn.Errorf("invalid authorization")
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			return nil, authn.Errorf("invalid claims format")
		}

		return claims, nil
	}
}
