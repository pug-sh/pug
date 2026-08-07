package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func (s *stubProjectKeyLookup) InvalidateProjectKeys(_ context.Context, _ string, _ ...string) {}

func newStubLookup() *stubProjectKeyLookup {
	return &stubProjectKeyLookup{
		publicProjects: map[string]dbread.Project{
			"pub_valid123": {ID: "proj-1"},
		},
		privateProjects: map[string]dbread.Project{
			"prv_valid456": {ID: "proj-2"},
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
						// GetProjectByIDAndOrgMember scan order: CreateTime, DisplayName, FcmServiceJson, ID, OrgID, ReportingTimezone, UpdateTime
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

	t.Run("non-HS256 algorithm returns error", func(t *testing.T) {
		// Sign with HS384 — still HMAC, so the keyfunc's SigningMethodHMAC
		// check passes; only WithValidMethods("HS256") rejects it. This
		// isolates that the algorithm pin works, not just the keyfunc.
		raw := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.MapClaims{
			"sub": "cust-1",
			"aud": coreauth.Audience,
			"iss": coreauth.Issuer,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		token, err := raw.SignedString(jwtKey)
		if err != nil {
			t.Fatalf("failed to sign test JWT: %v", err)
		}
		if _, err := authFunc(ctx, newJWTRequest(token, "")); err == nil {
			t.Fatal("expected error for non-HS256 algorithm")
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

func TestWithPrivateKeyAuth(t *testing.T) {
	stub := newStubLookup()
	authFunc := WithPrivateKeyAuth(stub)
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
			t.Error("expected Customer to be nil for private key auth")
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
			t.Fatal("expected error for public key on private key auth")
		} else if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("private key via query param rejected (header only)", func(t *testing.T) {
		// A query-param credential is never read: the header is empty, so this
		// fails as a missing key regardless of the ?api_key value.
		if _, err := authFunc(ctx, newRequest("", "prv_valid456")); err == nil {
			t.Fatal("expected error for query-param key on private key auth")
		} else if got := err.Error(); !strings.Contains(got, "x-api-key header not present") {
			t.Errorf("error = %q, want to contain %q", got, "x-api-key header not present")
		}
	})

	t.Run("missing key returns error", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("", "")); err == nil {
			t.Fatal("expected error for missing key")
		} else if got := err.Error(); !strings.Contains(got, "x-api-key header not present") {
			t.Errorf("error = %q, want to contain %q", got, "x-api-key header not present")
		}
	})

	t.Run("invalid prefix rejected", func(t *testing.T) {
		if _, err := authFunc(ctx, newRequest("xyz_invalid", "")); err == nil {
			t.Fatal("expected error for invalid prefix")
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
		errAuthFunc := WithPrivateKeyAuth(errStub)

		if _, err := errAuthFunc(ctx, newRequest("prv_valid456", "")); err == nil {
			t.Fatal("expected error for database failure")
		} else if got := err.Error(); !strings.Contains(got, "failed to validate API key") {
			t.Errorf("error = %q, want to contain %q", got, "failed to validate API key")
		}
	})
}

// TestNormalizeBearerAPIKey pins the credential normalisation /mcp relies on. It
// lives here, at the owner, rather than only being covered transitively from the
// mcp package: the value this returns is stashed in the request context and later
// re-injected as the credential on every in-process tool call, so "what counts as
// the effective key" is a security-relevant contract.
func TestNormalizeBearerAPIKey(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string // x-api-key header
		authHeader string // Authorization header
		want       string // returned key: the effective credential
		// wantHeader is the x-api-key header left on the request, and it is asserted
		// separately from want because the two legitimately disagree: a public key is
		// never an effective credential (want "") yet stays on the request untouched for
		// the auth boundary to reject. It is the header, not the return value, that the
		// outer WithPrivateKeyAuth reads — so dropping the Set in NormalizeBearerAPIKey
		// would break Bearer auth for every MCP client while every want below still held.
		wantHeader string
	}{
		{"private x-api-key passes through", "prv_a", "", "prv_a", "prv_a"},
		// x-api-key must win. The function MUTATES the header, so if a Bearer could
		// overwrite an explicit x-api-key, anything able to inject an Authorization
		// header (a proxy, a gateway) could swap the caller onto another project's key.
		{"x-api-key wins over a conflicting bearer", "prv_a", "Bearer prv_b", "prv_a", "prv_a"},
		{"bearer prv is promoted", "", "Bearer prv_a", "prv_a", "prv_a"},
		{"bearer scheme is case-insensitive", "", "bEaReR prv_a", "prv_a", "prv_a"},
		// A public key is extractable from client apps and must never become the
		// effective credential, from either header.
		{"public x-api-key is not an effective key", "pub_a", "", "", "pub_a"},
		{"bearer pub is not promoted", "", "Bearer pub_a", "", ""},
		{"a JWT bearer is not promoted", "", "Bearer eyJhbGciOiJIUzI1NiJ9.e30.x", "", ""},
		{"no credential", "", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.apiKey != "" {
				req.Header.Set(HeaderAPIKey, tc.apiKey)
			}
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}

			if got := NormalizeBearerAPIKey(req); got != tc.want {
				t.Errorf("NormalizeBearerAPIKey() = %q, want %q", got, tc.want)
			}
			if got := req.Header.Get(HeaderAPIKey); got != tc.wantHeader {
				t.Errorf("resulting %s header = %q, want %q", HeaderAPIKey, got, tc.wantHeader)
			}
		})
	}
}
