package billing

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

// none is what an absent value prints as. Never a zero: absent means no quota and
// no bound on history.
const none = "(none)"

func writeReport(out io.Writer, org dbread.Org, ent corebilling.Entitlement, rec corebilling.Record,
	subs []dbread.BillingSubscription, history []corebilling.HistoryEntry, invoices []corebilling.Invoice,
) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	row(w, "org", fmt.Sprintf("%s  %q", org.ID, org.DisplayName))
	row(w, "created", instant(org.CreateTime.Time))
	if ent.BillingEnabled {
		row(w, "billing", "enabled")
	} else {
		row(w, "billing", "DISABLED (PUG_BILLING_ENABLED) — every org resolves with no quota")
	}

	section(w, "RESOLVED", "")
	row(w, "  plan", fmt.Sprintf("%s (%s)", ent.DisplayName, ent.Slug))
	row(w, "  status", string(ent.Status))
	row(w, "  pricing", pricing(ent))
	row(w, "  included events", quota(ent.IncludedEvents))
	row(w, "  retention", retention(ent.RetentionDays))
	row(w, "  usage period", fmt.Sprintf("%s → %s", instant(ent.PeriodStart), instant(ent.PeriodEnd)))
	row(w, "  next charge", chargeLine(ent))
	row(w, "  trial ends", instant(ent.TrialEndsAt))
	row(w, "  contract ends", contractEnd(ent.ContractEndsAt))
	row(w, "  subscription", subscription(ent))

	if !rec.Present {
		section(w, "STORED", "(no row — resolving from the org's age)")
	} else {
		section(w, "STORED", "")
		row(w, "  plan slug", rec.PlanSlug)
		row(w, "  flat fee", money(rec.FlatFeeCents))
		row(w, "  block rate", money(rec.BlockRateCents))
		row(w, "  included events", override(rec.IncludedEventsOverride))
		row(w, "  retention days", override(rec.RetentionDaysOverride))
		row(w, "  display name", text(rec.DisplayNameOverride))
		row(w, "  anchor day", override(int64(rec.AnchorDay)))
		row(w, "  contract ends", contractEnd(rec.ContractEndsAt))
		row(w, "  trial ends", instant(rec.TrialEndsAt))
		row(w, "  note", text(rec.Note))
	}

	if len(subs) == 0 {
		section(w, "SUBSCRIPTIONS", "(none stored)")
	} else {
		section(w, "SUBSCRIPTIONS", "")
		for _, sub := range subs {
			row(w, "  "+sub.Status, fmt.Sprintf("%s  %s  %s  next %s%s",
				sub.PlanSlug, sub.Provider, sub.ProviderSubID,
				instant(sub.CurrentPeriodEnd.Time), cancelFlag(sub)))
		}
	}

	// nil is "not asked for"; empty is "asked for, and nothing is recorded".
	if invoices != nil {
		if len(invoices) == 0 {
			section(w, "INVOICES", "(none)")
		} else {
			section(w, "INVOICES", "(newest first)")
			for _, inv := range invoices {
				fmt.Fprintf(w, "  %s\t%s\n", inv.ID, invoiceLine(inv))
			}
		}
	}

	if history != nil {
		if len(history) == 0 {
			section(w, "HISTORY", "(no recorded changes)")
		} else {
			section(w, "HISTORY", "(newest first)")
			for _, h := range history {
				fmt.Fprintf(w, "  %s\t%s\t%s\n", instant(h.ChangedAt), h.Actor, historyLine(h.Record))
			}
		}
	}

	return w.Flush()
}

func writePreview(out io.Writer, ent corebilling.Entitlement, events int64, q corebilling.Quote) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	row(w, "plan", fmt.Sprintf("%s (%s)", ent.DisplayName, ent.Slug))
	row(w, "pricing", pricing(ent))
	row(w, "events", comma(events))
	row(w, "blocks", comma(q.Blocks))
	for _, l := range q.Lines {
		row(w, "  "+l.Description, lineAmount(l, ent.Currency))
	}
	row(w, "total", price(&q.TotalCents, ent.Currency))
	return w.Flush()
}

func writeInvoices(out io.Writer, invoices []corebilling.Invoice) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, inv := range invoices {
		fmt.Fprintf(w, "%s\t%s\n", inv.ID, invoiceLine(inv))
	}
	return w.Flush()
}

func invoiceLine(inv corebilling.Invoice) string {
	parts := []string{
		string(inv.Status),
		fmt.Sprintf("%s → %s", inv.PeriodStart.UTC().Format(time.DateOnly), inv.PeriodEnd.UTC().Format(time.DateOnly)),
		"events=" + comma(inv.EventCount),
		price(&inv.AmountCents, inv.Currency),
	}
	if inv.Attempts > 0 {
		parts = append(parts, "attempts="+strconv.Itoa(inv.Attempts))
	}
	if !inv.NextAttemptAt.IsZero() && (inv.Status == corebilling.InvoiceOpen || inv.Status == corebilling.InvoiceFailed) {
		parts = append(parts, "next="+instant(inv.NextAttemptAt))
	}
	if inv.LastErrorCode != "" {
		parts = append(parts, "error="+inv.LastErrorCode)
	}
	if inv.ProviderPaymentID != "" {
		parts = append(parts, "payment="+inv.ProviderPaymentID)
	}
	return strings.Join(parts, "  ")
}

func row(w io.Writer, label, value string) {
	fmt.Fprintf(w, "%s\t%s\n", label, value)
}

func section(w io.Writer, name, note string) {
	fmt.Fprintln(w)
	if note == "" {
		fmt.Fprintln(w, name)
		return
	}
	fmt.Fprintln(w, name+"  "+note)
}

func historyLine(rec corebilling.Record) string {
	if !rec.Present {
		return "cleared"
	}
	parts := []string{rec.PlanSlug}
	if rec.FlatFeeCents > 0 {
		parts = append(parts, "flat-fee="+money(rec.FlatFeeCents))
	}
	if rec.BlockRateCents > 0 {
		parts = append(parts, "block-rate="+money(rec.BlockRateCents))
	}
	if rec.IncludedEventsOverride > 0 {
		parts = append(parts, "events="+comma(rec.IncludedEventsOverride))
	}
	if rec.RetentionDaysOverride > 0 {
		parts = append(parts, "retention="+comma(rec.RetentionDaysOverride)+"d")
	}
	if rec.DisplayNameOverride != "" {
		parts = append(parts, fmt.Sprintf("name=%q", rec.DisplayNameOverride))
	}
	if rec.AnchorDay > 0 {
		parts = append(parts, "anchor-day="+strconv.Itoa(rec.AnchorDay))
	}
	if !rec.ContractEndsAt.IsZero() {
		parts = append(parts, "until="+instant(rec.ContractEndsAt))
	}
	if !rec.TrialEndsAt.IsZero() {
		parts = append(parts, "trial-ends="+instant(rec.TrialEndsAt))
	}
	if rec.Note != "" {
		parts = append(parts, fmt.Sprintf("note=%q", rec.Note))
	}
	return strings.Join(parts, "  ")
}

// pricing renders the card's tiers or the deal's terms: what the customer is
// charged per block.
func pricing(ent corebilling.Entitlement) string {
	switch {
	case ent.Terms != nil:
		var parts []string
		if ent.Terms.FlatFeeCents > 0 {
			parts = append(parts, money(ent.Terms.FlatFeeCents)+" flat")
		}
		if ent.Terms.BlockRateCents > 0 {
			parts = append(parts, money(ent.Terms.BlockRateCents)+" per block over "+comma(ent.Terms.IncludedEvents))
		}
		return strings.Join(parts, ", ")
	case ent.Card != nil:
		parts := []string{fmt.Sprintf("%d free", ent.Card.FreeBlocks)}
		for _, t := range ent.Card.Tiers {
			if t.UpToBlock == 0 {
				parts = append(parts, money(t.CentsPerBlock)+" beyond")
			} else {
				parts = append(parts, fmt.Sprintf("%s to block %d", money(t.CentsPerBlock), t.UpToBlock))
			}
		}
		return strings.Join(parts, ", ") + fmt.Sprintf("  (blocks of %s events)", comma(ent.Card.BlockEvents))
	}
	return none
}

func chargeLine(ent corebilling.Entitlement) string {
	if ent.NextChargeAt.IsZero() {
		return none
	}
	if !ent.Chargeable {
		return instant(ent.NextChargeAt) + "  (no payment method)"
	}
	return instant(ent.NextChargeAt)
}

func cancelFlag(sub dbread.BillingSubscription) string {
	if sub.CancelAtPeriodEnd {
		return "  (cancels)"
	}
	return ""
}

func lineAmount(l corebilling.Line, currency string) string {
	if l.CentsPerBlock == 0 {
		if l.Blocks > 0 {
			return fmt.Sprintf("%s blocks", comma(l.Blocks))
		}
		return price(&l.AmountCents, currency)
	}
	return fmt.Sprintf("%s blocks × %s = %s", comma(l.Blocks), money(l.CentsPerBlock), price(&l.AmountCents, currency))
}

func subscription(ent corebilling.Entitlement) string {
	if ent.SubStatus == "" {
		return none
	}
	out := string(ent.SubStatus)
	if !ent.SubPeriodEnd.IsZero() {
		out += "  next " + instant(ent.SubPeriodEnd)
	}
	if ent.ProviderCustomerID != "" {
		out += "  customer " + ent.ProviderCustomerID
	}
	return out
}

func contractEnd(t time.Time) string {
	if t.IsZero() {
		return none
	}
	return fmt.Sprintf("%s  (runs through %s)", instant(t), t.UTC().AddDate(0, 0, -1).Format(time.DateOnly))
}

func instant(t time.Time) string {
	if t.IsZero() {
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

func text(v string) string {
	if v == "" {
		return none
	}
	return v
}

func override(v int64) string {
	if v == 0 {
		return none
	}
	return comma(v)
}

// money renders a stored cents column, where 0 is the absence of a value.
func money(cents int64) string {
	if cents == 0 {
		return none
	}
	return price(&cents, corebilling.Currency)
}

func retention(v *int64) string {
	if v == nil {
		return none
	}
	out := comma(*v) + " days"
	if years := *v / corebilling.RetentionYearDays; years > 0 && *v%corebilling.RetentionYearDays == 0 {
		if years == 1 {
			return out + "  (1 year)"
		}
		return fmt.Sprintf("%s  (%d years)", out, years)
	}
	return out
}

func quota(v *int64) string {
	if v == nil {
		return none
	}
	return comma(*v)
}

// price renders minor units. Only the currency pug sells in gets a decimal point.
func price(cents *int64, currency string) string {
	if cents == nil {
		return none
	}
	if currency != corebilling.Currency {
		return fmt.Sprintf("%s %s (minor units)", comma(*cents), currency)
	}
	return fmt.Sprintf("$%s.%02d %s", comma(*cents/100), *cents%100, currency)
}

func comma(v int64) string {
	s := strconv.FormatInt(v, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return sign + b.String()
}
