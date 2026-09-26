package domains

import (
	"errors"
	"strings"
	"testing"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestVerifyShowRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	cli, err := New(t.Context())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cli.Close)

	var verify strings.Builder
	if err := cli.Verify(t.Context(), &verify, org.ID, "ACME.com."); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, want := range []string{org.ID, `"acme"`, "verified by operator", "off", "allowed", "never"} {
		if !strings.Contains(verify.String(), want) {
			t.Errorf("Verify output is missing %q:\n%s", want, verify.String())
		}
	}
	if _, err := cli.svc.SetDomainSettings(t.Context(), org.ID, coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleMember}); err != nil {
		t.Fatal(err)
	}
	var show strings.Builder
	if err := cli.Show(t.Context(), &show, "acme.com"); err != nil || !strings.Contains(show.String(), coreorgs.RoleMember.String()) || !strings.Contains(show.String(), "restricted") {
		t.Errorf("Show = %q, %v; want auto-join as member and org creation restricted", show.String(), err)
	}
	if err := cli.Show(t.Context(), &show, "localhost"); !errors.Is(err, coreorgs.ErrDomainInvalid) {
		t.Errorf("Show(localhost) err = %v, want ErrDomainInvalid", err)
	}
	if err := cli.Verify(t.Context(), &verify, xid.New().String(), "acme.com"); !errors.Is(err, coreorgs.ErrOrgNotFound) {
		t.Errorf("Verify for an unknown org err = %v, want ErrOrgNotFound", err)
	}

	var release strings.Builder
	if err := cli.Release(t.Context(), &release, org.ID, "acme.com"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !strings.Contains(release.String(), "no org has added this domain") {
		t.Errorf("Release output = %q", release.String())
	}
	if err := cli.Release(t.Context(), &release, org.ID, "acme.com"); !errors.Is(err, coreorgs.ErrDomainNotFound) {
		t.Errorf("second release err = %v, want ErrDomainNotFound", err)
	}
}

func TestUnenforce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	w := dbwrite.New(pg.PgW)
	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	cli, err := New(t.Context())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cli.Close)
	d, err := cli.svc.VerifyDomainByOperator(t.Context(), org.ID, "acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := coreorgs.MarkSSOSeenInTx(t.Context(), w, "acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.svc.UpdateDomain(t.Context(), org.ID, d.ID, true); err != nil {
		t.Fatal(err)
	}

	var show strings.Builder
	if err := cli.Show(t.Context(), &show, "acme.com"); err != nil || !strings.Contains(show.String(), "REQUIRE SSO") || !strings.HasSuffix(strings.TrimSpace(show.String()), "on") {
		t.Fatalf("Show = %q, %v; want Require SSO on", show.String(), err)
	}
	var out strings.Builder
	if err := cli.Unenforce(t.Context(), &out, "ACME.com"); err != nil {
		t.Fatalf("Unenforce: %v", err)
	}
	if !strings.Contains(out.String(), "turned off in 1 org") || !strings.HasSuffix(strings.TrimSpace(out.String()), "off") {
		t.Errorf("Unenforce output = %q", out.String())
	}
	if err := coreorgs.CheckSignInInTx(t.Context(), w, "bob@acme.com", ""); err != nil {
		t.Errorf("after unenforce: %v", err)
	}
	if err := cli.Unenforce(t.Context(), &out, "acme.io"); !errors.Is(err, coreorgs.ErrDomainNotFound) {
		t.Errorf("Unenforce(unclaimed) err = %v, want ErrDomainNotFound", err)
	}
}
