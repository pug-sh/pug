package orgs

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// These tests are package-internal (package orgs) so they can assert against the
// unexported cache key. They exercise the Redis-backed GetMemberRole cache:
// positive-only population, cache-serving, and invalidation on the mutating
// paths.

func seedCacheCustomer(t *testing.T, w *dbwrite.Queries) string {
	t.Helper()
	id := xid.New().String()
	if _, err := w.CreateCustomer(context.Background(), dbwrite.CreateCustomerParams{
		ID:           id,
		Email:        id + "@example.com",
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	return id
}

func TestGetMemberRoleCaching(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	w := dbwrite.New(db.PgW)
	svc := NewServiceWithRoleCache(db.PgRO, db.PgW, nil, rd.Client)

	// freshOrg creates an org with a fresh admin so subtests don't interfere.
	freshOrg := func(t *testing.T) (orgID, adminID string) {
		t.Helper()
		adminID = seedCacheCustomer(t, w)
		org, err := svc.CreateOrgWithDefaults(ctx, adminID, "acme-"+adminID)
		if err != nil {
			t.Fatalf("CreateOrgWithDefaults: %v", err)
		}
		return org.ID, adminID
	}

	addMember := func(t *testing.T, orgID string, role Role) string {
		t.Helper()
		id := seedCacheCustomer(t, w)
		if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID:      orgID,
			CustomerID: id,
			Role:       role.String(),
		}); err != nil {
			t.Fatalf("seed member: %v", err)
		}
		return id
	}

	t.Run("positive cache populated and then served from cache", func(t *testing.T) {
		orgID, adminID := freshOrg(t)

		role, err := svc.GetMemberRole(ctx, orgID, adminID)
		if err != nil || role != RoleAdmin {
			t.Fatalf("GetMemberRole = (%q, %v), want (ADMIN, nil)", role, err)
		}
		if got := rd.Client.Get(ctx, memberRoleCacheKey(orgID, adminID)).Val(); got != RoleAdmin.String() {
			t.Fatalf("cache entry = %q, want %q", got, RoleAdmin.String())
		}

		// Change the role out-of-band (no service invalidation); the cached value
		// must shadow the DB until invalidated/expired — proving it is served from
		// cache, not re-read.
		if _, err := w.UpdateOrgMemberRole(ctx, dbwrite.UpdateOrgMemberRoleParams{
			OrgID:      orgID,
			CustomerID: adminID,
			Role:       RoleMember.String(),
		}); err != nil {
			t.Fatalf("out-of-band update: %v", err)
		}
		if role, _ := svc.GetMemberRole(ctx, orgID, adminID); role != RoleAdmin {
			t.Fatalf("after out-of-band DB change GetMemberRole = %q, want ADMIN (served from cache)", role)
		}
	})

	t.Run("non-membership is not cached (positive-only)", func(t *testing.T) {
		orgID, _ := freshOrg(t)
		stranger := seedCacheCustomer(t, w)

		if _, err := svc.GetMemberRole(ctx, orgID, stranger); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("GetMemberRole(stranger) err = %v, want ErrMemberNotFound", err)
		}
		if n, _ := rd.Client.Exists(ctx, memberRoleCacheKey(orgID, stranger)).Result(); n != 0 {
			t.Fatal("non-membership was cached; caching must be positive-only")
		}
	})

	t.Run("freshly added member is visible immediately (positive-only: no stale negative)", func(t *testing.T) {
		// Pins the property the un-invalidated add paths (invite acceptance, org
		// creation, the cross-package auth provisioning path) rely on: a non-member
		// lookup caches nothing, so adding a member needs no invalidation — the new
		// role is visible on the very next lookup. A regression that cached
		// non-membership would silently lock out every freshly-invited/provisioned
		// user until TTL; this catches it.
		orgID, _ := freshOrg(t)
		newcomer := seedCacheCustomer(t, w)

		// Lookup BEFORE membership exists: the miss must leave no cached entry.
		if _, err := svc.GetMemberRole(ctx, orgID, newcomer); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("pre-add GetMemberRole err = %v, want ErrMemberNotFound", err)
		}
		if n, _ := rd.Client.Exists(ctx, memberRoleCacheKey(orgID, newcomer)).Result(); n != 0 {
			t.Fatal("a non-member lookup cached a negative entry; caching must be positive-only")
		}

		// Add the member directly (mirrors the INSERT-only add paths, which do not
		// invalidate). The new role must resolve immediately.
		if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID:      orgID,
			CustomerID: newcomer,
			Role:       RoleMember.String(),
		}); err != nil {
			t.Fatalf("add member: %v", err)
		}
		if role, err := svc.GetMemberRole(ctx, orgID, newcomer); err != nil || role != RoleMember {
			t.Fatalf("post-add GetMemberRole = (%q, %v), want (MEMBER, nil) — stale negative cached?", role, err)
		}
	})

	t.Run("UpdateMemberRole invalidates the cache", func(t *testing.T) {
		orgID, _ := freshOrg(t)
		member := addMember(t, orgID, RoleMember)

		if role, _ := svc.GetMemberRole(ctx, orgID, member); role != RoleMember { // warm cache
			t.Fatalf("warm: GetMemberRole = %q, want MEMBER", role)
		}
		if _, err := svc.UpdateMemberRole(ctx, orgID, member, RoleAdmin); err != nil {
			t.Fatalf("UpdateMemberRole: %v", err)
		}
		if role, _ := svc.GetMemberRole(ctx, orgID, member); role != RoleAdmin {
			t.Fatalf("after promote GetMemberRole = %q, want ADMIN (cache must have been invalidated)", role)
		}
	})

	t.Run("RemoveMemberSafe invalidates the cache", func(t *testing.T) {
		orgID, _ := freshOrg(t)
		member := addMember(t, orgID, RoleMember)

		if role, _ := svc.GetMemberRole(ctx, orgID, member); role != RoleMember { // warm cache
			t.Fatalf("warm: GetMemberRole = %q, want MEMBER", role)
		}
		if err := svc.RemoveMemberSafe(ctx, orgID, member); err != nil {
			t.Fatalf("RemoveMemberSafe: %v", err)
		}
		if n, _ := rd.Client.Exists(ctx, memberRoleCacheKey(orgID, member)).Result(); n != 0 {
			t.Fatal("RemoveMemberSafe did not invalidate the cache entry")
		}
		if _, err := svc.GetMemberRole(ctx, orgID, member); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("after removal GetMemberRole err = %v, want ErrMemberNotFound", err)
		}
	})

	t.Run("Leave invalidates the cache", func(t *testing.T) {
		// Org has a fresh admin (freshOrg) plus this member, so the member is
		// neither the last admin nor the last member — Leave succeeds and must
		// invalidate the cached role. Leave gates the invalidation behind its
		// `n == 1` success branch, so this guards that wiring specifically.
		orgID, _ := freshOrg(t)
		member := addMember(t, orgID, RoleMember)

		if role, _ := svc.GetMemberRole(ctx, orgID, member); role != RoleMember { // warm cache
			t.Fatalf("warm: GetMemberRole = %q, want MEMBER", role)
		}
		if err := svc.Leave(ctx, orgID, member); err != nil {
			t.Fatalf("Leave: %v", err)
		}
		if n, _ := rd.Client.Exists(ctx, memberRoleCacheKey(orgID, member)).Result(); n != 0 {
			t.Fatal("Leave did not invalidate the cache entry")
		}
		if _, err := svc.GetMemberRole(ctx, orgID, member); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("after leave GetMemberRole err = %v, want ErrMemberNotFound", err)
		}
	})
}

// TestGetMemberRoleCorruptCacheSelfHeals pins the cache-read recovery branch: a
// cached value outside the recognized role set must NOT be trusted — GetMemberRole
// drops it and falls through to Postgres, returning the real role (and re-caching
// the valid value). Guards against a poisoned/format-drifted cache silently
// fabricating or denying a role.
func TestGetMemberRoleCorruptCacheSelfHeals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	w := dbwrite.New(db.PgW)
	svc := NewServiceWithRoleCache(db.PgRO, db.PgW, nil, rd.Client)

	adminID := seedCacheCustomer(t, w)
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "corrupt-cache-"+adminID)
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}

	// Poison the cache with a value outside the recognized role set.
	if err := rd.Client.Set(ctx, memberRoleCacheKey(org.ID, adminID), "ORG_ROLE_BOGUS", 0).Err(); err != nil {
		t.Fatalf("seed corrupt cache: %v", err)
	}

	role, err := svc.GetMemberRole(ctx, org.ID, adminID)
	if err != nil || role != RoleAdmin {
		t.Fatalf("GetMemberRole = (%q, %v), want (ADMIN, nil) — corrupt cache not self-healed", role, err)
	}
	// The bad entry must have been dropped and then repopulated with the valid role.
	if got := rd.Client.Get(ctx, memberRoleCacheKey(org.ID, adminID)).Val(); got != RoleAdmin.String() {
		t.Errorf("cache entry = %q, want %q (self-heal should rewrite it)", got, RoleAdmin.String())
	}
}
