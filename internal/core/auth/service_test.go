package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/golang-jwt/jwt/v5"
)

func TestAuthService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	jwtKey := []byte("test-secret-key-for-jwt")
	svc := auth.NewService(db.PgRO, db.PgW, jwtKey)
	ctx := context.Background()

	var signupToken string

	t.Run("SignUpWithEmail", func(t *testing.T) {
		token, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123")
		if err != nil {
			t.Fatalf("SignUpWithEmail: %v", err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}
		signupToken = token
	})

	t.Run("SignUpWithEmail_duplicate", func(t *testing.T) {
		if _, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123"); err == nil {
			t.Fatal("expected error for duplicate email")
		} else if !errors.Is(err, auth.ErrEmailAlreadyExists) {
			t.Errorf("expected ErrEmailAlreadyExists, got: %v", err)
		}
	})

	t.Run("SignInWithEmail_valid", func(t *testing.T) {
		token, err := svc.SignInWithEmail(ctx, "test@example.com", "password123")
		if err != nil {
			t.Fatalf("SignInWithEmail: %v", err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}
	})

	t.Run("SignInWithEmail_wrong_password", func(t *testing.T) {
		if _, err := svc.SignInWithEmail(ctx, "test@example.com", "wrongpassword"); err == nil {
			t.Fatal("expected error for wrong password")
		} else if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("expected ErrInvalidCredentials, got: %v", err)
		}
	})

	t.Run("SignInWithEmail_nonexistent", func(t *testing.T) {
		if _, err := svc.SignInWithEmail(ctx, "nobody@example.com", "password123"); err == nil {
			t.Fatal("expected error for nonexistent email")
		} else if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("expected ErrInvalidCredentials, got: %v", err)
		}
	})

	t.Run("JWT_structure", func(t *testing.T) {
		if signupToken == "" {
			t.Skip("skipping: SignUpWithEmail did not produce a token")
		}
		parsed, err := jwt.Parse(signupToken, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, errors.New("unexpected signing method")
			}
			return jwtKey, nil
		})
		if err != nil {
			t.Fatalf("JWT parse: %v", err)
		}
		if !parsed.Valid {
			t.Fatal("JWT is not valid")
		}

		iss, err := parsed.Claims.GetIssuer()
		if err != nil {
			t.Fatalf("GetIssuer: %v", err)
		}
		if iss != "pug/auth" {
			t.Errorf("issuer = %q, want %q", iss, "pug/auth")
		}

		aud, err := parsed.Claims.GetAudience()
		if err != nil {
			t.Fatalf("GetAudience: %v", err)
		}
		if len(aud) != 1 || aud[0] != "pug/dashboard" {
			t.Errorf("audience = %v, want [pug/dashboard]", aud)
		}

		sub, err := parsed.Claims.GetSubject()
		if err != nil {
			t.Fatalf("GetSubject: %v", err)
		}
		if sub == "" {
			t.Error("expected non-empty subject (customer ID)")
		}

		exp, err := parsed.Claims.GetExpirationTime()
		if err != nil {
			t.Fatalf("GetExpirationTime: %v", err)
		}
		expectedExp := time.Now().Add(90 * 24 * time.Hour)
		diff := exp.Sub(expectedExp)
		if diff < -time.Minute || diff > time.Minute {
			t.Errorf("expiry %v is not within 1 minute of expected %v", exp, expectedExp)
		}
	})
}
