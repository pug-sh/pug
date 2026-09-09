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

// none is what an absent value prints as. Never a zero: absent means no quota, no
// bound on history and no list price — "0 days of history" worst of all.
const none = "(none)"

func writeReport(out io.Writer, org dbread.Org, ent corebilling.Entitlement, rec corebilling.Record, subs []dbread.BillingSubscription, history []corebilling.HistoryEntry) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	row(w, "org", fmt.Sprintf("%s  %q", org.ID, org.DisplayName))
	row(w, "created", instant(org.CreateTime.Time))
	if ent.BillingEnabled {
		row(w, "billing", "enabled")
	} else {
		// Said in full because every field below it is the disabled answer, not this
		// org's: with the switch off every org resolves free with no quota at all.
		row(w, "billing", "DISABLED (PUG_BILLING_ENABLED) — every org resolves with no quota")
	}

	section(w, "RESOLVED", "")
	row(w, "  plan", fmt.Sprintf("%s (%s)", ent.DisplayName, ent.Slug))
	row(w, "  status", string(ent.Status))
	row(w, "  included events", quota(ent.IncludedEvents))
	row(w, "  retention", retention(ent.RetentionDays))
	row(w, "  list price", price(ent.PriceCents, ent.Currency))
	row(w, "  usage period", fmt.Sprintf("%s → %s", instant(ent.PeriodStart), instant(ent.PeriodEnd)))
	row(w, "  trial ends", instant(ent.TrialEndsAt))
	row(w, "  contract ends", contractEnd(ent.ContractEndsAt))
	row(w, "  subscription", subscription(ent))

	if !rec.Present {
		section(w, "STORED", "(no row — resolving from the org's age)")
	} else {
		section(w, "STORED", "")
		row(w, "  plan slug", rec.PlanSlug)
		row(w, "  included events", override(rec.IncludedEventsOverride))
		row(w, "  retention days", override(rec.RetentionDaysOverride))
		row(w, "  display name", text(rec.DisplayNameOverride))
		row(w, "  anchor day", override(int64(rec.AnchorDay)))
		row(w, "  contract ends", contractEnd(rec.ContractEndsAt))
		row(w, "  provider product", text(rec.ProviderProductID))
		row(w, "  trial ends", instant(rec.TrialEndsAt))
		row(w, "  note", text(rec.Note))
	}

	if len(subs) == 0 {
		section(w, "SUBSCRIPTIONS", "(none stored)")
	} else {
		section(w, "SUBSCRIPTIONS", "")
		for _, sub := range subs {
			row(w, "  "+sub.Status, fmt.Sprintf("%s  %s  %s  %s  ends %s",
				sub.PlanSlug, price(&sub.PriceCents, sub.Currency), sub.Provider,
				sub.ProviderSubID, instant(sub.CurrentPeriodEnd.Time)))
		}
	}

	// nil is "--history was not asked for"; empty is "asked for, and nothing is
	// recorded" — which has to say so rather than print no section at all.
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

func row(w io.Writer, label, value string) {
	fmt.Fprintf(w, "%s\t%s\n", label, value)
}

// section starts a block. It carries no tab, which keeps the header free of a
// tabwriter's padding and closes the preceding block so each aligns alone.
func section(w io.Writer, name, note string) {
	fmt.Fprintln(w)
	if note == "" {
		fmt.Fprintln(w, name)
		return
	}
	fmt.Fprintln(w, name+"  "+note)
}

// historyLine is one recorded snapshot on a single line, carrying only the fields
// that have a value, so a renewal reads as the two things that changed.
func historyLine(rec corebilling.Record) string {
	if !rec.Present {
		return "cleared"
	}
	parts := []string{rec.PlanSlug}
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
	if rec.ProviderProductID != "" {
		parts = append(parts, "product="+rec.ProviderProductID)
	}
	if rec.Note != "" {
		parts = append(parts, fmt.Sprintf("note=%q", rec.Note))
	}
	return strings.Join(parts, "  ")
}

func subscription(ent corebilling.Entitlement) string {
	if ent.SubStatus == "" {
		return none
	}
	out := string(ent.SubStatus)
	if !ent.SubPeriodEnd.IsZero() {
		out += "  bills next " + instant(ent.SubPeriodEnd)
	}
	if ent.ProviderCustomerID != "" {
		out += "  customer " + ent.ProviderCustomerID
	}
	return out
}

// contractEnd prints the stored instant with the last day it covers beside it. The
// resolver's comparison is half-open, so the stored value is --until's day plus one.
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

// override renders a stored override column, where 0 is the absence of one.
func override(v int64) string {
	if v == 0 {
		return none
	}
	return comma(v)
}

// retention renders a day count with its years beside it when they divide
// evenly, since "2,555 days" is read as a typo more readily than as seven years.
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

// price renders minor units. Minor units are not always hundredths (JPY has
// none), so only the currency pug sells in gets a decimal point.
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
