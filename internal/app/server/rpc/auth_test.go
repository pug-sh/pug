package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

type stubProjectKeyLookup struct {
	publicProjects  map[string]dbread.Project
	privateProjects map[string]dbread.Project
	forceErr        error // if set, all lookups return this error
}

func (s *stubProjectKeyLookup) GetProjectByPublicApiKey(_ context.Context, key string) (dbread.Project, error) {
	if s.forceErr != nil {
		return dbread.Project{}, s.forceErr
	}
	p, ok := s.publicProjects[key]
	if !ok {
		return dbread.Project{}, pgx.ErrNoRows
	}
	return p, nil
}

func (s *stubProjectKeyLookup) GetProjectByPrivateApiKey(_ context.Context, key string) (dbread.Project, error) {
	if s.forceErr != nil {
		return dbread.Project{}, s.forceErr
	}
	p, ok := s.privateProjects[key]
	if !ok {
		return dbread.Project{}, pgx.ErrNoRows
	}
	return p, nil
}

func (s *stubProjectKeyLookup) InvalidateProjectKeys(_ context.Context, _, _ string) {}

func newStubLookup() *stubProjectKeyLookup {
	return &stubProjectKeyLookup{
		publicProjects: map[string]dbread.Project{
			"pub_valid123": {ID: "proj-1", PublicApiKey: "pub_valid123"},
		},
		privateProjects: map[string]dbread.Project{
			"prv_valid456": {ID: "proj-2", PrivateApiKey: "prv_valid456"},
		},
	}
}

func newRequest(header, queryKey string) *http.Request {
	u := &url.URL{Path: "/test"}
	if queryKey != "" {
		q := u.Query()
		q.Set(QueryAPIKey, queryKey)
		u.RawQuery = q.Encode()
	}
	req := &http.Request{URL: u, Header: http.Header{}}
	if header != "" {
		req.Header.Set(HeaderAPIKey, header)
	}
	return req
}

func TestWithSDKAuth(t *testing.T) {
	stub := newStubLookup()
	authFunc := WithSDKAuth(stub)
	ctx := context.Background()

	t.Run("public key via header succeeds", func(t *testing.T) {
		result, err := authFunc(ctx, newRequest("pub_valid123", ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.Project.ID != "proj-1" {
			t.Errorf("project ID = %q, want %q", p.Project.ID, "proj-1")
		}
		if p.Customer != nil {
			t.Error("expected Customer to be nil for SDK auth")
		}
		if p.AuthType != AuthTypePublicKey {
			t.Errorf("AuthType = %v, want %v", p.AuthType, AuthTypePublicKey)
		}
		if want := "...d123"; p.MaskedAPIKey != want {
			t.Errorf("MaskedAPIKey = %q, want %q", p.MaskedAPIKey, want)
		}
		if p.JWTID != "" {
			t.Errorf("JWTID = %q, want empty for API key auth", p.JWTID)
		}
	})

	t.Run("private key via header succeeds", func(t *testing.T) {
		result, err := authFunc(ctx, newRequest("prv_valid456", ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.Project.ID != "proj-2" {
			t.Errorf("project ID = %q, want %q", p.Project.ID, "proj-2")
		}
		if p.AuthType != AuthTypePrivateKey {
			t.Errorf("AuthType = %v, want %v", p.AuthType, AuthTypePrivateKey)
		}
		if p.MaskedAPIKey == "" {
			t.Error("expected MaskedAPIKey to be set for API key auth")
		}
		if p.JWTID != "" {
			t.Errorf("JWTID = %q, want empty for API key auth", p.JWTID)
		}
	})

	t.Run("public key via query param succeeds", func(t *testing.T) {
		result, err := authFunc(ctx, newRequest("", "pub_valid123"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.Project.ID != "proj-1" {
			t.Errorf("project ID = %q, want %q", p.Project.ID, "proj-1")
		}
	})

	t.Run("private key via query param rejected", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("", "prv_valid456")); err == nil {
			t.Fatal("expected error for private key in query param")
		} else if got := err.Error(); !strings.Contains(got, "beacon requests only support public API keys") {
			t.Errorf("error = %q, want to contain beacon rejection message", got)
		}
	})

	t.Run("header takes precedence over query param", func(t *testing.T) {
		result, err := authFunc(ctx, newRequest("prv_valid456", "prv_shouldbeignored"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.Project.ID != "proj-2" {
			t.Errorf("project ID = %q, want %q", p.Project.ID, "proj-2")
		}
	})

	t.Run("missing key returns error", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("", "")); err == nil {
			t.Fatal("expected error for missing key")
		}
	})

	t.Run("invalid prefix returns error", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("xyz_invalid", "")); err == nil {
			t.Fatal("expected error for invalid prefix")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("valid prefix but nonexistent key returns error", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("pub_doesnotexist", "")); err == nil {
			t.Fatal("expected error for nonexistent key")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("nonexistent private key returns error", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("prv_doesnotexist", "")); err == nil {
			t.Fatal("expected error for nonexistent private key")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("database error returns failed to validate", func(t *testing.T) {
		errStub := &stubProjectKeyLookup{
			publicProjects:  map[string]dbread.Project{},
			privateProjects: map[string]dbread.Project{},
			forceErr:        errors.New("connection refused"),
		}
		errAuthFunc := WithSDKAuth(errStub)

		if _, err := errAuthFunc(ctx, newRequest("pub_valid123", "")); err == nil {
			t.Fatal("expected error for database failure")
		} else if got := err.Error(); !strings.Contains(got, "failed to validate API key") {
			t.Errorf("error = %q, want to contain %q", got, "failed to validate API key")
		}
	})
}

// stubPGXRow implements pgx.Row for testing.
type stubPGXRow struct {
	scanFn func(dest ...any) error
}

func (r *stubPGXRow) Scan(dest ...any) error {
	return r.scanFn(dest...)
}

// stubDBTX implements dbread.DBTX for testing JWT auth.
type stubDBTX struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *stubDBTX) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (s *stubDBTX) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (s *stubDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.queryRowFn(ctx, sql, args...)
}

func signTestJWT(t *testing.T, key []byte, claims jwt.MapClaims) string {
	t.Helper()
	// Default to the same registered claims generateJWT mints so the token
	// satisfies WithJWTAuth's issuer/audience/expiry checks. Tests exercising
	// those checks override the relevant claim (or delete it) before signing.
	if _, ok := claims["aud"]; !ok {
		claims["aud"] = coreauth.Audience
	}
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = coreauth.Issuer
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("failed to sign test JWT: %v", err)
	}
	return signed
}

func newJWTRequest(token, projectID string) *http.Request {
	req := &http.Request{URL: &url.URL{Path: "/test"}, Header: http.Header{}}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if projectID != "" {
		req.Header.Set(HeaderProjectID, projectID)
	}
	return req
}

func TestWithJWTAuth(t *testing.T) {
	jwtKey := []byte("test-jwt-key")
	ctx := context.Background()

	db := &stubDBTX{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			switch {
			case strings.Contains(sql, "from customers"):
				customerID := args[0].(string)
				if customerID == "cust-1" {
					return &stubPGXRow{scanFn: func(dest ...any) error {
						// GetCustomerByID scan order: CreateTime, DisplayName, Email, ID, PasswordHash, PictureUri, UpdateTime
						*(dest[3].(*string)) = "cust-1"
						*(dest[2].(*string)) = "test@example.com"
						return nil
					}}
				}
				return &stubPGXRow{scanFn: func(_ ...any) error { return pgx.ErrNoRows }}
			case strings.Contains(sql, "from projects"):
				projectID := args[0].(string)
				if projectID == "proj-1" {
					return &stubPGXRow{scanFn: func(dest ...any) error {
						// GetProjectByIDAndOrgMember scan order: CreateTime, DisplayName, FcmServiceJson, ID, OrgID, PrivateApiKey, PublicApiKey, UpdateTime
						*(dest[3].(*string)) = "proj-1"
						*(dest[4].(*string)) = "org-1"
						return nil
					}}
				}
				return &stubPGXRow{scanFn: func(_ ...any) error { return pgx.ErrNoRows }}
			default:
				return &stubPGXRow{scanFn: func(_ ...any) error { return errors.New("unexpected query") }}
			}
		},
	}

	queries := dbread.New(db)
	authFunc := WithJWTAuth(jwtKey, queries)

	t.Run("valid JWT returns principal with customer", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1"})
		result, err := authFunc(ctx, newJWTRequest(token, ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.AuthType != AuthTypeJWT {
			t.Errorf("AuthType = %v, want %v", p.AuthType, AuthTypeJWT)
		}
		if p.Customer == nil || p.Customer.ID != "cust-1" {
			t.Errorf("Customer.ID = %v, want %q", p.Customer, "cust-1")
		}
		if p.Project != nil {
			t.Error("expected Project to be nil without x-project-id header")
		}
		if p.MaskedAPIKey != "" {
			t.Errorf("MaskedAPIKey = %q, want empty for JWT auth", p.MaskedAPIKey)
		}
	})

	t.Run("valid JWT with jti populates JWTID", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1", "jti": "token-id-123"})
		result, err := authFunc(ctx, newJWTRequest(token, ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.JWTID != "token-id-123" {
			t.Errorf("JWTID = %q, want %q", p.JWTID, "token-id-123")
		}
	})

	t.Run("valid JWT without jti leaves JWTID empty", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1"})
		result, err := authFunc(ctx, newJWTRequest(token, ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.JWTID != "" {
			t.Errorf("JWTID = %q, want empty", p.JWTID)
		}
	})

	t.Run("valid JWT with x-project-id populates Project", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1"})
		result, err := authFunc(ctx, newJWTRequest(token, "proj-1"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.Project == nil || p.Project.ID != "proj-1" {
			t.Errorf("Project.ID = %v, want %q", p.Project, "proj-1")
		}
		if p.Project != nil && p.Project.OrgID != "org-1" {
			t.Errorf("Project.OrgID = %q, want %q", p.Project.OrgID, "org-1")
		}
	})

	t.Run("valid JWT with nonexistent project returns error", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1"})
		if _, err := authFunc(ctx, newJWTRequest(token, "proj-nonexistent")); err == nil {
			t.Fatal("expected error for nonexistent project")
		} else if got := err.Error(); !strings.Contains(got, "project not found or access denied") {
			t.Errorf("error = %q, want to contain %q", got, "project not found or access denied")
		}
	})

	t.Run("customer not found returns error", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "nonexistent-cust"})
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for nonexistent customer")
		} else if got := err.Error(); !strings.Contains(got, "invalid authorization") {
			t.Errorf("error = %q, want to contain %q", got, "invalid authorization")
		}
	})

	t.Run("missing Authorization header returns error", func(t *testing.T) {
		req := &http.Request{URL: &url.URL{Path: "/test"}, Header: http.Header{}}
		if _, err := authFunc(ctx, req); err == nil {
			t.Fatal("expected error for missing Authorization header")
		} else if got := err.Error(); !strings.Contains(got, "Authorization header not present") {
			t.Errorf("error = %q, want to contain %q", got, "Authorization header not present")
		}
	})

	t.Run("non-Bearer prefix returns error", func(t *testing.T) {
		req := &http.Request{URL: &url.URL{Path: "/test"}, Header: http.Header{}}
		req.Header.Set("Authorization", "Basic abc123")
		if _, err := authFunc(ctx, req); err == nil {
			t.Fatal("expected error for non-Bearer prefix")
		} else if got := err.Error(); !strings.Contains(got, "Authorization header must start with Bearer") {
			t.Errorf("error = %q, want to contain %q", got, "Authorization header must start with Bearer")
		}
	})

	t.Run("empty Bearer token returns error", func(t *testing.T) {
		req := &http.Request{URL: &url.URL{Path: "/test"}, Header: http.Header{}}
		req.Header.Set("Authorization", "Bearer ")
		if _, err := authFunc(ctx, req); err == nil {
			t.Fatal("expected error for empty Bearer token")
		} else if got := err.Error(); !strings.Contains(got, "Bearer token is empty") {
			t.Errorf("error = %q, want to contain %q", got, "Bearer token is empty")
		}
	})

	t.Run("invalid JWT signature returns error", func(t *testing.T) {
		token := signTestJWT(t, []byte("wrong-key"), jwt.MapClaims{"sub": "cust-1"})
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for invalid JWT signature")
		} else if got := err.Error(); !strings.Contains(got, "invalid authorization") {
			t.Errorf("error = %q, want to contain %q", got, "invalid authorization")
		}
	})

	t.Run("expired JWT returns error", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1", "exp": time.Now().Add(-time.Hour).Unix()})
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for expired JWT")
		} else if got := err.Error(); !strings.Contains(got, "invalid authorization") {
			t.Errorf("error = %q, want to contain %q", got, "invalid authorization")
		}
	})

	t.Run("JWT without expiry returns error", func(t *testing.T) {
		// Build inline (bypassing signTestJWT's exp default) to assert
		// WithExpirationRequired rejects a token that never expires.
		raw := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "cust-1", "aud": coreauth.Audience, "iss": coreauth.Issuer})
		token, err := raw.SignedString(jwtKey)
		if err != nil {
			t.Fatalf("failed to sign test JWT: %v", err)
		}
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for JWT without expiry")
		} else if got := err.Error(); !strings.Contains(got, "invalid authorization") {
			t.Errorf("error = %q, want to contain %q", got, "invalid authorization")
		}
	})

	t.Run("wrong audience returns error", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1", "aud": "some-other-service"})
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for wrong audience")
		} else if got := err.Error(); !strings.Contains(got, "invalid authorization") {
			t.Errorf("error = %q, want to contain %q", got, "invalid authorization")
		}
	})

	t.Run("wrong issuer returns error", func(t *testing.T) {
		token := signTestJWT(t, jwtKey, jwt.MapClaims{"sub": "cust-1", "iss": "evil-issuer"})
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for wrong issuer")
		} else if got := err.Error(); !strings.Contains(got, "invalid authorization") {
			t.Errorf("error = %q, want to contain %q", got, "invalid authorization")
		}
	})
}

func TestWithDualAuth(t *testing.T) {
	stub := newStubLookup()
	// WithDualAuth requires a jwtAuth fallback; use a dummy key and nil queries
	// since we only test the API key path here.
	authFunc := WithDualAuth([]byte("test-jwt-key"), nil, stub)
	ctx := context.Background()

	t.Run("private key via header succeeds", func(t *testing.T) {
		result, err := authFunc(ctx, newRequest("prv_valid456", ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p := result.(*Principal)
		if p.Project.ID != "proj-2" {
			t.Errorf("project ID = %q, want %q", p.Project.ID, "proj-2")
		}
		if p.Customer != nil {
			t.Error("expected Customer to be nil for API key auth")
		}
		if p.AuthType != AuthTypePrivateKey {
			t.Errorf("AuthType = %v, want %v", p.AuthType, AuthTypePrivateKey)
		}
		if p.MaskedAPIKey == "" {
			t.Error("expected MaskedAPIKey to be set for API key auth")
		}
	})

	t.Run("public key via header rejected", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("pub_valid123", "")); err == nil {
			t.Fatal("expected error for public key on dual auth")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("nonexistent private key returns error", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("prv_doesnotexist", "")); err == nil {
			t.Fatal("expected error for nonexistent private key")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("invalid prefix rejected", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("xyz_invalid", "")); err == nil {
			t.Fatal("expected error for invalid prefix")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("missing header falls back to JWT", func(t *testing.T) {
		// No API key header → falls back to JWT auth, which will fail
		// because there's no Authorization header.
		if _, err := authFunc(ctx, newRequest("", "")); err == nil {
			t.Fatal("expected error for missing auth")
		} else if got := err.Error(); !strings.Contains(got, "Authorization header not present") {
			t.Errorf("error = %q, want JWT fallback error", got)
		}
	})

	t.Run("database error returns failed to validate", func(t *testing.T) {
		errStub := &stubProjectKeyLookup{
			publicProjects:  map[string]dbread.Project{},
			privateProjects: map[string]dbread.Project{},
			forceErr:        errors.New("connection refused"),
		}
		errAuthFunc := WithDualAuth([]byte("test-jwt-key"), nil, errStub)

		if _, err := errAuthFunc(ctx, newRequest("prv_valid456", "")); err == nil {
			t.Fatal("expected error for database failure")
		} else if got := err.Error(); !strings.Contains(got, "failed to validate API key") {
			t.Errorf("error = %q, want to contain %q", got, "failed to validate API key")
		}
	})
}
