package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/spf13/cobra"
)

func TestFormatInfraHealth(t *testing.T) {
	if got := formatInfraHealth(nil); got != green+"connected"+reset {
		t.Fatalf("connected status = %q", got)
	}

	errMsg := formatInfraHealth(errors.New("connection refused"))
	if !strings.Contains(errMsg, red) || !strings.Contains(errMsg, "connection refused") {
		t.Fatalf("error status = %q", errMsg)
	}
}

func TestShortProbeError(t *testing.T) {
	if got := shortProbeError(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline = %q, want timeout", got)
	}

	long := strings.Repeat("x", 100)
	if got := shortProbeError(errors.New(long)); len(got) != 80 {
		t.Fatalf("truncated length = %d, want 80", len(got))
	}

	if got := shortProbeError(errors.New("first line\nsecond line")); got != "first line" {
		t.Fatalf("first line only = %q", got)
	}
}

func TestEmailDevStatus(t *testing.T) {
	t.Run("missing dashboard base URL", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_RESEND_API_KEY", "test-api-key")

		enabled, status := emailDevStatus()
		if enabled {
			t.Fatal("expected email worker to be disabled")
		}
		if want := "disabled (missing PUG_DASHBOARD_BASE_URL)"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("default resend requires API key", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "")
		t.Setenv("PUG_RESEND_API_KEY", "")

		enabled, status := emailDevStatus()
		if enabled {
			t.Fatal("expected email worker to be disabled")
		}
		if want := "disabled (missing PUG_RESEND_API_KEY for resend)"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("resend enabled when configured", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "resend")
		t.Setenv("PUG_RESEND_API_KEY", "test-api-key")

		enabled, status := emailDevStatus()
		if !enabled {
			t.Fatal("expected email worker to be enabled")
		}
		if want := "email"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("ses enabled without app-specific credentials", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "ses")
		t.Setenv("PUG_RESEND_API_KEY", "")

		enabled, status := emailDevStatus()
		if !enabled {
			t.Fatal("expected email worker to be enabled")
		}
		if want := "email"; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})

	t.Run("unsupported provider is disabled", func(t *testing.T) {
		t.Setenv("PUG_DASHBOARD_BASE_URL", "https://dashboard.example")
		t.Setenv("PUG_EMAIL_FROM", "noreply@example.com")
		t.Setenv("PUG_EMAIL_PROVIDER", "mailgun")

		enabled, status := emailDevStatus()
		if enabled {
			t.Fatal("expected email worker to be disabled")
		}
		if want := `disabled (unsupported provider "mailgun")`; status != want {
			t.Fatalf("status = %q, want %q", status, want)
		}
	})
}

func TestRenderEmailPreviewKinds(t *testing.T) {
	r := coreemail.NewRenderer(coreemail.Brand{ProductName: "Pug", DashboardURL: "https://app.example"})
	for _, kind := range []string{"magic_link", "invite", "provider_test"} {
		html, text, err := renderEmailPreview(context.Background(), r, kind, "https://app.example/x")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if html == "" || text == "" {
			t.Fatalf("%s: empty render (html=%d bytes, text=%d bytes)", kind, len(html), len(text))
		}
	}
}

func TestRenderEmailPreviewUnknownKind(t *testing.T) {
	r := coreemail.NewRenderer(coreemail.Brand{ProductName: "Pug"})
	if _, _, err := renderEmailPreview(context.Background(), r, "bogus", "https://app.example/x"); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// newSetCmd carries billingSetCmd's real flags. A fresh command per test keeps
// the Changed() state — which is what drives set-vs-clear — from leaking between
// them.
func newSetCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "set"}
	billingSetFlags(cmd)
	return cmd
}

func setFlags(t *testing.T, cmd *cobra.Command, kv map[string]string) {
	t.Helper()
	for name, v := range kv {
		if err := cmd.Flags().Set(name, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBillingChangeRequiresAPlan(t *testing.T) {
	if _, err := billingChange(newSetCmd(t)); err == nil {
		t.Error("no --plan: err = nil, want a required-flag error")
	}
}

// The expensive mistake this guards: a mistyped negative used to take the clear
// branch and silently revert a negotiated quota to the catalog number.
func TestBillingChangeRefusesANegativeEventCount(t *testing.T) {
	cmd := newSetCmd(t)
	setFlags(t, cmd, map[string]string{"plan": "growth", "events": "-5"})
	if _, err := billingChange(cmd); err == nil {
		t.Error("--events -5: err = nil, want it refused rather than treated as a clear")
	}
}

func TestBillingChangeRefusesAnAnchorDayOutOfRange(t *testing.T) {
	for _, day := range []string{"32", "-1"} {
		cmd := newSetCmd(t)
		setFlags(t, cmd, map[string]string{"plan": "growth", "anchor-day": day})
		if _, err := billingChange(cmd); err == nil {
			t.Errorf("--anchor-day %s: err = nil, want it refused", day)
		}
	}
}

func TestBillingChangeMapsFlagsToTheTriState(t *testing.T) {
	cmd := newSetCmd(t)
	setFlags(t, cmd, map[string]string{
		"plan": "custom", "events": "5000000", "name": "Acme",
		"price": "40000", "currency": "USD", "anchor-day": "17",
		"until": "2027-01-01", "note": "INV-123",
	})

	change, err := billingChange(cmd)
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	if change.PlanSlug != "custom" {
		t.Errorf("plan = %s, want custom", change.PlanSlug)
	}
	if got := change.IncludedEvents.Apply(0); got != 5_000_000 {
		t.Errorf("events = %d, want 5000000", got)
	}
	if got := change.PriceCents.Apply(-1); got != 40_000 {
		t.Errorf("price = %d, want 40000", got)
	}
	if got := change.AnchorDay.Apply(0); got != 17 {
		t.Errorf("anchor day = %d, want 17", got)
	}
	if got := change.Note.Apply(""); got != "INV-123" {
		t.Errorf("note = %q, want INV-123", got)
	}
	// Parsed and pushed to the following midnight; that the day stays covered is
	// pinned against the resolver in core.
	want := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	if got := change.ContractEndsAt.Apply(time.Time{}); !got.Equal(want) {
		t.Errorf("contract ends at %s, want %s", got, want)
	}
}

// Zero is a real price — a comped deal — so only a negative clears it, and 0
// events is the clear rather than a quota of none.
func TestBillingChangeClearsOnTheEmptyValue(t *testing.T) {
	cmd := newSetCmd(t)
	setFlags(t, cmd, map[string]string{
		"plan": "growth", "events": "0", "name": "", "price": "-1", "until": "",
	})

	change, err := billingChange(cmd)
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	if got := change.IncludedEvents.Apply(999); got != 0 {
		t.Errorf("--events 0 left %d in place, want it cleared", got)
	}
	if got := change.DisplayName.Apply("Acme"); got != "" {
		t.Errorf(`--name "" left %q in place, want it cleared`, got)
	}
	if got := change.PriceCents.Apply(999); got != 0 {
		t.Errorf("--price -1 left %d in place, want it cleared", got)
	}
	if got := change.ContractEndsAt.Apply(time.Now()); !got.IsZero() {
		t.Errorf(`--until "" left %s in place, want it cleared`, got)
	}
}

func TestBillingChangeKeepsAZeroPrice(t *testing.T) {
	cmd := newSetCmd(t)
	setFlags(t, cmd, map[string]string{"plan": "growth", "price": "0", "currency": "USD"})

	change, err := billingChange(cmd)
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	if got := change.PriceCents.Apply(999); got != 0 {
		t.Errorf("--price 0 = %d, want a stored zero (a comped deal), not the old value", got)
	}
}

// An un-passed flag must leave the stored value alone: the common re-set is a
// renewal on unchanged terms.
func TestBillingChangeLeavesUnpassedFlagsAlone(t *testing.T) {
	cmd := newSetCmd(t)
	setFlags(t, cmd, map[string]string{"plan": "growth"})

	change, err := billingChange(cmd)
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	if got := change.IncludedEvents.Apply(5_000_000); got != 5_000_000 {
		t.Errorf("an un-passed --events changed the stored quota to %d", got)
	}
	if got := change.Currency.Apply("EUR"); got != "EUR" {
		t.Errorf("an un-passed --currency changed the stored value to %q", got)
	}
}

// capture swaps os.Stdout for a pipe: these helpers print rather than return, and
// the printed shape is the operator-facing contract.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return b.String()
}

// "no quota" and "a quota of zero" are the one distinction the billing subsystem
// exists to keep, so the operator-facing rendering of absence is worth pinning.
func TestQuotaTextSpellsOutAbsence(t *testing.T) {
	if got := quotaText(nil); !strings.Contains(got, "none") {
		t.Errorf("quotaText(nil) = %q, want it to say none rather than print a number", got)
	}
	if strings.Contains(quotaText(nil), "0") {
		t.Errorf("quotaText(nil) = %q, want no digit an operator could read as a quota", quotaText(nil))
	}
	v := int64(0)
	if got := quotaText(&v); !strings.Contains(got, "0 events") {
		t.Errorf("quotaText(0) = %q, want a real zero to print as one", got)
	}
}

func TestPriceTextKeepsAZeroPriceDistinctFromNone(t *testing.T) {
	if got := priceText(nil, "USD"); !strings.Contains(got, "none") {
		t.Errorf("priceText(nil) = %q, want none", got)
	}
	v := int64(0)
	if got := priceText(&v, "USD"); !strings.Contains(got, "0 USD") {
		t.Errorf("priceText(0) = %q, want a comped deal to print its zero", got)
	}
}

// The whole point of the history table is attribution, so a change must not be
// able to reach the database without a usable name on it.
func TestBillingActorRejectsWhatWouldRecordNobody(t *testing.T) {
	cases := []struct {
		name  string
		actor string
		want  string
	}{
		{"absent", "", ""},
		{"blank", "   ", ""},
		{"too long for the column", strings.Repeat("a", billingActorMaxLen+1), ""},
		{"trimmed", "  praveen  ", "praveen"},
		{"a ticket reference", "praveen/SUP-412", "praveen/SUP-412"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			billingActorFlag(cmd)
			if err := cmd.Flags().Set("actor", tc.actor); err != nil {
				t.Fatalf("set --actor: %v", err)
			}

			got, err := billingActor(cmd)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("billingActor() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("billingActor() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("billingActor() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Cobra refuses the command outright when the flag is absent, so the check above
// is the shape guard and this is the presence one.
func TestBillingMutationsRequireAnActor(t *testing.T) {
	for _, cmd := range []*cobra.Command{billingSetCmd, billingExtendTrialCmd, billingClearCmd} {
		if cmd.Flags().Lookup("actor") == nil {
			t.Errorf("%q has no --actor flag", cmd.Name())
			continue
		}
		if cmd.Flags().Lookup("actor").Annotations[cobra.BashCompOneRequiredFlag] == nil {
			t.Errorf("%q does not require --actor", cmd.Name())
		}
	}
}

func TestOverrideAndDateHelpersRenderAbsence(t *testing.T) {
	if got := overrideText(false, 0); !strings.Contains(got, "none") {
		t.Errorf("overrideText(unset) = %q, want none", got)
	}
	if got := overrideText(true, 17); got != "17" {
		t.Errorf("overrideText(17) = %q, want 17", got)
	}
	if got := dateOrNone(time.Time{}); got != "none" {
		t.Errorf("dateOrNone(zero) = %q, want none", got)
	}
	if got := dateOrNone(time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)); got != "2027-01-01" {
		t.Errorf("dateOrNone = %q, want 2027-01-01", got)
	}
	if got := textOrNone(""); got != "none" {
		t.Errorf("textOrNone(empty) = %q, want none", got)
	}
}

// printStoredRecord is what makes an override that is not in force today visible,
// so the absent-row case has to say so rather than print a blank block.
func TestPrintStoredRecordHandlesAnAbsentRow(t *testing.T) {
	out := capture(t, func() { printStoredRecord("o_1", corebilling.Record{}) })
	if !strings.Contains(out, "none") {
		t.Errorf("printStoredRecord(absent) = %q, want it to say no row is stored", out)
	}
}

func TestPrintStoredRecordShowsTheOverridesAndNote(t *testing.T) {
	price := int64(40_000)
	out := capture(t, func() {
		printStoredRecord("o_1", corebilling.Record{
			Present:                true,
			PlanSlug:               "custom",
			IncludedEventsOverride: 5_000_000,
			DisplayNameOverride:    "Acme Enterprise",
			PriceCentsOverride:     &price,
			CurrencyOverride:       "USD",
			AnchorDay:              17,
			Note:                   "annual wire, INV-123",
		})
	})
	for _, want := range []string{"custom", "5000000", "Acme Enterprise", "40000 USD", "17", "INV-123"} {
		if !strings.Contains(out, want) {
			t.Errorf("printStoredRecord output is missing %q:\n%s", want, out)
		}
	}
}
