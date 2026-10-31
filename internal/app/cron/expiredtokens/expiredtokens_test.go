package expiredtokens

import (
	"slices"
	"testing"

	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/app/cron"
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

	_, err := pg.PgW.Exec(t.Context(), `
		insert into refresh_tokens (id, customer_id, family_id, token_hash, expires_at, consumed_at, revoked_at)
		values
			($1, $2, $3, $4, now() - interval '1 hour', now(), null),
			($5, $2, $6, $7, now() - interval '1 hour', null, now()),
			($8, $2, $9, $10, now() + interval '1 hour', null, null),
			($11, $2, $12, $13, now() + interval '1 hour', now(), null),
			($14, $2, $15, $16, now() + interval '1 hour', null, now())`,
		xid.New().String(), customerID, xid.New().String(), "expired-consumed-refresh",
		xid.New().String(), xid.New().String(), "expired-revoked-refresh",
		xid.New().String(), xid.New().String(), "live-active-refresh",
		xid.New().String(), xid.New().String(), "live-consumed-refresh",
		xid.New().String(), xid.New().String(), "live-revoked-refresh")
	if err != nil {
		t.Fatalf("seed refresh tokens: %v", err)
	}

	_, err = pg.PgW.Exec(t.Context(), `
		insert into email_action_tokens (id, email, purpose, token_hash, expires_at, consumed_at)
		values
			($1, 'expired@example.com', 'magic_link', $2, now() - interval '1 hour', now()),
			($3, 'expired@example.com', 'org_invite', $4, now() - interval '1 hour', null),
			($5, 'live@example.com', 'magic_link', $6, now() + interval '1 hour', null),
			($7, 'live@example.com', 'org_invite', $8, now() + interval '1 hour', null)`,
		xid.New().String(), "expired-consumed-magic-link",
		xid.New().String(), "expired-active-invite",
		xid.New().String(), "live-magic-link",
		xid.New().String(), "live-invite")
	if err != nil {
		t.Fatalf("seed email action tokens: %v", err)
	}

	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var refreshHashes, emailHashes []string
	rows, err := pg.PgRO.Query(t.Context(), "select token_hash from refresh_tokens order by token_hash")
	if err != nil {
		t.Fatalf("query refresh token hashes: %v", err)
	}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			t.Fatalf("scan refresh token hash: %v", err)
		}
		refreshHashes = append(refreshHashes, hash)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate refresh token hashes: %v", err)
	}
	rows.Close()

	rows, err = pg.PgRO.Query(t.Context(), "select token_hash from email_action_tokens order by token_hash")
	if err != nil {
		t.Fatalf("query email action token hashes: %v", err)
	}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			t.Fatalf("scan email action token hash: %v", err)
		}
		emailHashes = append(emailHashes, hash)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate email action token hashes: %v", err)
	}
	rows.Close()

	wantRefreshHashes := []string{"live-active-refresh", "live-consumed-refresh", "live-revoked-refresh"}
	if !slices.Equal(refreshHashes, wantRefreshHashes) {
		t.Errorf("refresh token hashes = %v, want %v", refreshHashes, wantRefreshHashes)
	}
	wantEmailHashes := []string{"live-invite", "live-magic-link"}
	if !slices.Equal(emailHashes, wantEmailHashes) {
		t.Errorf("email action token hashes = %v, want %v", emailHashes, wantEmailHashes)
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
