package expiredtokens

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestRunDeletesExpiredTokensRegardlessOfStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	customerID := xid.New().String()
	if _, err := dbwrite.New(pg.PgW).CreateCustomer(t.Context(), dbwrite.CreateCustomerParams{
		ID: customerID, Email: "expired-token-cleanup@example.com",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	// The live used and revoked rows were marked 89 days ago and expire in an hour,
	// so deleting them after any grace under 89 days fails here.
	if _, err := pg.PgW.Exec(t.Context(), `
		insert into refresh_tokens (id, customer_id, family_id, token_hash, expires_at, consumed_at, revoked_at)
		select left(md5(token_hash), 20), $1, left(md5(token_hash), 20), token_hash, expires_at, consumed_at, revoked_at
		from (values
			('expired-active-refresh',   now() - interval '1 hour', null,                       null),
			('expired-consumed-refresh', now() - interval '1 hour', now() - interval '89 days', null),
			('expired-revoked-refresh',  now() - interval '1 hour', null,                       now() - interval '89 days'),
			('live-active-refresh',      now() + interval '1 hour', null,                       null),
			('live-consumed-refresh',    now() + interval '1 hour', now() - interval '89 days', null),
			('live-revoked-refresh',     now() + interval '1 hour', null,                       now() - interval '89 days')
		) as t(token_hash, expires_at, consumed_at, revoked_at)`,
		customerID); err != nil {
		t.Fatalf("seed refresh tokens: %v", err)
	}

	// live-superseded-invite has the shape ResendInvite leaves behind: used, not yet expired.
	if _, err := pg.PgW.Exec(t.Context(), `
		insert into email_action_tokens (id, email, purpose, token_hash, expires_at, consumed_at)
		select left(md5(token_hash), 20), 'someone@example.com', purpose, token_hash, expires_at, consumed_at
		from (values
			('expired-active-magic-link',   'magic_link', now() - interval '1 hour', null),
			('expired-consumed-magic-link', 'magic_link', now() - interval '1 hour', now() - interval '2 hours'),
			('expired-active-invite',       'org_invite', now() - interval '1 hour', null),
			('live-magic-link',             'magic_link', now() + interval '1 hour', null),
			('live-invite',                 'org_invite', now() + interval '6 days', null),
			('live-superseded-invite',      'org_invite', now() + interval '6 days', now() - interval '23 hours')
		) as t(token_hash, purpose, expires_at, consumed_at)`); err != nil {
		t.Fatalf("seed email action tokens: %v", err)
	}

	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var refreshHashes, emailHashes []string
	if err := pg.PgRO.QueryRow(t.Context(),
		"select array_agg(token_hash order by token_hash) from refresh_tokens").Scan(&refreshHashes); err != nil {
		t.Fatalf("read refresh tokens: %v", err)
	}
	if err := pg.PgRO.QueryRow(t.Context(),
		"select array_agg(token_hash order by token_hash) from email_action_tokens").Scan(&emailHashes); err != nil {
		t.Fatalf("read email action tokens: %v", err)
	}

	wantRefreshHashes := []string{"live-active-refresh", "live-consumed-refresh", "live-revoked-refresh"}
	if !slices.Equal(refreshHashes, wantRefreshHashes) {
		t.Errorf("refresh token hashes = %v, want %v", refreshHashes, wantRefreshHashes)
	}
	wantEmailHashes := []string{"live-invite", "live-magic-link", "live-superseded-invite"}
	if !slices.Equal(emailHashes, wantEmailHashes) {
		t.Errorf("email action token hashes = %v, want %v", emailHashes, wantEmailHashes)
	}
}

func TestRunDeletesDeadMCPGrants(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	ctx := t.Context()
	w := dbwrite.New(pg.PgW)

	customerID := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: customerID, Email: "mcp-grant-cleanup@example.com"}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "MCP"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	project, err := projects.CreateProjectInTx(ctx, w, org.ID, "default", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// spent's only pair expired, so the same pass deletes the pair and then the grant.
	if _, err := pg.PgW.Exec(ctx, `
		insert into mcp_grants (client_id, client_name, code_challenge, code_expires_at, code_hash, code_used_at,
			customer_id, id, project_id, redirect_uri, revoke_time)
		select 'https://claude.ai/oauth/mcp-oauth-client-metadata', 'Claude', 'challenge', code_expires_at, md5(id),
			code_used_at, $1, id, $2, 'https://claude.ai/api/mcp/auth_callback', revoke_time
		from (values
			('awaiting-exchange', now() + interval '4 minutes', null::timestamptz, null::timestamptz),
			('expired-unused',    now() - interval '1 minute',  null,                 null),
			('revoked',           now() - interval '1 day',     now() - interval '1 day', now() - interval '1 hour'),
			('live',              now() - interval '1 day',     now() - interval '1 day', null),
			('spent',             now() - interval '91 days',   now() - interval '91 days', null)
		) as t(id, code_expires_at, code_used_at, revoke_time)`,
		customerID, project.ID); err != nil {
		t.Fatalf("seed grants: %v", err)
	}
	if _, err := pg.PgW.Exec(ctx, `
		insert into refresh_tokens (access_expires_at, access_token_hash, customer_id, expires_at, family_id, id,
			mcp_grant_id, token_hash)
		select now() - interval '1 hour', 'at-' || grant_id, $1, expires_at, grant_id, left(md5(grant_id), 20),
			grant_id, 'rt-' || grant_id
		from (values
			('revoked', now() + interval '89 days'),
			('live',    now() + interval '89 days'),
			('spent',   now() - interval '1 day')
		) as t(grant_id, expires_at)`,
		customerID); err != nil {
		t.Fatalf("seed token pairs: %v", err)
	}

	if err := Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var grants, pairs []string
	if err := pg.PgRO.QueryRow(ctx, "select array_agg(id::text order by id) from mcp_grants").Scan(&grants); err != nil {
		t.Fatalf("read grants: %v", err)
	}
	if err := pg.PgRO.QueryRow(ctx, "select array_agg(token_hash order by token_hash) from refresh_tokens").Scan(&pairs); err != nil {
		t.Fatalf("read token pairs: %v", err)
	}
	if want := []string{"awaiting-exchange", "live"}; !slices.Equal(grants, want) {
		t.Errorf("grants = %v, want %v", grants, want)
	}
	if want := []string{"rt-live"}; !slices.Equal(pairs, want) {
		t.Errorf("token pairs = %v, want %v", pairs, want)
	}
}

func TestRunFailsWithoutPostgres(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://pug@127.0.0.1:1/pug")
	if err := Run(t.Context()); !errors.As(err, new(*pgconn.ConnectError)) {
		t.Fatalf("Run = %v, want a Postgres connect error", err)
	}
}

func TestRunExitsZeroWhileAnotherPassHoldsTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())

	customerID := xid.New().String()
	if _, err := dbwrite.New(pg.PgW).CreateCustomer(t.Context(), dbwrite.CreateCustomerParams{
		ID: customerID, Email: "expired-token-lock@example.com",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	const expiredHash = "expired-token-must-survive-lock-contention"
	if _, err := pg.PgW.Exec(t.Context(), `
		insert into refresh_tokens (id, customer_id, family_id, token_hash, expires_at)
		values ($1, $2, $3, $4, now() - interval '1 hour')`,
		xid.New().String(), customerID, xid.New().String(), expiredHash); err != nil {
		t.Fatalf("seed expired refresh token: %v", err)
	}

	ctx := t.Context()
	holder, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if acquired, err := dbwrite.New(holder).TryCronLock(ctx, int64(cron.LockExpiredTokens)); err != nil || !acquired {
		t.Fatalf("TryCronLock = %t, %v", acquired, err)
	}
	if err := Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil while another pass holds the lock", err)
	}

	var remaining int
	if err := pg.PgRO.QueryRow(ctx, "select count(*) from refresh_tokens where token_hash = $1", expiredHash).Scan(&remaining); err != nil {
		t.Fatalf("count locked token: %v", err)
	}
	if remaining != 1 {
		t.Errorf("expired token rows after lock contention = %d, want 1", remaining)
	}
}
