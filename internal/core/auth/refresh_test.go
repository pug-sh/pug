package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// seedSignedInCustomer creates a password customer and returns an initial session.
func seedSignedInCustomer(t *testing.T, svc *auth.Service, write *dbwrite.Queries, id, email string) auth.Session {
	t.Helper()
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: id, Email: email, DisplayName: "", PictureUri: "", PasswordHash: string(hash),
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	session, err := svc.SignInWithEmail(ctx, email, "password123")
	if err != nil {
		t.Fatalf("SignInWithEmail: %v", err)
	}
	return session
}

func TestRefreshSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)

	t.Run("rotates and invalidates the old token", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		session := seedSignedInCustomer(t, svc, write, "rt-rotate", "rt-rotate@example.com")

		rotated, err := svc.RefreshSession(ctx, session.RefreshToken)
		if err != nil {
			t.Fatalf("RefreshSession: %v", err)
		}
		if rotated.AccessToken == "" || rotated.RefreshToken == "" {
			t.Fatal("expected a non-empty rotated pair")
		}
		if rotated.RefreshToken == session.RefreshToken {
			t.Fatal("refresh token must change on rotation")
		}

		// The new token works...
		if _, err := svc.RefreshSession(ctx, rotated.RefreshToken); err != nil {
			t.Fatalf("rotated token should still refresh: %v", err)
		}
	})

	t.Run("reuse of a consumed token revokes the whole family", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		session := seedSignedInCustomer(t, svc, write, "rt-reuse", "rt-reuse@example.com")

		// Rotate once: session.RefreshToken is now consumed, `next` is live.
		next, err := svc.RefreshSession(ctx, session.RefreshToken)
		if err != nil {
			t.Fatalf("first refresh: %v", err)
		}

		// Replay the consumed token → reuse detected → ErrInvalidToken.
		if _, err := svc.RefreshSession(ctx, session.RefreshToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("reused token err = %v, want ErrInvalidToken", err)
		}

		// And the family is dead: the previously-live `next` token is now revoked too.
		if _, err := svc.RefreshSession(ctx, next.RefreshToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("post-reuse live token err = %v, want ErrInvalidToken (family revoked)", err)
		}
	})

	t.Run("concurrent rotation of the same token trips reuse-detection and kills the family", func(t *testing.T) {
		// This pins WHY the frontend must single-flight refreshes. N callers present
		// the SAME refresh token at once. FOR UPDATE serializes them: the first
		// consumes+rotates; every later txn re-reads the row with consumed_at now set,
		// so it takes the reuse-detection branch (NOT the defensive ConsumeRefreshToken
		// lost-race branch, which is unreachable while the row lock is held) and revokes
		// the whole family. Net: exactly one immediate success, but the family — including
		// the winner's freshly minted successor — is dead afterward.
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		session := seedSignedInCustomer(t, svc, write, "rt-concurrent", "rt-concurrent@example.com")

		const n = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		sessions := make([]auth.Session, n)
		errs := make([]error, n)
		for i := range n {
			wg.Go(func() {
				<-start
				sessions[i], errs[i] = svc.RefreshSession(ctx, session.RefreshToken)
			})
		}
		close(start) // release together to maximize overlap
		wg.Wait()

		successes := 0
		var winner auth.Session
		for i, err := range errs {
			switch {
			case err == nil:
				successes++
				winner = sessions[i]
			case errors.Is(err, auth.ErrInvalidToken):
				// expected: losers see the consumed row → reuse-detection
			default:
				t.Errorf("unexpected error from concurrent refresh: %v", err)
			}
		}
		if successes != 1 {
			t.Fatalf("want exactly 1 successful rotation under contention, got %d", successes)
		}
		// The racers revoked the family, so the winner's token must now be rejected too.
		if _, err := svc.RefreshSession(ctx, winner.RefreshToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("winner token after concurrent reuse err = %v, want ErrInvalidToken (family revoked)", err)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		session := seedSignedInCustomer(t, svc, write, "rt-expired", "rt-expired@example.com")

		if _, err := db.PgW.Exec(ctx,
			"update refresh_tokens set expires_at = now() - interval '1 hour' where token_hash = $1",
			hashToken(session.RefreshToken),
		); err != nil {
			t.Fatalf("backdate expiry: %v", err)
		}

		if _, err := svc.RefreshSession(ctx, session.RefreshToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expired token err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("unknown token is rejected", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		if _, err := svc.RefreshSession(ctx, "not-a-real-token"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("unknown token err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("sign-out revokes the session", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		session := seedSignedInCustomer(t, svc, write, "rt-signout", "rt-signout@example.com")

		if err := svc.RevokeSession(ctx, session.RefreshToken); err != nil {
			t.Fatalf("RevokeSession: %v", err)
		}
		if _, err := svc.RefreshSession(ctx, session.RefreshToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("post sign-out refresh err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("sign-out with empty/unknown token is a no-op", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		if err := svc.RevokeSession(ctx, ""); err != nil {
			t.Fatalf("RevokeSession(empty): %v", err)
		}
		if err := svc.RevokeSession(ctx, "no-such-token"); err != nil {
			t.Fatalf("RevokeSession(unknown): %v", err)
		}
	})

	t.Run("access tokens carry the refresh family as sid", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		session := seedSignedInCustomer(t, svc, write, "rt-sid", "rt-sid@example.com")
		var family string
		if err := db.PgW.QueryRow(ctx, "select family_id from refresh_tokens where token_hash = $1",
			hashToken(session.RefreshToken)).Scan(&family); err != nil {
			t.Fatalf("read family: %v", err)
		}

		rotated, err := svc.RefreshSession(ctx, session.RefreshToken)
		if err != nil {
			t.Fatalf("RefreshSession: %v", err)
		}
		if got := sessionIDOf(t, session.AccessToken); got != family {
			t.Errorf("sign-in sid = %q, want the refresh family %q", got, family)
		}
		if got := sessionIDOf(t, rotated.AccessToken); got != family {
			t.Errorf("refresh sid = %q, want the refresh family %q", got, family)
		}
	})

	t.Run("an MCP refresh token gets no session and its sign-out revokes nothing", func(t *testing.T) {
		svc := mustNewTestAuthService(t, db, &stubPublisher{})
		seedSignedInCustomer(t, svc, write, "rt-mcp", "rt-mcp@example.com")
		org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-rt-mcp", DisplayName: "MCP"})
		if err != nil {
			t.Fatalf("CreateOrg: %v", err)
		}
		project, err := projects.CreateProjectInTx(ctx, write, org.ID, "default", "")
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		const refreshToken = "mcp_rt_test"
		if _, err := db.PgW.Exec(ctx, `insert into mcp_grants
			(client_id, client_name, code_challenge, code_expires_at, code_hash, code_used_at, customer_id, id, project_id, redirect_uri)
			values ('https://claude.ai/oauth/mcp-oauth-client-metadata', 'Claude', 'challenge', now(), $1, now(),
			'rt-mcp', 'grant-rt-mcp', $2, 'https://claude.ai/api/mcp/auth_callback')`,
			hashToken("mcp_code_test"), project.ID); err != nil {
			t.Fatalf("insert grant: %v", err)
		}
		if _, err := db.PgW.Exec(ctx, `insert into refresh_tokens
			(access_expires_at, access_token_hash, customer_id, expires_at, family_id, id, mcp_grant_id, token_hash)
			values (now() + interval '1 hour', $1, 'rt-mcp', now() + interval '90 days', 'grant-rt-mcp', 'rt-mcp-pair',
			'grant-rt-mcp', $2)`,
			hashToken("mcp_at_test"), hashToken(refreshToken)); err != nil {
			t.Fatalf("insert token pair: %v", err)
		}

		if _, err := svc.RefreshSession(ctx, refreshToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("RefreshSession(MCP token) err = %v, want ErrInvalidToken", err)
		}
		if err := svc.RevokeSession(ctx, refreshToken); err != nil {
			t.Fatalf("RevokeSession(MCP token): %v", err)
		}
		var consumed, revoked bool
		if err := db.PgW.QueryRow(ctx,
			"select consumed_at is not null, revoked_at is not null from refresh_tokens where id = 'rt-mcp-pair'",
		).Scan(&consumed, &revoked); err != nil {
			t.Fatalf("read token pair: %v", err)
		}
		if consumed || revoked {
			t.Fatalf("MCP row consumed = %v, revoked = %v; want both untouched", consumed, revoked)
		}
	})
}

// sessionIDOf reads an access token's sid claim.
func sessionIDOf(t *testing.T, accessToken string) string {
	t.Helper()
	var claims struct {
		jwt.RegisteredClaims
		SessionID string `json:"sid"`
	}
	if _, _, err := jwt.NewParser().ParseUnverified(accessToken, &claims); err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	return claims.SessionID
}
