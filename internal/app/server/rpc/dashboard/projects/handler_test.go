package projects

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// ctxWithCustomer returns a context with a JWT principal carrying the given customer.
func ctxWithCustomer(ctx context.Context, c dbread.Customer) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &c,
	})
}

// ctxWithProject returns a context with a JWT principal carrying the given project.
func ctxWithProject(ctx context.Context, p dbread.Project) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Project:  &p,
	})
}

// ctxWithCustomerProject carries BOTH a customer and a project, mirroring the real
// JWT+x-project-id principal. Project-lifecycle handlers need it: the admin gate
// resolves the caller's role from the project's org, which requires the customer.
func ctxWithCustomerProject(ctx context.Context, c dbread.Customer, p dbread.Project) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &c,
		Project:  &p,
	})
}

func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		if ae.Code() != want {
			t.Errorf("got code %v, want %v (err: %v)", ae.Code(), want, err)
		}
		return
	}
	if got := connect.CodeOf(err); got != want {
		t.Errorf("got code %v, want %v (err: %v)", got, want, err)
	}
}

func assertReason(t *testing.T, err error, want apperr.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Errorf("expected *apperr.Error to assert reason %q, got %T", want, err)
		return
	}
	if ae.Reason() != want {
		t.Errorf("got reason %q, want %q (err: %v)", ae.Reason(), want, err)
	}
}

// newIntegrationServer sets up a real Postgres + service + handler. Returns
// the handler, a seeded customer, and a seeded org backed by that customer as admin.
func newIntegrationServer(t *testing.T) (*server, dbread.Customer, string) {
	t.Helper()
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customerID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        customerID + "@test.example.com",
		DisplayName:  "Test User",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	customer, err := read.GetCustomerByID(ctx, customerID)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}

	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-"+orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("insert org member: %v", err)
	}

	return srv, customer, orgID
}

// ----- Delete: project not found → CodeNotFound + ReasonProjectNotFound ----

func TestHandler_Delete_ProjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// newIntegrationServer seeds the customer as org admin, so the admin gate
	// passes and the handler reaches the not-found path.
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	_, err := srv.Delete(
		ctxWithCustomerProject(ctx, customer, dbread.Project{ID: xid.New().String(), OrgID: orgID}),
		connect.NewRequest(&projectsv1.DeleteRequest{}),
	)
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonProjectNotFound)
}

// ----- UpdateMeta: project not found → CodeNotFound + ReasonProjectNotFound ----

func TestHandler_UpdateMeta_ProjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	_, err := srv.UpdateMeta(
		ctxWithCustomerProject(ctx, customer, dbread.Project{ID: xid.New().String(), OrgID: orgID}),
		connect.NewRequest(&projectsv1.UpdateMetaRequest{
			DisplayName: proto.String("new name"),
		}),
	)
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonProjectNotFound)
}

// ----- UpdateMeta: malformed timezone → CodeInvalidArgument (rejected, not coerced) ----

func TestHandler_UpdateMeta_InvalidTimezone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Admin caller so the gate passes; the zone is validated before any DB access,
	// so the project need not exist.
	srv, customer, orgID := newIntegrationServer(t)
	_, err := srv.UpdateMeta(
		ctxWithCustomerProject(context.Background(), customer, dbread.Project{ID: xid.New().String(), OrgID: orgID}),
		connect.NewRequest(&projectsv1.UpdateMetaRequest{
			DisplayName:       proto.String("name"),
			ReportingTimezone: proto.String("Not/A/Zone"), // passes proto charset, unknown to tzdata
		}),
	)
	assertCode(t, err, connect.CodeInvalidArgument)
	assertReason(t, err, apperr.ReasonInvalidTimezone)
}

// ----- UpdateMeta: "UTC" full-replaces (clears) the stored zone to "" ----
//
// UpdateMeta normalizes via tzx.Normalize (not a coalesce-preserve), so sending
// "UTC" must overwrite a previously-set zone with "" rather than preserving it.

func TestHandler_UpdateMeta_ClearsTimezoneToUTC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customerID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: customerID, Email: customerID + "@test.example.com", DisplayName: "U", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	customer, err := read.GetCustomerByID(ctx, customerID)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}
	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx, `INSERT INTO orgs (id, display_name) VALUES ($1, $2)`, orgID, "tz-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: orgID, CustomerID: customerID, Role: "ORG_ROLE_ADMIN"}); err != nil {
		t.Fatalf("insert org member: %v", err)
	}

	created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
		OrgId:             proto.String(orgID),
		DisplayName:       proto.String("tz project"),
		ReportingTimezone: proto.String("Asia/Kolkata"),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projectID := created.Msg.GetProject().GetId()

	// Create response must echo the stored zone on the read message.
	if got := created.Msg.GetProject().GetReportingTimezone(); got != "Asia/Kolkata" {
		t.Errorf("Create response ReportingTimezone = %q, want Asia/Kolkata", got)
	}

	if stored, err := projectsSvc.GetProjectByID(ctx, projectID); err != nil {
		t.Fatalf("GetProjectByID after create: %v", err)
	} else if stored.ReportingTimezone != "Asia/Kolkata" {
		t.Fatalf("after create ReportingTimezone = %q, want Asia/Kolkata", stored.ReportingTimezone)
	}

	// Get must also surface the stored zone (read-model mapper).
	if got, err := srv.Get(
		ctxWithProject(ctx, dbread.Project{ID: projectID, OrgID: orgID, ReportingTimezone: "Asia/Kolkata"}),
		connect.NewRequest(&projectsv1.GetRequest{}),
	); err != nil {
		t.Fatalf("Get: %v", err)
	} else if tz := got.Msg.GetProject().GetReportingTimezone(); tz != "Asia/Kolkata" {
		t.Errorf("Get response ReportingTimezone = %q, want Asia/Kolkata", tz)
	}

	updated, err := srv.UpdateMeta(
		ctxWithCustomerProject(ctx, customer, dbread.Project{ID: projectID, OrgID: orgID}),
		connect.NewRequest(&projectsv1.UpdateMetaRequest{
			DisplayName:       proto.String("tz project"),
			ReportingTimezone: proto.String("UTC"), // must clear to "", not preserve Asia/Kolkata
		}),
	)
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}

	// UpdateMeta response must echo the cleared zone as "" (not "UTC").
	if got := updated.Msg.GetProject().GetReportingTimezone(); got != "" {
		t.Errorf("UpdateMeta response ReportingTimezone = %q, want \"\" (UTC)", got)
	}

	stored, err := projectsSvc.GetProjectByID(ctx, projectID)
	if err != nil {
		t.Fatalf("GetProjectByID after update: %v", err)
	}
	if stored.ReportingTimezone != "" {
		t.Errorf("after clearing, ReportingTimezone = %q, want \"\" (UTC)", stored.ReportingTimezone)
	}
}

// ----- Create: admin required → CodePermissionDenied + ReasonOrgAdminRequired ----

// TestHandler_Create_NonAdminReturnsPermissionDenied verifies the admin gate on
// project Create that lives in the CreateProjectAsAdmin SQL CTE — a race-safe,
// service-level check distinct from (and defense-in-depth behind) the
// AuthzInterceptor. Every non-admin role — member AND viewer — is rejected at the
// CTE with ORG_ADMIN_REQUIRED.
func TestHandler_Create_NonAdminReturnsPermissionDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-perm"); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	seedRole := func(role string) dbread.Customer {
		id := xid.New().String()
		if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
			ID: id, Email: id + "@test.example.com", DisplayName: "U", PasswordHash: "x",
		}); err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID: orgID, CustomerID: id, Role: role,
		}); err != nil {
			t.Fatalf("seed member: %v", err)
		}
		c, err := read.GetCustomerByID(ctx, id)
		if err != nil {
			t.Fatalf("read customer: %v", err)
		}
		return c
	}

	for _, role := range []string{"ORG_ROLE_MEMBER", "ORG_ROLE_VIEWER"} {
		t.Run(role, func(t *testing.T) {
			caller := seedRole(role)
			_, err := srv.Create(
				ctxWithCustomer(ctx, caller),
				connect.NewRequest(&projectsv1.CreateRequest{
					OrgId:       proto.String(orgID),
					DisplayName: proto.String("new project " + role),
				}),
			)
			assertCode(t, err, connect.CodePermissionDenied)
			assertReason(t, err, apperr.ReasonOrgAdminRequired)
		})
	}
}

// ----- Create: duplicate project name → CodeAlreadyExists + ReasonProjectNameTaken ----

func TestHandler_Create_DuplicateNameReturnsAlreadyExists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	_, err := srv.Create(
		ctxWithCustomer(ctx, customer),
		connect.NewRequest(&projectsv1.CreateRequest{
			OrgId:       proto.String(orgID),
			DisplayName: proto.String("my project"),
		}),
	)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second create with the same name should conflict.
	_, err = srv.Create(
		ctxWithCustomer(ctx, customer),
		connect.NewRequest(&projectsv1.CreateRequest{
			OrgId:       proto.String(orgID),
			DisplayName: proto.String("my project"),
		}),
	)
	assertCode(t, err, connect.CodeAlreadyExists)
	assertReason(t, err, apperr.ReasonProjectNameTaken)
}

// ----- UpdateMeta: name-only update preserves the stored timezone -----
//
// Regression guard for the partial update: omitting reporting_timezone must NOT
// reset it to UTC (the pre-partial-update full-replace footgun). protovalidate is
// not in the direct-handler path, so this exercises handler+SQL preservation only.
func TestHandler_UpdateMeta_NameOnlyPreservesTimezone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
		OrgId:             proto.String(orgID),
		DisplayName:       proto.String("orig"),
		ReportingTimezone: proto.String("Asia/Kolkata"),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projectID := created.Msg.GetProject().GetId()

	updated, err := srv.UpdateMeta(
		ctxWithCustomerProject(ctx, customer, dbread.Project{ID: projectID, OrgID: orgID}),
		connect.NewRequest(&projectsv1.UpdateMetaRequest{
			DisplayName: proto.String("renamed"), // reporting_timezone omitted (nil)
		}),
	)
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got := updated.Msg.GetProject().GetDisplayName(); got != "renamed" {
		t.Errorf("display_name = %q, want renamed", got)
	}
	if got := updated.Msg.GetProject().GetReportingTimezone(); got != "Asia/Kolkata" {
		t.Errorf("reporting_timezone = %q, want Asia/Kolkata (preserved, not reset)", got)
	}
}

// ----- UpdateMeta: timezone-only update preserves the display name -----
func TestHandler_UpdateMeta_TimezoneOnlyPreservesName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
		OrgId:       proto.String(orgID),
		DisplayName: proto.String("keep me"), // timezone omitted → "" (UTC)
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projectID := created.Msg.GetProject().GetId()

	updated, err := srv.UpdateMeta(
		ctxWithCustomerProject(ctx, customer, dbread.Project{ID: projectID, OrgID: orgID}),
		connect.NewRequest(&projectsv1.UpdateMetaRequest{
			ReportingTimezone: proto.String("Asia/Kolkata"), // display_name omitted (nil)
		}),
	)
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got := updated.Msg.GetProject().GetDisplayName(); got != "keep me" {
		t.Errorf("display_name = %q, want 'keep me' (preserved)", got)
	}
	if got := updated.Msg.GetProject().GetReportingTimezone(); got != "Asia/Kolkata" {
		t.Errorf("reporting_timezone = %q, want Asia/Kolkata", got)
	}
}

// ----- UpdateMeta: an explicit empty reporting_timezone resets a non-UTC zone to UTC -----
//
// Distinct code path from the "UTC" string (TestHandler_UpdateMeta_ClearsTimezoneToUTC):
// a present "" is a non-nil pointer to empty, so it must enter the presence branch
// (req.Msg.ReportingTimezone != nil), pass tzx.Validate(""), and be WRITTEN as "" —
// not treated as omitted (which would preserve the old zone). Re-reads via
// GetProjectByID to assert persistence, not just the echoed RETURNING row.
func TestHandler_UpdateMeta_EmptyStringResetsTimezoneToUTC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customerID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: customerID, Email: customerID + "@test.example.com", DisplayName: "U", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	customer, err := read.GetCustomerByID(ctx, customerID)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}
	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx, `INSERT INTO orgs (id, display_name) VALUES ($1, $2)`, orgID, "tz-empty-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: orgID, CustomerID: customerID, Role: "ORG_ROLE_ADMIN"}); err != nil {
		t.Fatalf("insert org member: %v", err)
	}

	created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
		OrgId:             proto.String(orgID),
		DisplayName:       proto.String("tz empty project"),
		ReportingTimezone: proto.String("Asia/Kolkata"),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projectID := created.Msg.GetProject().GetId()

	updated, err := srv.UpdateMeta(
		ctxWithCustomerProject(ctx, customer, dbread.Project{ID: projectID, OrgID: orgID}),
		connect.NewRequest(&projectsv1.UpdateMetaRequest{
			ReportingTimezone: proto.String(""), // explicit empty → reset to UTC (display_name omitted)
		}),
	)
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got := updated.Msg.GetProject().GetReportingTimezone(); got != "" {
		t.Errorf("UpdateMeta response ReportingTimezone = %q, want \"\" (UTC)", got)
	}
	// display_name was omitted → must be preserved.
	if got := updated.Msg.GetProject().GetDisplayName(); got != "tz empty project" {
		t.Errorf("display_name = %q, want 'tz empty project' (preserved)", got)
	}

	stored, err := projectsSvc.GetProjectByID(ctx, projectID)
	if err != nil {
		t.Fatalf("GetProjectByID after update: %v", err)
	}
	if stored.ReportingTimezone != "" {
		t.Errorf("after empty-string reset, stored ReportingTimezone = %q, want \"\" (UTC)", stored.ReportingTimezone)
	}
}

// TestHandler_ProjectLifecycle_AdminAllowed exercises the admin happy path for the
// project-lifecycle handlers (Delete / UpdateMeta / UpdateFCMServiceJSON). The
// admin-only ROLE gate now lives in the AuthzInterceptor (see
// rpc.TestRoleGatedAdminOnlyRPCs), so this no longer asserts denials — it only
// confirms the handlers themselves run for an authorized caller.
func TestHandler_ProjectLifecycle_AdminAllowed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	newProject := func() dbread.Project {
		created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
			OrgId: proto.String(orgID), DisplayName: proto.String("p-" + xid.New().String()),
		}))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return dbread.Project{ID: created.Msg.GetProject().GetId(), OrgID: orgID}
	}

	p := newProject()
	if _, err := srv.UpdateMeta(ctxWithCustomerProject(ctx, customer, p), connect.NewRequest(&projectsv1.UpdateMetaRequest{DisplayName: proto.String("renamed")})); err != nil {
		t.Fatalf("admin UpdateMeta: %v", err)
	}
	if _, err := srv.UpdateFCMServiceJSON(ctxWithCustomerProject(ctx, customer, p), connect.NewRequest(&projectsv1.UpdateFCMServiceJSONRequest{FcmServiceJson: proto.String("{}")})); err != nil {
		t.Fatalf("admin UpdateFCMServiceJSON: %v", err)
	}
	if _, err := srv.Delete(ctxWithCustomerProject(ctx, customer, p), connect.NewRequest(&projectsv1.DeleteRequest{})); err != nil {
		t.Fatalf("admin Delete: %v", err)
	}
}

// ----- API keys: list / create / delete -----

// TestHandler_ApiKeys_Lifecycle walks the flow a user actually performs: a new
// project already has its public key, they mint a private one (seeing it exactly
// once), then revoke it.
func TestHandler_ApiKeys_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
		OrgId:       proto.String(orgID),
		DisplayName: proto.String("keys project"),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	project := created.Msg.GetProject()
	projectCtx := ctxWithCustomerProject(ctx, customer, dbread.Project{ID: project.GetId(), OrgID: orgID})

	t.Run("a new project lists only its starter public key", func(t *testing.T) {
		res, err := srv.ListApiKeys(projectCtx, connect.NewRequest(&projectsv1.ListApiKeysRequest{}))
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		keys := res.Msg.GetApiKeys()
		if len(keys) != 1 {
			t.Fatalf("got %d keys, want 1", len(keys))
		}
		if keys[0].GetKind() != projectsv1.ApiKeyKind_API_KEY_KIND_PUBLIC {
			t.Errorf("starter key kind = %v, want PUBLIC", keys[0].GetKind())
		}
		// ListApiKeys is the only way to read a project's keys — Project carries none.
		if !strings.HasPrefix(keys[0].GetKey(), "pub_") {
			t.Errorf("starter key = %q, want a pub_ key", keys[0].GetKey())
		}
	})

	var privateKeyID string
	t.Run("creating a private key returns it once", func(t *testing.T) {
		res, err := srv.CreateApiKey(projectCtx, connect.NewRequest(&projectsv1.CreateApiKeyRequest{
			Kind:        projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE.Enum(),
			DisplayName: proto.String("CI"),
		}))
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}
		privateKeyID = res.Msg.GetApiKey().GetId()

		if !strings.HasPrefix(res.Msg.GetKey(), "prv_") {
			t.Errorf("returned key = %q, want a prv_ key", res.Msg.GetKey())
		}
		// The response message itself must not carry the secret — only the
		// top-level key field does, and only here.
		if got := res.Msg.GetApiKey().GetKey(); got != "" {
			t.Errorf("ApiKey.key = %q, want empty for a private key", got)
		}
		if res.Msg.GetApiKey().GetMasked() == "" {
			t.Error("expected a mask to identify the key by later")
		}

		// ...and never again: a list must show only the mask.
		list, err := srv.ListApiKeys(projectCtx, connect.NewRequest(&projectsv1.ListApiKeysRequest{}))
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		for _, k := range list.Msg.GetApiKeys() {
			if k.GetKind() == projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE && k.GetKey() != "" {
				t.Errorf("ListApiKeys returned a private key value %q", k.GetKey())
			}
		}
		if len(list.Msg.GetApiKeys()) != 2 {
			t.Errorf("got %d keys, want 2", len(list.Msg.GetApiKeys()))
		}
	})

	t.Run("deleting the private key removes it", func(t *testing.T) {
		if _, err := srv.DeleteApiKey(projectCtx, connect.NewRequest(&projectsv1.DeleteApiKeyRequest{
			Id: proto.String(privateKeyID),
		})); err != nil {
			t.Fatalf("DeleteApiKey: %v", err)
		}

		list, err := srv.ListApiKeys(projectCtx, connect.NewRequest(&projectsv1.ListApiKeysRequest{}))
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		if len(list.Msg.GetApiKeys()) != 1 {
			t.Errorf("got %d keys after delete, want 1", len(list.Msg.GetApiKeys()))
		}
	})
}

func TestHandler_DeleteApiKey_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	created, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
		OrgId:       proto.String(orgID),
		DisplayName: proto.String("nf project"),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projectCtx := ctxWithCustomerProject(ctx, customer,
		dbread.Project{ID: created.Msg.GetProject().GetId(), OrgID: orgID})

	_, err = srv.DeleteApiKey(projectCtx, connect.NewRequest(&projectsv1.DeleteApiKeyRequest{
		Id: proto.String("nosuchkey00000000000"),
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonApiKeyNotFound)
}

// A key id is not a capability: presenting another project's key id must read as
// "not found" rather than revoking it.
func TestHandler_DeleteApiKey_IsProjectScoped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	newProject := func(name string) *projectsv1.Project {
		t.Helper()
		res, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&projectsv1.CreateRequest{
			OrgId:       proto.String(orgID),
			DisplayName: proto.String(name),
		}))
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return res.Msg.GetProject()
	}

	victim := newProject("victim")
	attacker := newProject("attacker")

	victimCtx := ctxWithCustomerProject(ctx, customer, dbread.Project{ID: victim.GetId(), OrgID: orgID})
	attackerCtx := ctxWithCustomerProject(ctx, customer, dbread.Project{ID: attacker.GetId(), OrgID: orgID})

	victimKeys, err := srv.ListApiKeys(victimCtx, connect.NewRequest(&projectsv1.ListApiKeysRequest{}))
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	victimKeyID := victimKeys.Msg.GetApiKeys()[0].GetId()

	_, err = srv.DeleteApiKey(attackerCtx, connect.NewRequest(&projectsv1.DeleteApiKeyRequest{
		Id: proto.String(victimKeyID),
	}))
	assertCode(t, err, connect.CodeNotFound)

	after, err := srv.ListApiKeys(victimCtx, connect.NewRequest(&projectsv1.ListApiKeysRequest{}))
	if err != nil {
		t.Fatalf("ListApiKeys after: %v", err)
	}
	if len(after.Msg.GetApiKeys()) != 1 {
		t.Errorf("victim now has %d keys — its key was deleted from another project's context", len(after.Msg.GetApiKeys()))
	}
}

// Every key RPC scopes to the x-project-id project. A principal without one is
// rejected before the service is reached — hence the nil service here, which
// would panic if a handler ever stopped resolving the project first.
func TestHandler_ApiKeys_RequireProjectScope(t *testing.T) {
	srv := NewServer(nil)
	ctx := ctxWithCustomer(context.Background(), dbread.Customer{ID: "cust-no-project"})

	_, err := srv.ListApiKeys(ctx, connect.NewRequest(&projectsv1.ListApiKeysRequest{}))
	assertCode(t, err, connect.CodeUnauthenticated)

	_, err = srv.CreateApiKey(ctx, connect.NewRequest(&projectsv1.CreateApiKeyRequest{
		Kind: projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE.Enum(),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)

	_, err = srv.DeleteApiKey(ctx, connect.NewRequest(&projectsv1.DeleteApiKeyRequest{Id: proto.String("k1")}))
	assertCode(t, err, connect.CodeUnauthenticated)
}
