package billing

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

func testOrg() dbread.Org {
	return dbread.Org{
		ID:          "o_2f9k",
		DisplayName: "Acme Inc",
		CreateTime:  pgtype.Timestamptz{Time: time.Date(2026, 1, 14, 9, 0, 0, 0, time.UTC), Valid: true},
	}
}

func render(t *testing.T, ent corebilling.Entitlement, rec corebilling.Record, history []corebilling.HistoryEntry) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeReport(&buf, testOrg(), ent, rec, history); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	return buf.String()
}

// line returns the value of one labelled line, scoped to a section: the same label
// appears under RESOLVED and STORED, and an unscoped lookup reads the resolved one
// and passes while the stored value is wrong.
func line(t *testing.T, out, section, label string) string {
	t.Helper()
	in := section == ""
	for l := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case trimmed == "":
			continue
		case isSectionHeader(trimmed):
			in = strings.HasPrefix(trimmed, section)
			continue
		case in && strings.HasPrefix(trimmed, label):
			return strings.TrimSpace(strings.TrimPrefix(trimmed, label))
		}
	}
	t.Fatalf("no %q line under %q in:\n%s", label, section, out)
	return ""
}

func isSectionHeader(line string) bool {
	for _, s := range []string{"RESOLVED", "STORED", "HISTORY"} {
		if strings.HasPrefix(line, s) {
			return true
		}
	}
	return false
}

// Absent means NO quota and NO list price. Rendering either as 0 states a
// billing figure the deployment never claimed.
func TestReportNeverRendersAbsentAsZero(t *testing.T) {
	out := render(t, corebilling.Entitlement{
		Slug: "custom", DisplayName: "Custom", Currency: "USD", Status: corebilling.StatusActive,
		BillingEnabled: true,
	}, corebilling.Record{}, nil)

	if got := line(t, out, "RESOLVED", "included events"); got != none {
		t.Fatalf("included events = %q, want %q", got, none)
	}
	if got := line(t, out, "RESOLVED", "list price"); got != none {
		t.Fatalf("list price = %q, want %q", got, none)
	}
	if got := line(t, out, "RESOLVED", "retention"); got != none {
		t.Fatalf("retention = %q, want %q", got, none)
	}
}

// An operator reads "2,555 days" as a typo more readily than as seven years, so
// the years are printed beside a whole multiple.
func TestReportRetentionNamesTheYears(t *testing.T) {
	for days, want := range map[int64]string{
		365:   "365 days  (1 year)",
		2_555: "2,555 days  (7 years)",
		400:   "400 days",
	} {
		out := render(t, corebilling.Entitlement{
			Slug: "scale", DisplayName: "Scale", Currency: "USD", Status: corebilling.StatusActive,
			RetentionDays: &days, BillingEnabled: true,
		}, corebilling.Record{}, nil)
		if got := line(t, out, "RESOLVED", "retention"); got != want {
			t.Errorf("retention for %d days = %q, want %q", days, got, want)
		}
	}
}

// Zero is a real price -- the two floors -- and must not read as absence.
func TestReportRendersZeroPrice(t *testing.T) {
	zero := int64(0)
	free := int64(10_000)
	out := render(t, corebilling.Entitlement{
		Slug: "free", DisplayName: "Free", Currency: "USD", Status: corebilling.StatusFree,
		PriceCents: &zero, IncludedEvents: &free, BillingEnabled: true,
	}, corebilling.Record{}, nil)

	if got := line(t, out, "RESOLVED", "list price"); got != "$0.00 USD" {
		t.Fatalf("list price = %q, want $0.00 USD", got)
	}
	if got := line(t, out, "RESOLVED", "included events"); got != "10,000" {
		t.Fatalf("included events = %q, want 10,000", got)
	}
}

// The stored instant is the day after the one an operator typed. Printing the
// pair is what stops that reading as an off-by-one.
func TestReportContractEndNamesTheLastDayCovered(t *testing.T) {
	ends := corebilling.ContractEndExclusive(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	out := render(t, corebilling.Entitlement{
		Slug: "custom", DisplayName: "Custom", Currency: "USD", ContractEndsAt: ends, BillingEnabled: true,
	}, corebilling.Record{Present: true, PlanSlug: "custom", ContractEndsAt: ends}, nil)

	got := line(t, out, "STORED", "contract ends")
	if !strings.Contains(got, "2027-01-01T00:00:00Z") {
		t.Fatalf("contract ends = %q, want the stored instant", got)
	}
	if !strings.Contains(got, "runs through 2026-12-31") {
		t.Fatalf("contract ends = %q, want the last day it covers", got)
	}
}

// With the switch off every field beneath is the disabled answer, not this org's.
func TestReportSaysWhenBillingIsDisabled(t *testing.T) {
	out := render(t, corebilling.Entitlement{Slug: "free", DisplayName: "Free", Currency: "USD"},
		corebilling.Record{Present: true, PlanSlug: "custom", IncludedEventsOverride: 5_000_000}, nil)

	if got := line(t, out, "", "billing"); !strings.Contains(got, "PUG_BILLING_ENABLED") {
		t.Fatalf("billing = %q, want it to name the switch", got)
	}
	// The stored row is still printed: it is the only thing on the page the switch
	// does not change, and it is what a grant is prepared against.
	if got := line(t, out, "STORED", "included events"); got != "5,000,000" {
		t.Fatalf("stored included events = %q, want 5,000,000", got)
	}
	// While the resolved answer is the switch's, not the row's.
	if got := line(t, out, "RESOLVED", "included events"); got != none {
		t.Fatalf("resolved included events = %q, want %q", got, none)
	}
}

func TestReportAbsentRowIsNotAnError(t *testing.T) {
	out := render(t, corebilling.Entitlement{
		Slug: "trial", DisplayName: "Trial", Currency: "USD", Status: corebilling.StatusTrialing,
		BillingEnabled: true,
	}, corebilling.Record{}, nil)

	if !strings.Contains(out, "no row") {
		t.Fatalf("want the absent row said plainly, got:\n%s", out)
	}
	if strings.Contains(out, "plan slug") {
		t.Fatalf("want no stored fields for an absent row, got:\n%s", out)
	}
}

func TestHistoryLine(t *testing.T) {
	cleared := historyLine(corebilling.Record{})
	if cleared != "cleared" {
		t.Fatalf("cleared = %q", cleared)
	}

	got := historyLine(corebilling.Record{
		Present: true, PlanSlug: "custom", IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride: 3_650,
		DisplayNameOverride:   "Acme Enterprise", AnchorDay: 17,
		ContractEndsAt:    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		ProviderProductID: "prod_2f9k", Note: "$400/mo, INV-123",
	})
	for _, want := range []string{
		"custom", "events=5,000,000", "retention=3,650d", `name="Acme Enterprise"`, "anchor-day=17",
		"until=2027-01-01T00:00:00Z", "product=prod_2f9k", `note="$400/mo, INV-123"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("history line = %q, want it to carry %s", got, want)
		}
	}

	// A renewal reads as the fields that carry a value, not as eight (none)s.
	renewal := historyLine(corebilling.Record{Present: true, PlanSlug: "growth"})
	if strings.Contains(renewal, none) {
		t.Fatalf("renewal line = %q, want no absent fields spelled out", renewal)
	}
}

func TestComma(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 999: "999", 1_000: "1,000", 10_000: "10,000",
		5_000_000: "5,000,000", 1_234_567_890: "1,234,567,890", -1_500: "-1,500",
	} {
		if got := comma(in); got != want {
			t.Fatalf("comma(%d) = %q, want %q", in, got, want)
		}
	}
}

// Minor units are not always hundredths, so a currency pug does not sell in is
// never given a decimal point it has not earned.
func TestPriceOnlyScalesTheCurrencyPugSells(t *testing.T) {
	cents := int64(2_000)
	if got := price(&cents, corebilling.Currency); got != "$20.00 USD" {
		t.Fatalf("USD price = %q", got)
	}
	if got := price(&cents, "JPY"); !strings.Contains(got, "minor units") || strings.Contains(got, "$") {
		t.Fatalf("JPY price = %q, want unscaled minor units", got)
	}
}

// The subscription line is what tells an operator whether a resolved plan is
// backed by money, so each part appears only once it has a value.
func TestSubscriptionLine(t *testing.T) {
	if got := subscription(corebilling.Entitlement{}); got != none {
		t.Errorf("no subscription = %q, want %q", got, none)
	}

	bare := subscription(corebilling.Entitlement{SubStatus: corebilling.SubStatusPastDue})
	if bare != string(corebilling.SubStatusPastDue) {
		t.Errorf("status-only line = %q", bare)
	}

	full := subscription(corebilling.Entitlement{
		SubStatus:          corebilling.SubStatusActive,
		SubPeriodEnd:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		ProviderCustomerID: "cus_1",
	})
	for _, want := range []string{"active", "bills next 2026-07-01", "customer cus_1"} {
		if !strings.Contains(full, want) {
			t.Errorf("subscription line %q is missing %q", full, want)
		}
	}
}
