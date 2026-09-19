package billing

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"

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

func render(t *testing.T, ent entitlement.Entitlement, rec entitlement.Record, history []entitlement.HistoryEntry) string {
	t.Helper()
	return renderWithSubs(t, ent, rec, nil, history)
}

func renderWithSubs(t *testing.T, ent entitlement.Entitlement, rec entitlement.Record,
	subs []dbread.BillingSubscription, history []entitlement.HistoryEntry,
) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeReport(&buf, testOrg(), ent, rec, subs, history); err != nil {
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

// Absent means NO allowance and NO retention bound. Rendering either as 0 states
// a billing figure the deployment never claimed. There is no list price line any
// more: a graduated card is a table, and an org with neither card nor terms is
// one nothing prices.
func TestReportNeverRendersAbsentAsZero(t *testing.T) {
	out := render(t, entitlement.Entitlement{
		Slug: "custom", DisplayName: "Custom", Currency: "USD", Status: entitlement.StatusActive,
		BillingEnabled: true,
	}, entitlement.Record{}, nil)

	if got := line(t, out, "RESOLVED", "included events"); got != none {
		t.Fatalf("included events = %q, want %q", got, none)
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
		out := render(t, entitlement.Entitlement{
			Slug: "scale", DisplayName: "Scale", Currency: "USD", Status: entitlement.StatusActive,
			RetentionDays: &days, BillingEnabled: true,
		}, entitlement.Record{}, nil)
		if got := line(t, out, "RESOLVED", "retention"); got != want {
			t.Errorf("retention for %d days = %q, want %q", days, got, want)
		}
	}
}

// A zero fee on a deal is a real term -- a rate-only arrangement charges from the
// first event -- and must not read as absence.
func TestReportRendersAZeroFee(t *testing.T) {
	terms := entitlement.CustomTerms{FlatFeeCents: 0, RateCentsPerMillion: 3_000}
	free := int64(10_000)
	out := render(t, entitlement.Entitlement{
		Slug: "custom", DisplayName: "Custom", Currency: "USD", Status: entitlement.StatusActive,
		IncludedEvents: &free, BillingEnabled: true, Terms: &terms,
	}, entitlement.Record{}, nil)

	if got := line(t, out, "RESOLVED", "flat fee"); got != "$0.00 USD" {
		t.Fatalf("flat fee = %q, want $0.00 USD", got)
	}
	if got := line(t, out, "RESOLVED", "included events"); got != "10,000" {
		t.Fatalf("included events = %q, want 10,000", got)
	}
}

// The stored instant is the day after the one an operator typed. Printing the
// pair is what stops that reading as an off-by-one.
func TestReportContractEndNamesTheLastDayCovered(t *testing.T) {
	ends := entitlement.ContractEndExclusive(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	out := render(t, entitlement.Entitlement{
		Slug: "custom", DisplayName: "Custom", Currency: "USD", ContractEndsAt: ends, BillingEnabled: true,
	}, entitlement.Record{Present: true, PlanSlug: "custom", ContractEndsAt: ends}, nil)

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
	out := render(t, entitlement.Entitlement{Slug: "free", DisplayName: "Free", Currency: "USD"},
		entitlement.Record{Present: true, PlanSlug: "custom", IncludedEventsOverride: 5_000_000}, nil)

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
	out := render(t, entitlement.Entitlement{
		Slug: "trial", DisplayName: "Trial", Currency: "USD", Status: entitlement.StatusTrialing,
		BillingEnabled: true,
	}, entitlement.Record{}, nil)

	if !strings.Contains(out, "no row") {
		t.Fatalf("want the absent row said plainly, got:\n%s", out)
	}
	if strings.Contains(out, "plan slug") {
		t.Fatalf("want no stored fields for an absent row, got:\n%s", out)
	}
}

func TestHistoryLine(t *testing.T) {
	cleared := historyLine(entitlement.Record{})
	if cleared != "cleared" {
		t.Fatalf("cleared = %q", cleared)
	}

	got := historyLine(entitlement.Record{
		Present: true, PlanSlug: "custom", IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride: 3_650,
		DisplayNameOverride:   "Acme Enterprise", AnchorDay: 17,
		ContractEndsAt:    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		TrialEndsAt:       time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ProviderProductID: "prod_2f9k", Note: "$400/mo, INV-123",
	})
	for _, want := range []string{
		"custom", "events=5,000,000", "retention=3,650d", `name="Acme Enterprise"`, "anchor-day=17",
		"until=2027-01-01T00:00:00Z", "trial-ends=2026-10-01T00:00:00Z",
		"product=prod_2f9k", `note="$400/mo, INV-123"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("history line = %q, want it to carry %s", got, want)
		}
	}

	// A renewal reads as the fields that carry a value, not as eight (none)s.
	renewal := historyLine(entitlement.Record{Present: true, PlanSlug: "growth"})
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
	if got := subscription(entitlement.Entitlement{}); got != none {
		t.Errorf("no subscription = %q, want %q", got, none)
	}

	bare := subscription(entitlement.Entitlement{SubStatus: corebilling.SubStatusPastDue})
	if bare != string(corebilling.SubStatusPastDue) {
		t.Errorf("status-only line = %q", bare)
	}

	full := subscription(entitlement.Entitlement{
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

// The resolved entitlement nils a non-live subscription and resolves nothing while
// billing is off. The section reads the rows directly, so both stay visible.
func TestReportShowsStoredSubscriptionsTheResolvedAnswerHides(t *testing.T) {
	ent := entitlement.Entitlement{Slug: "free", DisplayName: "Free", Status: entitlement.StatusFree}
	subs := []dbread.BillingSubscription{{
		Currency:      "USD",
		OrgID:         "o_2f9k",
		PlanSlug:      "growth",
		PriceCents:    2000,
		Provider:      "dodo",
		ProviderSubID: "sub_1",
		Status:        "cancelled",
	}}

	out := renderWithSubs(t, ent, entitlement.Record{}, subs, nil)
	if !strings.Contains(out, "sub_1") || !strings.Contains(out, "cancelled") {
		t.Errorf("SUBSCRIPTIONS section did not name the stored row:\n%s", out)
	}

	if empty := renderWithSubs(t, ent, entitlement.Record{}, nil, nil); !strings.Contains(empty, "(none stored)") {
		t.Errorf("an org with no rows should say so:\n%s", empty)
	}
}

// An org whose entitlement has never been touched must still report an empty
// history when one is asked for: a clean history and no history are different
// answers, and only the nil says the operator did not ask.
func TestHistorySectionSeparatesUnaskedFromEmpty(t *testing.T) {
	ent := entitlement.Entitlement{Slug: entitlement.SlugFree, DisplayName: "Free"}

	if out := render(t, ent, entitlement.Record{}, []entitlement.HistoryEntry{}); !strings.Contains(out, "(no recorded changes)") {
		t.Errorf("an empty history did not report itself:\n%s", out)
	}
	if out := render(t, ent, entitlement.Record{}, nil); strings.Contains(out, "HISTORY") {
		t.Errorf("a history nobody asked for was printed anyway:\n%s", out)
	}
}

// A card is printed as the table it is. Printing only its allowance would hide
// what the org is actually charged past it.
func TestReportRendersTheRateCardsTiers(t *testing.T) {
	c := entitlement.CurrentCard()
	out := render(t, entitlement.Entitlement{
		Slug: c.Slug, DisplayName: c.DisplayName, Currency: c.Currency,
		Status: entitlement.StatusFree, BillingEnabled: true, Card: &c,
	}, entitlement.Record{}, nil)

	got := line(t, out, "RESOLVED", "rate card")
	if !strings.Contains(got, "free") {
		t.Errorf("rate card = %q, want the free allowance named", got)
	}
	if !strings.Contains(got, "/M") {
		t.Errorf("rate card = %q, want a per-million rate", got)
	}
}

// A deal's money prints in BOTH halves: once it lapses the resolved answer hides
// it, and it still carries onto the next set.
func TestReportPrintsDealMoneyInBothHalves(t *testing.T) {
	terms := entitlement.CustomTerms{FlatFeeCents: 40_000, RateCentsPerMillion: 3_000}
	out := render(t, entitlement.Entitlement{
		Slug: "custom", DisplayName: "Custom", Currency: "USD",
		Status: entitlement.StatusActive, BillingEnabled: true, Terms: &terms,
	}, entitlement.Record{
		Present: true, PlanSlug: "custom",
		FlatFeeCents: 40_000, RateCentsPerMillion: 3_000,
	}, nil)

	for _, section := range []string{"RESOLVED", "STORED"} {
		if got := line(t, out, section, "flat fee"); got == none || got == "" {
			t.Errorf("%s flat fee = %q, want the deal's fee", section, got)
		}
		if got := line(t, out, section, "rate per million"); got == none || got == "" {
			t.Errorf("%s rate per million = %q, want the deal's rate", section, got)
		}
	}
}
