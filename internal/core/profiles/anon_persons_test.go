package profiles_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"strings"

	"github.com/pug-sh/pug/internal/cookieless"
	"github.com/pug-sh/pug/internal/core/profiles"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

// TestProfilesList_IncludesAnonymousPersons pins the core derived-person
// contract: a distinct_id that only ever produced events (no identify call)
// lists as a first-class person — id = distinct_id, empty external_id, no
// traits — with its activity summary, create_time = first seen, and
// update_time = last seen. Get resolves the same person; GetByExternalID("")
// must never match one.
func TestProfilesList_IncludesAnonymousPersons(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-anon"
	first := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	last := first.Add(10 * time.Minute)

	session := uuid.NewString()
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-1", "page_view", session,
		map[string]string{"$browser": "Chrome", "$country": "US"}, map[string]string{}, first)
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-1", "click", session,
		map[string]string{"$browser": "Chrome", "$country": "US"}, map[string]string{}, last)

	// An identified profile created after the anon person's first-seen, so the
	// list order (create_time DESC) is deterministic.
	identifiedCreate := first.Add(20 * time.Minute)
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u-1", projectID, "ext-1", map[string]any{}, uint8(0), identifiedCreate, identifiedCreate,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "ext-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, identifiedCreate)

	service := profiles.NewService(nil, ch.Conn, nil)
	got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 persons (identified + anonymous), got %d", len(got))
	}
	if got[0].ID != "u-1" || got[1].ID != "anon-1" {
		t.Fatalf("order = [%s, %s], want [u-1, anon-1] (create_time DESC)", got[0].ID, got[1].ID)
	}

	anon := got[1]
	if anon.ExternalID != "" {
		t.Errorf("anon ExternalID = %q, want empty", anon.ExternalID)
	}
	if len(anon.Properties) != 0 {
		t.Errorf("anon Properties = %v, want empty", anon.Properties)
	}
	if anon.CreateTime.Unix() != first.Unix() {
		t.Errorf("anon CreateTime = %v, want first seen %v", anon.CreateTime, first)
	}
	if anon.UpdateTime.Unix() != last.Unix() {
		t.Errorf("anon UpdateTime = %v, want last seen %v", anon.UpdateTime, last)
	}
	if anon.Activity == nil {
		t.Fatal("anon Activity is nil, want populated summary")
	}
	if anon.Activity.TotalEvents != 2 {
		t.Errorf("anon TotalEvents = %d, want 2", anon.Activity.TotalEvents)
	}
	if anon.Activity.Pageviews != 1 {
		t.Errorf("anon Pageviews = %d, want 1 (click is not a page_view)", anon.Activity.Pageviews)
	}
	if anon.Activity.Sessions != 1 {
		t.Errorf("anon Sessions = %d, want 1", anon.Activity.Sessions)
	}
	if anon.Activity.Browser != "Chrome" {
		t.Errorf("anon Browser = %q, want Chrome", anon.Activity.Browser)
	}
	if anon.Activity.Country != "US" {
		t.Errorf("anon Country = %q, want US", anon.Activity.Country)
	}

	// Get resolves the derived person by its distinct_id.
	single, err := service.GetByID(ctx, projectID, "anon-1")
	if err != nil {
		t.Fatalf("GetByID(anon-1): %v", err)
	}
	if single.ID != "anon-1" || single.Activity == nil || single.Activity.TotalEvents != 2 {
		t.Errorf("GetByID(anon-1) = %+v, want derived person with 2 events", single)
	}

	// Unknown ids still 404, and the empty external_id can never match a person.
	if _, err := service.GetByID(ctx, projectID, "nope"); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("GetByID(nope) err = %v, want ErrProfileNotFound", err)
	}
	if _, err := service.GetByExternalID(ctx, projectID, ""); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("GetByExternalID(\"\") err = %v, want ErrProfileNotFound", err)
	}
}

// TestProfilesList_ClaimedIdentityExclusion pins the three claim rules and the
// non-resurrection property. After an identify merge (canonical profile +
// alias + soft-deleted anon tombstone), neither the anon distinct_id nor the
// external_id lists as its own person — and a LATE event for the merged anon
// id must not resurrect it (the alias keeps it claimed; persons are derived,
// so there is no row to un-delete). GetByID on the claimed id redirects to the
// canonical profile.
func TestProfilesList_ClaimedIdentityExclusion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-claim"
	preIdentify := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	postIdentify := preIdentify.Add(time.Hour)

	// End state of an identify merge, exactly as the identify worker writes it:
	// the canonical profile, the anon soft-delete tombstone, and the alias.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u-1", projectID, "ext-1", map[string]any{}, uint8(0), postIdentify, postIdentify,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"anon-1", projectID, "", map[string]any{}, uint8(1), postIdentify, postIdentify,
	); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "u-1", "ext-1", projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, preIdentify)
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "ext-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, postIdentify)

	service := profiles.NewService(nil, ch.Conn, nil)
	assertOnlyCanonical := func(stage string, wantEvents int64) {
		t.Helper()
		got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
		if err != nil {
			t.Fatalf("%s: List: %v", stage, err)
		}
		if len(got) != 1 || got[0].ID != "u-1" {
			ids := make([]string, len(got))
			for i, p := range got {
				ids[i] = p.ID
			}
			t.Fatalf("%s: persons = %v, want exactly [u-1] (claimed ids must not list)", stage, ids)
		}
		if got[0].Activity == nil || got[0].Activity.TotalEvents != wantEvents {
			t.Fatalf("%s: canonical TotalEvents = %+v, want %d (activity aggregates across claimed ids)", stage, got[0].Activity, wantEvents)
		}
	}

	assertOnlyCanonical("post-merge", 2)

	// A late event for the merged anon id re-fires the activity MV. It must
	// fold into the canonical profile, not resurrect an anonymous person.
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, postIdentify.Add(30*time.Minute))
	assertOnlyCanonical("after late event", 3)

	// The claimed id's URL keeps working: Get redirects to the canonical profile.
	redirected, err := service.GetByID(ctx, projectID, "anon-1")
	if err != nil {
		t.Fatalf("GetByID(anon-1): %v", err)
	}
	if redirected.ID != "u-1" {
		t.Errorf("GetByID(anon-1) resolved to %q, want canonical u-1", redirected.ID)
	}
}

// TestProfilesList_MixedKeysetPagination walks the (create_time DESC, id DESC)
// cursor across interleaved identified and anonymous persons: every person
// appears exactly once, in order, across pages.
func TestProfilesList_MixedKeysetPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-page"
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	// Interleaved create times: anon-a < u-1 < anon-b < u-2 < anon-c.
	times := map[string]time.Time{
		"anon-a": base,
		"u-1":    base.Add(1 * time.Minute),
		"anon-b": base.Add(2 * time.Minute),
		"u-2":    base.Add(3 * time.Minute),
		"anon-c": base.Add(4 * time.Minute),
	}
	for _, id := range []string{"anon-a", "anon-b", "anon-c"} {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, id, "page_view", uuid.NewString(),
			map[string]string{}, map[string]string{}, times[id])
	}
	for _, id := range []string{"u-1", "u-2"} {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, projectID, "ext-"+id, map[string]any{}, uint8(0), times[id], times[id],
		); err != nil {
			t.Fatalf("seed profile %s: %v", id, err)
		}
	}

	service := profiles.NewService(nil, ch.Conn, nil)
	var pages [][]string
	params := profiles.ListParams{ProjectID: projectID, PageSize: 2}
	for {
		got, err := service.List(ctx, params)
		if err != nil {
			t.Fatalf("List page %d: %v", len(pages), err)
		}
		if len(got) == 0 {
			break
		}
		ids := make([]string, len(got))
		for i, p := range got {
			ids[i] = p.ID
		}
		pages = append(pages, ids)
		last := got[len(got)-1]
		params.HasCursor = true
		params.CursorTime = last.CreateTime
		params.CursorID = last.ID
		if len(pages) > 5 {
			t.Fatalf("pagination did not terminate: %v", pages)
		}
	}

	want := [][]string{{"anon-c", "u-2"}, {"anon-b", "u-1"}, {"anon-a"}}
	if len(pages) != len(want) {
		t.Fatalf("pages = %v, want %v", pages, want)
	}
	for i := range want {
		if len(pages[i]) != len(want[i]) {
			t.Fatalf("page %d = %v, want %v", i, pages[i], want[i])
		}
		for j := range want[i] {
			if pages[i][j] != want[i][j] {
				t.Fatalf("page %d = %v, want %v", i, pages[i], want[i])
			}
		}
	}
}

// TestProfilesList_FiltersApplyToAnonymousPersons pins filter semantics on the
// unioned person set: activity (auto) filters match anonymous persons through
// their own summary; profile-property filters treat them as trait-less
// (EQUALS excludes them, IS_NOT_SET matches them).
func TestProfilesList_FiltersApplyToAnonymousPersons(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-filter"
	now := time.Now().UTC().Truncate(time.Second)

	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-chrome", "page_view", uuid.NewString(),
		map[string]string{"$browser": "Chrome"}, map[string]string{}, now)
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-safari", "page_view", uuid.NewString(),
		map[string]string{"$browser": "Safari"}, map[string]string{}, now)

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u-pro", projectID, "ext-pro", `{"plan": "pro"}`, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "ext-pro", "page_view", uuid.NewString(),
		map[string]string{"$browser": "Chrome"}, map[string]string{}, now)

	service := profiles.NewService(nil, ch.Conn, nil)
	listIDs := func(cond chq.Condition) map[string]bool {
		t.Helper()
		got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100, Filter: cond})
		if err != nil {
			t.Fatalf("List with filter: %v", err)
		}
		ids := make(map[string]bool, len(got))
		for _, p := range got {
			ids[p.ID] = true
		}
		return ids
	}

	// Activity filter ($browser = Chrome) — the same expression the profiles
	// RPC handler builds for PROPERTY_SOURCE_AUTO list filters.
	browserCond, err := chq.AutoPropertyConditionForColumns(&commonv1.PropertyFilter{
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_AUTO.Enum(),
		Property: proto.String("$browser"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("Chrome"),
	}, "coalesce(activity_summary.latest_browser, '')", "")
	if err != nil {
		t.Fatalf("build browser condition: %v", err)
	}
	if ids := listIDs(browserCond); len(ids) != 2 || !ids["anon-chrome"] || !ids["u-pro"] {
		t.Errorf("$browser=Chrome matched %v, want {anon-chrome, u-pro}", ids)
	}

	// Trait EQUALS excludes trait-less anonymous persons.
	planCond, err := chq.ProfilePropertyCondition(&commonv1.PropertyFilter{
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("pro"),
	})
	if err != nil {
		t.Fatalf("build plan condition: %v", err)
	}
	if ids := listIDs(planCond); len(ids) != 1 || !ids["u-pro"] {
		t.Errorf("plan=pro matched %v, want {u-pro}", ids)
	}

	// Trait IS_NOT_SET matches anonymous persons like any trait-less profile.
	notSetCond, err := chq.ProfilePropertyCondition(&commonv1.PropertyFilter{
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET.Enum(),
	})
	if err != nil {
		t.Fatalf("build not-set condition: %v", err)
	}
	if ids := listIDs(notSetCond); len(ids) != 2 || !ids["anon-chrome"] || !ids["anon-safari"] {
		t.Errorf("plan IS_NOT_SET matched %v, want {anon-chrome, anon-safari}", ids)
	}
}

// TestErasure_ByID_AnonymousPerson pins Delete-by-id for a derived anonymous
// person: the id has no Postgres row, so the prelude resolves it through the
// ClickHouse activity probe, freezes the bare distinct_id, and the worker
// erases events + rollups. A control person survives, and a repeat request
// after completion finds nothing left to erase.
func TestErasure_ByID_AnonymousPerson(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	now := time.Now().UTC().Truncate(time.Second)
	// A realistic full-length anonymous id (anon-<uuid>, 41 chars) so the ledger
	// insert path exercises migration 016's text column: char(20) would reject
	// this id on INSERT, and would blank-pad a shorter one and corrupt the
	// frozen fan-out. keepID is a control that must survive.
	eraseID := "anon-" + uuid.NewString()
	const keepID = "anon-keep-me"
	session := uuid.NewString()

	for range 2 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, eraseID, "page_view", session,
			map[string]string{}, map[string]string{}, now)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, keepID, "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	requestID, status, err := svc.RequestErasureByID(ctx, projectID, eraseID, "")
	if err != nil {
		t.Fatalf("RequestErasureByID(anon): %v", err)
	}
	if status != profiles.ComplianceStatusPending {
		t.Errorf("status = %q, want pending", status)
	}
	dr, err := svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if !dr.ProfileID.Valid || dr.ProfileID.String != eraseID {
		t.Errorf("ledger profile_id = %v, want %q (the anon id IS the person id)", dr.ProfileID, eraseID)
	}
	if dr.ExternalID.Valid {
		t.Errorf("ledger external_id = %q, want NULL", dr.ExternalID.String)
	}

	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}

	if got := chCount(t, ctx, ch, "SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, eraseID); got != 0 {
		t.Errorf("erased anon events remain: %d", got)
	}
	if got := chCount(t, ctx, ch, "SELECT count() FROM distinct_id_activity_states WHERE project_id = ? AND distinct_id = ?", projectID, eraseID); got != 0 {
		t.Errorf("erased anon activity states remain: %d", got)
	}
	if got := chCount(t, ctx, ch, "SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) = ?", projectID, session); got != 0 {
		t.Errorf("erased anon session rollup remains: %d", got)
	}
	if got := chCount(t, ctx, ch, "SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, keepID); got != 1 {
		t.Errorf("control events = %d, want 1", got)
	}

	dr, err = svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest after execute: %v", err)
	}
	if profiles.ComplianceStatus(dr.Status) != profiles.ComplianceStatusCompleted {
		t.Errorf("status = %q, want completed", dr.Status)
	}
	if dr.EventsAffected != 2 {
		t.Errorf("events_identified = %d, want 2", dr.EventsAffected)
	}

	// The person is gone from the read path...
	if _, err := svc.GetByID(ctx, projectID, eraseID); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("GetByID(erased) err = %v, want ErrProfileNotFound", err)
	}
	got, err := svc.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
	if err != nil {
		t.Fatalf("List after erasure: %v", err)
	}
	if len(got) != 1 || got[0].ID != keepID {
		t.Errorf("post-erasure persons = %d, want only %q", len(got), keepID)
	}

	// ...and a fresh request has nothing to key on (completed rows don't reopen).
	if _, _, err := svc.RequestErasureByID(ctx, projectID, eraseID, ""); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("repeat RequestErasureByID err = %v, want ErrProfileNotFound", err)
	}
}

// TestErasure_ByID_AliasResolvesToCanonicalSubject pins two behaviors: a
// by-id erasure of a claimed (merged) anonymous id erases the CANONICAL data
// subject with its full fan-out, and the profiles delete covers every frozen
// distinct_id — the merge tombstone row keyed by the anon id is physically
// removed, not just the canonical profile row.
func TestErasure_ByID_AliasResolvesToCanonicalSubject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)
	now := time.Now().UTC().Truncate(time.Second)

	const (
		externalID = "alias-erase@example.com"
		anonID     = "anon-alias-erase"
	)
	profileID := xid.New().String()

	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID: profileID, ProjectID: projectID, ExternalID: postgres.NewText(externalID), Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("seed pg profile: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		profileID, projectID, externalID, map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed ch profile: %v", err)
	}
	// The merge tombstone for the absorbed anon id — physically present in the
	// ReplacingMergeTree even though reads hide it.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		anonID, projectID, "", map[string]any{}, uint8(1), now, now,
	); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		anonID, profileID, externalID, projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	for _, e := range []struct{ distinctID string }{{externalID}, {externalID}, {anonID}, {anonID}} {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, e.distinctID, "page_view", uuid.NewString(),
			map[string]string{}, map[string]string{}, now)
	}

	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	// Erase by the ALIAS id — the request must record the canonical subject.
	requestID, _, err := svc.RequestErasureByID(ctx, projectID, anonID, "")
	if err != nil {
		t.Fatalf("RequestErasureByID(alias): %v", err)
	}
	dr, err := svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if !dr.ProfileID.Valid || dr.ProfileID.String != profileID {
		t.Errorf("ledger profile_id = %v, want canonical %q", dr.ProfileID, profileID)
	}

	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}

	if got := chCount(t, ctx, ch,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id IN (?, ?, ?)",
		projectID, externalID, anonID, profileID); got != 0 {
		t.Errorf("subject events remain: %d", got)
	}
	// The IN-delete must reach BOTH profile rows: canonical and the tombstone
	// keyed by the alias distinct_id.
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM profiles WHERE project_id = ? AND id IN (?, ?)",
		projectID, profileID, anonID); got != 0 {
		t.Errorf("profile rows remain (tombstone not covered by IN-delete?): %d", got)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM profile_aliases WHERE project_id = ? AND profile_id = ?", projectID, profileID); got != 0 {
		t.Errorf("aliases remain: %d", got)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM profiles WHERE id = $1", profileID); got != 0 {
		t.Errorf("pg profile remains: %d", got)
	}
	dr, err = svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest after execute: %v", err)
	}
	if profiles.ComplianceStatus(dr.Status) != profiles.ComplianceStatusCompleted {
		t.Errorf("status = %q, want completed", dr.Status)
	}
	if dr.EventsAffected != 4 {
		t.Errorf("events_identified = %d, want 4", dr.EventsAffected)
	}
}

// TestErasure_ByID_AcceptsExternalIDShapedInput pins the ladder's external_id
// shape: callers holding the id an events row displays (the external_id) can
// pass it to the by-id path and still get the full-subject erasure.
func TestErasure_ByID_AcceptsExternalIDShapedInput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)
	now := time.Now().UTC().Truncate(time.Second)

	const externalID = "ext-shaped@example.com"
	profileID := xid.New().String()

	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID: profileID, ProjectID: projectID, ExternalID: postgres.NewText(externalID), Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("seed pg profile: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		profileID, projectID, externalID, map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed ch profile: %v", err)
	}
	for range 2 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, externalID, "page_view", uuid.NewString(),
			map[string]string{}, map[string]string{}, now)
	}

	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)
	requestID, _, err := svc.RequestErasureByID(ctx, projectID, externalID, "")
	if err != nil {
		t.Fatalf("RequestErasureByID(external-id-shaped): %v", err)
	}
	dr, err := svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if !dr.ProfileID.Valid || dr.ProfileID.String != profileID {
		t.Errorf("ledger profile_id = %v, want resolved %q", dr.ProfileID, profileID)
	}
	if !dr.ExternalID.Valid || dr.ExternalID.String != externalID {
		t.Errorf("ledger external_id = %v, want %q", dr.ExternalID, externalID)
	}

	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, externalID); got != 0 {
		t.Errorf("events remain: %d", got)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM profiles WHERE project_id = ? AND id = ?", projectID, profileID); got != 0 {
		t.Errorf("ch profile remains: %d", got)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM profiles WHERE id = $1", profileID); got != 0 {
		t.Errorf("pg profile remains: %d", got)
	}
}

// TestProfilesList_CrossProjectIsolation pins the multi-tenant boundary on the
// NEW derived-person read path: the same anonymous distinct_id active in two
// projects must surface as two independent persons, and one project's activity
// must never fold into the other's. The erasure path has its own isolation test
// (TestErasure_CrossProjectIsolation); this guards the read CTEs.
func TestProfilesList_CrossProjectIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	const projectA, projectB = "proj-iso-a", "proj-iso-b"
	const sharedAnon = "anon-shared"

	// The same anon id, with DIFFERENT event counts per project: 1 in A, 2 in B.
	// A cross-project leak would show A's person with 3 events (or list B's).
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectA, sharedAnon, "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)
	for range 2 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectB, sharedAnon, "page_view", uuid.NewString(),
			map[string]string{}, map[string]string{}, now)
	}
	// An identified profile in B only — must not appear in A's list.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u-b", projectB, "ext-b", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	service := profiles.NewService(nil, ch.Conn, nil)

	gotA, err := service.List(ctx, profiles.ListParams{ProjectID: projectA, PageSize: 100})
	if err != nil {
		t.Fatalf("List(A): %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != sharedAnon {
		ids := make([]string, len(gotA))
		for i, p := range gotA {
			ids[i] = p.ID
		}
		t.Fatalf("List(A) = %v, want exactly [%s]", ids, sharedAnon)
	}
	if gotA[0].Activity == nil || gotA[0].Activity.TotalEvents != 1 {
		t.Errorf("List(A) person events = %+v, want 1 (no cross-project fold)", gotA[0].Activity)
	}

	// Get for the shared id in A must see only A's single event.
	singleA, err := service.GetByID(ctx, projectA, sharedAnon)
	if err != nil {
		t.Fatalf("GetByID(A): %v", err)
	}
	if singleA.Activity == nil || singleA.Activity.TotalEvents != 1 {
		t.Errorf("GetByID(A) events = %+v, want 1", singleA.Activity)
	}

	// B has the shared anon (2 events) plus the identified profile.
	gotB, err := service.List(ctx, profiles.ListParams{ProjectID: projectB, PageSize: 100})
	if err != nil {
		t.Fatalf("List(B): %v", err)
	}
	if len(gotB) != 2 {
		t.Fatalf("List(B) = %d persons, want 2 (shared anon + identified)", len(gotB))
	}
	for _, p := range gotB {
		if p.ID == sharedAnon && (p.Activity == nil || p.Activity.TotalEvents != 2) {
			t.Errorf("List(B) shared anon events = %+v, want 2", p.Activity)
		}
	}
}

// TestProfilesList_PaginationTieBreakAtEqualCreateTime exercises the cursor's
// secondary key. The mixed-pagination test uses all-distinct create_times, so
// the `create_time = ? AND id < ?` tie-break clause is never hit. Here three
// persons — two anonymous, one identified — share one create_time, forcing the
// tie-break across a page boundary. A broken secondary key drops or duplicates
// a person; every person must appear exactly once, in id-DESC order.
func TestProfilesList_PaginationTieBreakAtEqualCreateTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-tie"
	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	// anon-x and anon-z: one event each at ts → first_seen == ts.
	for _, id := range []string{"anon-x", "anon-z"} {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, id, "page_view", uuid.NewString(),
			map[string]string{}, map[string]string{}, ts)
	}
	// u-y: identified profile with create_time == ts.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u-y", projectID, "ext-y", map[string]any{}, uint8(0), ts, ts,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// ORDER BY create_time DESC, id DESC over the tie → id DESC: u-y, anon-z, anon-x.
	service := profiles.NewService(nil, ch.Conn, nil)
	seen := map[string]int{}
	var order []string
	params := profiles.ListParams{ProjectID: projectID, PageSize: 2}
	for i := 0; ; i++ {
		if i > 4 {
			t.Fatalf("pagination did not terminate: %v", order)
		}
		got, err := service.List(ctx, params)
		if err != nil {
			t.Fatalf("List page %d: %v", i, err)
		}
		if len(got) == 0 {
			break
		}
		for _, p := range got {
			seen[p.ID]++
			order = append(order, p.ID)
		}
		last := got[len(got)-1]
		params.HasCursor = true
		params.CursorTime = last.CreateTime
		params.CursorID = last.ID
	}

	want := []string{"u-y", "anon-z", "anon-x"}
	if len(order) != len(want) {
		t.Fatalf("paginated order = %v, want %v (each person exactly once)", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("paginated order = %v, want %v", order, want)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("person %q returned %d times across pages, want 1", id, n)
		}
	}
}

// TestProfilesList_ExcludesEmptyDistinctID pins the anonPersonsCTE
// `distinct_id != ”` guard: an event stream carrying a blank distinct_id must
// not materialize a single id=” anonymous person.
func TestProfilesList_ExcludesEmptyDistinctID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-empty-did"
	now := time.Now().UTC().Truncate(time.Second)

	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)
	// A real anon so a bug that drops the guard is distinguishable from an empty list.
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-real", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	service := profiles.NewService(nil, ch.Conn, nil)
	got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "anon-real" {
		ids := make([]string, len(got))
		for i, p := range got {
			ids[i] = p.ID
		}
		t.Fatalf("List = %v, want exactly [anon-real] (empty distinct_id must not list)", ids)
	}
}

// TestProfilesList_ClaimedByAliasOnly isolates the alias branch of claimed_ids.
// The main claim-exclusion test seeds BOTH a soft-delete tombstone profile row
// AND an alias for the anon id, so either claimed_ids branch alone would pass
// it. Here the anon id is claimed ONLY by an alias (no tombstone profile row),
// so this pins that the alias branch by itself excludes the id — and that a
// late event still does not resurrect it.
func TestProfilesList_ClaimedByAliasOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-alias-only"
	now := time.Now().UTC().Truncate(time.Second)

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u-1", projectID, "ext-1", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// Alias anon-1 → u-1, but deliberately NO tombstone profile row for anon-1,
	// so only the alias_id branch of claimed_ids can exclude it.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "u-1", "ext-1", projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "ext-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	service := profiles.NewService(nil, ch.Conn, nil)
	assertOnlyCanonical := func(stage string, wantEvents int64) {
		t.Helper()
		got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
		if err != nil {
			t.Fatalf("%s: List: %v", stage, err)
		}
		if len(got) != 1 || got[0].ID != "u-1" {
			ids := make([]string, len(got))
			for i, p := range got {
				ids[i] = p.ID
			}
			t.Fatalf("%s: persons = %v, want exactly [u-1] (alias-only claim must exclude anon-1)", stage, ids)
		}
		if got[0].Activity == nil || got[0].Activity.TotalEvents != wantEvents {
			t.Fatalf("%s: canonical events = %+v, want %d", stage, got[0].Activity, wantEvents)
		}
	}

	assertOnlyCanonical("alias-only", 2)

	// A late event for the alias-claimed anon must fold into the canonical, not
	// resurrect a standalone person — with no tombstone row to lean on.
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-1", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now.Add(time.Minute))
	assertOnlyCanonical("alias-only after late event", 3)
}

// TestErasure_ByID_EmptyIDReturnsNotFound pins the empty-id guard in the
// resolution ladder: an empty id can never name a person, so it must return
// ErrProfileNotFound and write no ledger row (rather than freezing an empty
// identifier set or matching a stray blank-distinct_id activity row).
func TestErasure_ByID_EmptyIDReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	if _, _, err := svc.RequestErasureByID(ctx, projectID, "", ""); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("RequestErasureByID(\"\") err = %v, want ErrProfileNotFound", err)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM compliance_requests WHERE project_id = $1", projectID); got != 0 {
		t.Errorf("compliance_requests rows = %d, want 0 (no ledger row for empty id)", got)
	}
}

// TestErasure_ByID_ChainedAliasFailsClosed pins the degraded-alias fail-closed
// path. A -> B -> C is an alias chain where the intermediate canonical B has no
// live Postgres row (soft-deleted as a later merge source) yet is itself an
// alias to C. Single-level fan-out cannot freeze the whole chain from A, so
// erasing by A must NOT silently erase only A's residual events and report the
// DSAR completed — it must fail closed with ErrDegradedAliasErasure and write
// no ledger row, surfacing the inconsistency for manual reconciliation.
func TestErasure_ByID_ChainedAliasFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	now := time.Now().UTC().Truncate(time.Second)

	const (
		aID = "anon-chain-a"
		bID = "anon-chain-b"
		cID = "canonical-chain-c"
	)
	// A -> B and B -> C: A resolves to B, and B is itself an alias (the chain).
	// Neither A nor B has a live Postgres profile row.
	for _, e := range []struct{ alias, profile string }{{aID, bID}, {bID, cID}} {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
			e.alias, e.profile, "", projectID,
		); err != nil {
			t.Fatalf("seed alias %s->%s: %v", e.alias, e.profile, err)
		}
	}
	// Residual activity for the presented id, so the fall-through would otherwise
	// find something to (partially) erase.
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, aID, "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	// A real nats client so the pre-fix fall-through (which would publish an
	// erase) fails as a clean assertion rather than a nil-client panic; post-fix
	// the request fails closed before any publish.
	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	requestID, _, err := svc.RequestErasureByID(ctx, projectID, aID, "")
	if !errors.Is(err, profiles.ErrDegradedAliasErasure) {
		t.Fatalf("RequestErasureByID(chained alias) err = %v, want ErrDegradedAliasErasure", err)
	}
	if requestID != "" {
		t.Errorf("requestID = %q, want empty on fail-closed", requestID)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM compliance_requests WHERE project_id = $1", projectID); got != 0 {
		t.Errorf("compliance_requests rows = %d, want 0 (fail closed writes no ledger row)", got)
	}
}

// TestErasure_ByID_TerminalAliasResidualCleanup guards the other side of the
// discriminator: an alias A -> B where B has no live Postgres row and is NOT
// itself an alias (a real canonical whose own erasure already completed) is the
// benign case. Erasing by A must still clean up A's residual events via a
// bare-id erasure (ledger row keyed by A), not fail closed.
func TestErasure_ByID_TerminalAliasResidualCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	now := time.Now().UTC().Truncate(time.Second)

	const (
		aID = "anon-terminal-a"
		bID = "canonical-terminal-b"
	)
	// A -> B only; B is a terminal (no B -> anything), and no live PG row for B.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		aID, bID, "", projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, aID, "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	requestID, _, err := svc.RequestErasureByID(ctx, projectID, aID, "")
	if err != nil {
		t.Fatalf("RequestErasureByID(terminal alias) err = %v, want nil (residual cleanup)", err)
	}
	dr, err := svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if !dr.ProfileID.Valid || dr.ProfileID.String != aID {
		t.Errorf("ledger profile_id = %v, want bare id %q", dr.ProfileID, aID)
	}
	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, aID); got != 0 {
		t.Errorf("residual events remain: %d", got)
	}
}

// TestErasureByExternalID_EmptyReturnsNotFound closes the asymmetry with the
// read path's GetByExternalID(""): an empty external_id can never name a
// subject, so DeleteDataSubject("") must return a clean ErrProfileNotFound and
// write no ledger row — not trip the compliance_requests CHECK constraint and
// surface as an opaque internal error.
func TestErasureByExternalID_EmptyReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	// nats/ch unused: the guard returns before any resolution or publish.
	svc := profiles.NewService(pg.PgW, nil, nil)

	if _, _, err := svc.RequestErasureByExternalID(ctx, projectID, "", ""); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("RequestErasureByExternalID(\"\") err = %v, want ErrProfileNotFound", err)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM compliance_requests WHERE project_id = $1", projectID); got != 0 {
		t.Errorf("compliance_requests rows = %d, want 0 (no ledger row for empty external_id)", got)
	}
}

// TestProfilesList_TombstoneClaimExclusionNoAlias isolates the profile-id
// branch of claimedIDsCTE. A soft-deleted (is_deleted=1) profile row whose
// distinct_id still has residual events, and which carries NO alias, must not
// resurface as a derived anonymous person — the tombstoned id is claimed by
// virtue of being a profile id (is_deleted is not consulted by claimedIDsCTE).
// Without this the "deleted user reappears" regression would pass unnoticed.
func TestProfilesList_TombstoneClaimExclusionNoAlias(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-tomb-noalias"
	now := time.Now().UTC().Truncate(time.Second)

	// Control: a live identified profile so the list is never trivially empty
	// (an empty result must not false-pass the exclusion assertion).
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"keep-1", projectID, "ext-keep", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed control profile: %v", err)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "ext-keep", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	// Tombstone: soft-deleted profile, no external_id, no alias, but residual
	// events keyed by its id.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"tomb-1", projectID, "", map[string]any{}, uint8(1), now, now,
	); err != nil {
		t.Fatalf("seed tombstone profile: %v", err)
	}
	for range 2 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "tomb-1", "page_view", uuid.NewString(),
			map[string]string{}, map[string]string{}, now)
	}

	service := profiles.NewService(nil, ch.Conn, nil)
	got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, len(got))
	for i, p := range got {
		ids[i] = p.ID
	}
	if len(got) != 1 || got[0].ID != "keep-1" {
		t.Fatalf("persons = %v, want exactly [keep-1] (tombstoned id must not resurface as an anon person)", ids)
	}
}

// TestErasure_ByID_NilClickHouseFailsLoud pins the deliberate removal of the
// nil-ClickHouse shortcut: an id with no Postgres profile row can only be
// resolved against the derived-person store, so with no ClickHouse conn the
// erasure must fail loudly rather than misreport ErrProfileNotFound — a false
// "not found" on a DSAR would tell a controller the subject has no data when
// the system merely could not check.
func TestErasure_ByID_NilClickHouseFailsLoud(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	svc := profiles.NewService(pg.PgW, nil, nil)

	_, _, err := svc.RequestErasureByID(ctx, projectID, "no-pg-row-id", "")
	if err == nil {
		t.Fatal("RequestErasureByID with nil ClickHouse err = nil, want a loud error")
	}
	if errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("err = %v, want a non-NotFound error (must not misreport a subject as absent)", err)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM compliance_requests WHERE project_id = $1", projectID); got != 0 {
		t.Errorf("compliance_requests rows = %d, want 0", got)
	}
}

// TestAnonPersons_CookielessIDsNeverBecomePersons pins migration 011's
// derived-persons exclusion: a cookieless- prefixed distinct_id (rotating
// daily, see internal/cookieless) must never surface as a person — without the
// activity-MV WHERE, every visitor-day would mint a ghost. The consented
// anon- visitor next to it proves the filter is prefix-scoped, not blanket.
func TestAnonPersons_CookielessIDsNeverBecomePersons(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	projectID := "proj-ckl-persons"
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "anon-real", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, cookieless.IDPrefix+"ghost", "page_view", uuid.NewString(),
		map[string]string{}, map[string]string{}, now)

	service := profiles.NewService(nil, ch.Conn, nil)

	got, err := service.List(ctx, profiles.ListParams{ProjectID: projectID, PageSize: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range got {
		if strings.HasPrefix(p.ID, cookieless.IDPrefix) {
			t.Errorf("cookieless id %q surfaced as a person — is migration 011's activity-MV WHERE intact?", p.ID)
		}
	}
	if len(got) != 1 || got[0].ID != "anon-real" {
		ids := make([]string, len(got))
		for i, p := range got {
			ids[i] = p.ID
		}
		t.Errorf("persons = %v, want exactly [anon-real]", ids)
	}

	if _, err := service.GetByID(ctx, projectID, cookieless.IDPrefix+"ghost"); !errors.Is(err, profiles.ErrProfileNotFound) {
		t.Errorf("GetByID(cookieless id) err = %v, want ErrProfileNotFound", err)
	}
}
