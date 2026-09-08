package billing

import (
	"errors"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

const actor = "praveen/INV-1"

// newBilling wires the process environment New reads, since the CLI opens its own
// pools, and hands back a command object beside a fresh org to run it against.
func newBilling(t *testing.T) (*CLI, string) {
	t.Helper()
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("PUG_BILLING_ENABLED", "true")

	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))

	cli, err := New(t.Context())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cli.Close)
	return cli, org.ID
}

func TestShowAnOrgWithNoRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cli, orgID := newBilling(t)
	var out strings.Builder
	if err := cli.Show(t.Context(), &out, orgID, true); err != nil {
		t.Fatalf("Show: %v", err)
	}
	got := out.String()
	for _, want := range []string{orgID, `"acme"`, "RESOLVED", "STORED", "(no row", "HISTORY", "(no recorded changes)"} {
		if !strings.Contains(got, want) {
			t.Errorf("Show output is missing %q:\n%s", want, got)
		}
	}
}

// The gap between resolved and stored is where the interesting bugs live, so a
// grant has to print both halves.
func TestSetThenShowReportsBothHalves(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cli, orgID := newBilling(t)
	events := int64(5_000_000)
	name := "Acme Enterprise"
	note := "$400/mo, INV-123"

	var set strings.Builder
	if err := cli.Set(t.Context(), &set, orgID, actor, corebilling.Change{
		PlanSlug:       corebilling.SlugCustom,
		IncludedEvents: &events,
		DisplayName:    &name,
		Note:           &note,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(set.String(), name) {
		t.Errorf("Set output is missing the granted display name:\n%s", set.String())
	}

	var show strings.Builder
	if err := cli.Show(t.Context(), &show, orgID, true); err != nil {
		t.Fatalf("Show: %v", err)
	}
	got := show.String()
	for _, want := range []string{corebilling.SlugCustom, "5,000,000", note, actor} {
		if !strings.Contains(got, want) {
			t.Errorf("Show output is missing %q:\n%s", want, got)
		}
	}
}

func TestExtendTrialReportsTheNewEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cli, orgID := newBilling(t)
	var out strings.Builder
	// Bracketed, because the command reads its own clock: a UTC midnight crossing
	// between the two reads would otherwise fail a correct write.
	before := time.Now()
	if err := cli.ExtendTrial(t.Context(), &out, orgID, actor, 30); err != nil {
		t.Fatalf("ExtendTrial: %v", err)
	}
	wants := []string{
		before.AddDate(0, 0, 30).UTC().Format(time.DateOnly),
		time.Now().AddDate(0, 0, 30).UTC().Format(time.DateOnly),
	}
	if !strings.Contains(out.String(), wants[0]) && !strings.Contains(out.String(), wants[1]) {
		t.Errorf("ExtendTrial output is missing the new trial end %v:\n%s", wants, out.String())
	}
}

// Clear reports the empty record its own transaction just wrote, not a re-read:
// against a replica the re-read could still show the deleted row.
func TestClearReturnsToTheFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cli, orgID := newBilling(t)
	events := int64(1_000_000)
	if err := cli.Set(t.Context(), &strings.Builder{}, orgID, actor,
		corebilling.Change{PlanSlug: corebilling.SlugCustom, IncludedEvents: &events}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var out strings.Builder
	if err := cli.Clear(t.Context(), &out, orgID, actor); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !strings.Contains(out.String(), "(no row") {
		t.Errorf("Clear output still reports a stored row:\n%s", out.String())
	}
}

// An org id an operator mistyped must say so rather than report the floors for
// an org that does not exist.
func TestUnknownOrgIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cli, _ := newBilling(t)
	err := cli.Show(t.Context(), &strings.Builder{}, "o_nope", false)
	if !errors.Is(err, corebilling.ErrOrgNotFound) {
		t.Fatalf("Show on an unknown org = %v, want ErrOrgNotFound", err)
	}
}

// The operator's ordinary typos: the error surfaces, and no report is printed for
// a row that was not written.
func TestRefusedMutationsReportTheirReason(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cli, orgID := newBilling(t)
	var out strings.Builder

	if err := cli.Set(t.Context(), &out, orgID, actor, corebilling.Change{PlanSlug: "no-such-tier"}); !errors.Is(err, corebilling.ErrPlanNotFound) {
		t.Errorf("Set on an unknown slug = %v, want ErrPlanNotFound", err)
	}
	if err := cli.ExtendTrial(t.Context(), &out, orgID, actor, 0); !errors.Is(err, corebilling.ErrTrialDaysRange) {
		t.Errorf("ExtendTrial with no days = %v, want ErrTrialDaysRange", err)
	}
	if err := cli.Clear(t.Context(), &out, orgID, actor); !errors.Is(err, corebilling.ErrNoEntitlement) {
		t.Errorf("Clear on an org with no row = %v, want ErrNoEntitlement", err)
	}
	if out.Len() != 0 {
		t.Errorf("a refused mutation printed a report:\n%s", out.String())
	}
}
