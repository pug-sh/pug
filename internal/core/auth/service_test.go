package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/auth"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/testutil"
	"github.com/golang-jwt/jwt/v5"
)

func TestAuthService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	projectsSvc := projects.NewService(db.PgRO, db.PgW, nil)
	jwtKey := []byte("test-secret-key-for-jwt")
	svc := auth.NewService(db.PgRO, db.PgW, jwtKey, projectsSvc)
	ctx := context.Background()

	var signupToken string

	t.Run("SignUpWithEmail", func(t *testing.T) {
		resp, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123")
		if err != nil {
			t.Fatalf("SignUpWithEmail: %v", err)
		}
		if resp.Token == "" {
			t.Fatal("expected non-empty token")
		}
		signupToken = resp.Token
	})

	t.Run("SignUpWithEmail_duplicate", func(t *testing.T) {
		_, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123")
		if err == nil {
			t.Fatal("expected error for duplicate email")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeAlreadyExists {
			t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeAlreadyExists)
		}
	})

	t.Run("SignInWithEmail_valid", func(t *testing.T) {
		resp, err := svc.SignInWithEmail(ctx, "test@example.com", "password123")
		if err != nil {
			t.Fatalf("SignInWithEmail: %v", err)
		}
		if resp.Token == "" {
			t.Fatal("expected non-empty token")
		}
	})

	t.Run("SignInWithEmail_wrong_password", func(t *testing.T) {
		_, err := svc.SignInWithEmail(ctx, "test@example.com", "wrongpassword")
		if err == nil {
			t.Fatal("expected error for wrong password")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeUnauthenticated {
			t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeUnauthenticated)
		}
	})

	t.Run("SignInWithEmail_nonexistent", func(t *testing.T) {
		_, err := svc.SignInWithEmail(ctx, "nobody@example.com", "password123")
		if err == nil {
			t.Fatal("expected error for nonexistent email")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeUnauthenticated {
			t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeUnauthenticated)
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
		if iss != "cotton/auth" {
			t.Errorf("issuer = %q, want %q", iss, "cotton/auth")
		}

		aud, err := parsed.Claims.GetAudience()
		if err != nil {
			t.Fatalf("GetAudience: %v", err)
		}
		if len(aud) != 1 || aud[0] != "cotton/dashboard" {
			t.Errorf("audience = %v, want [cotton/dashboard]", aud)
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
		diff := exp.Time.Sub(expectedExp)
		if diff < -time.Minute || diff > time.Minute {
			t.Errorf("expiry %v is not within 1 minute of expected %v", exp.Time, expectedExp)
		}
	})
}
