package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appbilling "github.com/pug-sh/pug/internal/app/billing"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/spf13/cobra"
)

// The tree is built by a function so a test can take a fresh one: cobra keeps
// parsed flag state on the command it is bound to.
var billingCmd = newBillingCmd()

func newBillingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "billing",
		Short: "Grant, extend and inspect org billing entitlements",
		Long: "Operator commands for the entitlement store — what an org is allowed to\n" +
			"send. Postgres only: no payments provider is contacted, and no price is\n" +
			"ever written here. Every write is attributed to --actor and appended to\n" +
			"the org's history in the same transaction.",
	}

	for _, c := range []*cobra.Command{
		newBillingShowCmd(),
		newBillingSetCmd(),
		newBillingExtendTrialCmd(),
		newBillingClearCmd(),
		newBillingPreviewCmd(),
		newBillingInvoiceCmd(),
	} {
		// Cobra reads this off the executed command, not its parent: without it a
		// failed query prints the usage block under the error.
		c.SilenceUsage = true
		cmd.AddCommand(c)
	}
	return cmd
}

func newBillingShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <org-id>",
		Short: "Print the resolved entitlement and the stored row beneath it",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, orgID string) error {
			history, _ := cmd.Flags().GetBool("history")
			invoices, _ := cmd.Flags().GetBool("invoices")
			return cli.Show(ctx, cmd.OutOrStdout(), orgID, appbilling.ShowOptions{History: history, Invoices: invoices})
		}),
	}
	cmd.Flags().Bool("history", false, "also print the org's recorded changes, newest first")
	cmd.Flags().Bool("invoices", false, "also print the org's invoices, newest first")
	return cmd
}

func newBillingSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <org-id>",
		Short: "Pin a rate card or record a deal, merging the flags given over whatever is stored",
		Long: "Pins a rate card or records a deal. Omitting a flag leaves the stored value\n" +
			"alone — the common re-set is a renewal on terms that have not changed — and\n" +
			"passing its empty value (--flat-fee 0, --block-rate 0, --events 0,\n" +
			"--retention-days 0, --name \"\", --anchor-day 0, --until \"\") clears it.\n" +
			"A custom plan needs a flat fee or a block rate; --events is its allowance.\n\n" +
			"--until is INCLUSIVE of the date given: --until 2026-12-31 runs the plan\n" +
			"through all of 31 December, and `show` prints the stored instant, which is\n" +
			"therefore the 1st.",
		Args: cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, orgID string) error {
			change, err := billingChange(cmd)
			if err != nil {
				return err
			}
			actor, _ := cmd.Flags().GetString("actor")
			err = cli.Set(ctx, cmd.OutOrStdout(), orgID, actor, change)
			// The service owns which slugs exist; naming them is a help message.
			if errors.Is(err, corebilling.ErrPlanNotFound) {
				return fmt.Errorf("%w %q (want %s)", err, change.PlanSlug, strings.Join(grantableSlugs(), ", "))
			}
			return err
		}),
	}
	cmd.Flags().String("plan", "", "rate card slug, custom, or free")
	cmd.Flags().Int64("flat-fee", 0, "USD cents charged every period regardless of usage; 0 clears it")
	cmd.Flags().Int64("block-rate", 0, "USD cents per 100,000 events over the allowance; 0 clears it")
	cmd.Flags().Int64("events", 0, "events per period before charges begin, in whole 100,000 blocks; 0 clears the override")
	cmd.Flags().Int64("retention-days", 0, "negotiated days of event history kept; 0 clears the override")
	cmd.Flags().String("name", "", "display name shown to the org; empty clears the override")
	cmd.Flags().Int("anchor-day", 0, "day of month the usage period turns over (1-31); 0 clears the override")
	cmd.Flags().String("until", "", "last day the deal runs, YYYY-MM-DD and inclusive; empty clears it")
	cmd.Flags().String("note", "", "free text an operator settles a renewal argument from; empty clears it")
	mustMarkRequired(cmd, "plan")
	requireActor(cmd)
	return cmd
}

func newBillingExtendTrialCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extend-trial <org-id>",
		Short: "Move the org's trial end to --days from now",
		Long: "Sets an absolute now + --days, so it can only ever lengthen a trial: a\n" +
			"--days that would land before the current end is refused rather than\n" +
			"silently cutting it short.",
		Args: cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, orgID string) error {
			days, _ := cmd.Flags().GetInt("days")
			actor, _ := cmd.Flags().GetString("actor")
			return cli.ExtendTrial(ctx, cmd.OutOrStdout(), orgID, actor, days)
		}),
	}
	cmd.Flags().Int("days", 0, fmt.Sprintf("days from now the trial should end (1-%d)", corebilling.MaxTrialDays))
	mustMarkRequired(cmd, "days")
	requireActor(cmd)
	return cmd
}

func newBillingPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview <org-id>",
		Short: "Price a number of events on what the org would be charged today",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, orgID string) error {
			events, _ := cmd.Flags().GetInt64("events")
			if events < 0 {
				return errors.New("--events cannot be negative")
			}
			return cli.Preview(ctx, cmd.OutOrStdout(), orgID, events)
		}),
	}
	cmd.Flags().Int64("events", 0, "events in the period to price")
	mustMarkRequired(cmd, "events")
	return cmd
}

func newBillingInvoiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invoice",
		Short: "Void or retry one invoice",
	}
	void := &cobra.Command{
		Use:   "void <invoice-id>",
		Short: "Stop a charge before it happens, or record a refund made in the provider's dashboard",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, id string) error {
			actor, _ := cmd.Flags().GetString("actor")
			note, _ := cmd.Flags().GetString("note")
			return cli.VoidInvoice(ctx, cmd.OutOrStdout(), id, actor, note)
		}),
	}
	void.Flags().String("note", "", "why, recorded on the invoice")
	mustMarkRequired(void, "note")
	requireActor(void)
	retry := &cobra.Command{
		Use:   "retry <invoice-id>",
		Short: "Put a failed or uncollectible invoice back in line to be charged now",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, id string) error {
			actor, _ := cmd.Flags().GetString("actor")
			return cli.RetryInvoice(ctx, cmd.OutOrStdout(), id, actor)
		}),
	}
	requireActor(retry)
	for _, c := range []*cobra.Command{void, retry} {
		c.SilenceUsage = true
		cmd.AddCommand(c)
	}
	return cmd
}

func newBillingClearCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear <org-id>",
		Short: "Delete the stored row, returning the org to the derived floors",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cli *appbilling.CLI, cmd *cobra.Command, orgID string) error {
			actor, _ := cmd.Flags().GetString("actor")
			return cli.Clear(ctx, cmd.OutOrStdout(), orgID, actor)
		}),
	}
	requireActor(cmd)
	return cmd
}

// A flag left off is nil, which keeps the stored value; a flag given its empty
// value is a clear.
func billingChange(cmd *cobra.Command) (corebilling.Change, error) {
	flags := cmd.Flags()
	planSlug, _ := flags.GetString("plan")
	change := corebilling.Change{
		PlanSlug:       planSlug,
		IncludedEvents: flagIfSet(cmd, "events", flags.GetInt64),
		RetentionDays:  flagIfSet(cmd, "retention-days", flags.GetInt64),
		DisplayName:    flagIfSet(cmd, "name", flags.GetString),
		AnchorDay:      flagIfSet(cmd, "anchor-day", flags.GetInt),
		Note:           flagIfSet(cmd, "note", flags.GetString),
		FlatFeeCents:   flagIfSet(cmd, "flat-fee", flags.GetInt64),
		BlockRateCents: flagIfSet(cmd, "block-rate", flags.GetInt64),
	}
	if err := checkOverrides(change); err != nil {
		return corebilling.Change{}, err
	}
	endsAt, err := contractEnd(cmd)
	if err != nil {
		return corebilling.Change{}, err
	}
	change.ContractEndsAt = endsAt
	return change, nil
}

func checkOverrides(change corebilling.Change) error {
	switch {
	case change.IncludedEvents != nil && *change.IncludedEvents < 0:
		return errors.New("--events cannot be negative; pass 0 to clear the override")
	case change.IncludedEvents != nil && *change.IncludedEvents%corebilling.BlockEvents != 0:
		return errors.New("--events must be a whole number of 100,000-event blocks")
	case change.FlatFeeCents != nil && *change.FlatFeeCents < 0:
		return errors.New("--flat-fee cannot be negative; pass 0 to clear it")
	case change.BlockRateCents != nil && *change.BlockRateCents < 0:
		return errors.New("--block-rate cannot be negative; pass 0 to clear it")
	case change.RetentionDays != nil && *change.RetentionDays < 0:
		return errors.New("--retention-days cannot be negative; pass 0 to clear the override")
	case change.AnchorDay != nil && (*change.AnchorDay < 0 || *change.AnchorDay > 31):
		return errors.New("--anchor-day must be between 1 and 31, or 0 to clear it")
	}
	return nil
}

// --until names the last day the deal runs; the resolver compares half-open, so
// the stored instant is the following midnight.
func contractEnd(cmd *cobra.Command) (*time.Time, error) {
	raw := flagIfSet(cmd, "until", cmd.Flags().GetString)
	if raw == nil {
		return nil, nil
	}
	var endsAt time.Time
	if *raw != "" {
		day, err := time.Parse(time.DateOnly, *raw)
		if err != nil {
			return nil, fmt.Errorf("--until must be a YYYY-MM-DD date, got %q", *raw)
		}
		endsAt = corebilling.ContractEndExclusive(day)
	}
	return &endsAt, nil
}

// The dropped error is an unregistered name: a wiring typo, not operator input.
func flagIfSet[T any](cmd *cobra.Command, name string, get func(string) (T, error)) *T {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	v, _ := get(name)
	return &v
}

// What --plan accepts for a NEW grant. Trial is extend-trial's alone; a retired
// card is kept for its holders and stays settable for an org already on it.
func grantableSlugs() []string {
	out := []string{corebilling.SlugFree}
	for _, c := range corebilling.Cards() {
		if !c.Retired {
			out = append(out, c.Slug)
		}
	}
	return append(out, corebilling.SlugCustom)
}

// Logs move to stderr so a command's report is the only thing on stdout. args[0]
// is the org or invoice id: every billing command declares ExactArgs(1).
func billingRunE(fn func(context.Context, *appbilling.CLI, *cobra.Command, string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		dotenv.LoadOrExit(ctx)

		cli, err := appbilling.New(ctx)
		if err != nil {
			return err
		}
		defer cli.Close()

		return fn(ctx, cli, cmd, args[0])
	}
}

func requireActor(cmd *cobra.Command) {
	cmd.Flags().String("actor", "", "who is making this change, recorded in the history (e.g. \"praveen/INV-123\")")
	mustMarkRequired(cmd, "actor")
}

// A flag name that does not exist is a wiring typo, not operator input.
func mustMarkRequired(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("pug billing: %s has no %s flag: %v", cmd.Name(), name, err))
		}
	}
}
