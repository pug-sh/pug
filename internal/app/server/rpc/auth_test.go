package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/jackc/pgx/v5"
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
		if p.AuthType != AuthTypeAPIKey {
			t.Errorf("AuthType = %v, want %v", p.AuthType, AuthTypeAPIKey)
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
		_, err := authFunc(ctx, newRequest("", "prv_valid456"))
		if err == nil {
			t.Fatal("expected error for private key in query param")
		}
		if got := err.Error(); !strings.Contains(got, "beacon requests only support public API keys") {
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
		_, err := authFunc(ctx, newRequest("", ""))
		if err == nil {
			t.Fatal("expected error for missing key")
		}
	})

	t.Run("invalid prefix returns error", func(t *testing.T) {
		_, err := authFunc(ctx, newRequest("xyz_invalid", ""))
		if err == nil {
			t.Fatal("expected error for invalid prefix")
		}
		if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("valid prefix but nonexistent key returns error", func(t *testing.T) {
		_, err := authFunc(ctx, newRequest("pub_doesnotexist", ""))
		if err == nil {
			t.Fatal("expected error for nonexistent key")
		}
		if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("nonexistent private key returns error", func(t *testing.T) {
		_, err := authFunc(ctx, newRequest("prv_doesnotexist", ""))
		if err == nil {
			t.Fatal("expected error for nonexistent private key")
		}
		if got := err.Error(); !strings.Contains(got, "invalid API key") {
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

		_, err := errAuthFunc(ctx, newRequest("pub_valid123", ""))
		if err == nil {
			t.Fatal("expected error for database failure")
		}
		if got := err.Error(); !strings.Contains(got, "failed to validate API key") {
			t.Errorf("error = %q, want to contain %q", got, "failed to validate API key")
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
		if p.AuthType != AuthTypeAPIKey {
			t.Errorf("AuthType = %v, want %v", p.AuthType, AuthTypeAPIKey)
		}
	})

	t.Run("public key via header rejected", func(t *testing.T) {
		_, err := authFunc(ctx, newRequest("pub_valid123", ""))
		if err == nil {
			t.Fatal("expected error for public key on dual auth")
		}
		if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("nonexistent private key returns error", func(t *testing.T) {
		_, err := authFunc(ctx, newRequest("prv_doesnotexist", ""))
		if err == nil {
			t.Fatal("expected error for nonexistent private key")
		}
		if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("invalid prefix rejected", func(t *testing.T) {
		_, err := authFunc(ctx, newRequest("xyz_invalid", ""))
		if err == nil {
			t.Fatal("expected error for invalid prefix")
		}
		if got := err.Error(); !strings.Contains(got, "invalid API key") {
			t.Errorf("error = %q, want to contain %q", got, "invalid API key")
		}
	})

	t.Run("missing header falls back to JWT", func(t *testing.T) {
		// No API key header → falls back to JWT auth, which will fail
		// because there's no Authorization header.
		_, err := authFunc(ctx, newRequest("", ""))
		if err == nil {
			t.Fatal("expected error for missing auth")
		}
		if got := err.Error(); !strings.Contains(got, "Authorization header not present") {
			t.Errorf("error = %q, want JWT fallback error", got)
		}
	})
}
