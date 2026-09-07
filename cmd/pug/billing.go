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

	"github.com/joho/godotenv"
	appbilling "github.com/pug-sh/pug/internal/app/billing"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/spf13/cobra"
)

func newBillingCmd() *cobra.Command {
	billingCmd := &cobra.Command{
		Use:   "billing",
		Short: "Grant, extend and inspect org billing entitlements",
		Long: "Operator commands for the entitlement store — what an org is allowed to\n" +
			"send. Postgres only: no payments provider is contacted, and no price is\n" +
			"ever written here. Every write is attributed to --actor and appended to\n" +
			"the org's history in the same transaction.",
	}

	showCmd := &cobra.Command{
		Use:   "show <org-id>",
		Short: "Print the resolved entitlement and the stored row beneath it",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			history, _ := cmd.Flags().GetBool("history")
			return appbilling.Show(ctx, cmd.OutOrStdout(), args[0], history)
		}),
	}
	showCmd.Flags().Bool("history", false, "also print the org's recorded changes, newest first")

	setCmd := &cobra.Command{
		Use:   "set <org-id>",
		Short: "Grant a plan, merging the flags given over whatever is stored",
		Long: "Grants a plan. Omitting an override flag leaves the stored value alone —\n" +
			"the common re-set is a renewal on terms that have not changed — and\n" +
			"passing its empty value (--events 0, --retention-days 0, --name \"\",\n" +
			"--anchor-day 0, --until \"\") clears it back to the plan's.\n\n" +
			"--until is INCLUSIVE of the date given: --until 2026-12-31 runs the plan\n" +
			"through all of 31 December, and `show` prints the stored instant, which is\n" +
			"therefore the 1st.",
		Args: cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			change, err := billingChange(cmd)
			if err != nil {
				return err
			}
			actor, _ := cmd.Flags().GetString("actor")
			err = appbilling.Set(ctx, cmd.OutOrStdout(), args[0], actor, change)
			// The service is the one authority on which slugs exist; naming them is a
			// help message, which is this layer's job.
			if errors.Is(err, corebilling.ErrPlanNotFound) {
				return fmt.Errorf("%w %q (want %s)", err, change.PlanSlug, strings.Join(grantableSlugs(), ", "))
			}
			return err
		}),
	}
	setCmd.Flags().String("plan", "", "catalog slug to grant")
	setCmd.Flags().String("actor", "", "who is making this change, recorded in the history (e.g. \"praveen/INV-123\")")
	setCmd.Flags().Int64("events", 0, "negotiated monthly event quota; 0 clears the override")
	setCmd.Flags().Int64("retention-days", 0, "negotiated days of event history kept; 0 clears the override")
	setCmd.Flags().String("name", "", "display name shown to the org; empty clears the override")
	setCmd.Flags().Int("anchor-day", 0, "day of month the usage period turns over (1-31); 0 clears the override")
	setCmd.Flags().String("until", "", "last day the deal runs, YYYY-MM-DD and inclusive; empty clears it")
	setCmd.Flags().String("note", "", "free text an operator settles a renewal argument from; empty clears it")
	setCmd.Flags().String("provider-product", "", "provider product a negotiated deal is bought against; empty clears it")
	mustMarkRequired(setCmd, "plan", "actor")

	extendTrialCmd := &cobra.Command{
		Use:   "extend-trial <org-id>",
		Short: "Move the org's trial end to --days from now",
		Long: "Sets an absolute now + --days, so it can only ever lengthen a trial: a\n" +
			"--days that would land before the current end is refused rather than\n" +
			"silently cutting it short.",
		Args: cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			days, _ := cmd.Flags().GetInt("days")
			actor, _ := cmd.Flags().GetString("actor")
			return appbilling.ExtendTrial(ctx, cmd.OutOrStdout(), args[0], actor, days)
		}),
	}
	extendTrialCmd.Flags().Int("days", 0, fmt.Sprintf("days from now the trial should end (1-%d)", corebilling.MaxTrialDays))
	extendTrialCmd.Flags().String("actor", "", "who is making this change, recorded in the history")
	mustMarkRequired(extendTrialCmd, "days", "actor")

	clearCmd := &cobra.Command{
		Use:   "clear <org-id>",
		Short: "Delete the stored row, returning the org to the derived floors",
		Args:  cobra.ExactArgs(1),
		RunE: billingRunE(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			actor, _ := cmd.Flags().GetString("actor")
			return appbilling.Clear(ctx, cmd.OutOrStdout(), args[0], actor)
		}),
	}
	clearCmd.Flags().String("actor", "", "who is making this change, recorded in the history")
	mustMarkRequired(clearCmd, "actor")

	for _, c := range []*cobra.Command{showCmd, setCmd, extendTrialCmd, clearCmd} {
		// Cobra reads this off the executed command, not its parent: without it a
		// failed database call prints the whole usage block under the error.
		c.SilenceUsage = true
		billingCmd.AddCommand(c)
	}
	return billingCmd
}

// billingChange maps the set flags onto one edit. A flag left off is nil, which
// keeps the stored value; a flag given its empty value is a clear.
func billingChange(cmd *cobra.Command) (corebilling.Change, error) {
	flags := cmd.Flags()
	change := corebilling.Change{}
	change.PlanSlug, _ = flags.GetString("plan")

	if flags.Changed("events") {
		v, _ := flags.GetInt64("events")
		if v < 0 {
			return corebilling.Change{}, errors.New("--events cannot be negative; pass 0 to clear the override")
		}
		change.IncludedEvents = &v
	}
	if flags.Changed("retention-days") {
		v, _ := flags.GetInt64("retention-days")
		if v < 0 {
			return corebilling.Change{}, errors.New("--retention-days cannot be negative; pass 0 to clear the override")
		}
		change.RetentionDays = &v
	}
	if flags.Changed("name") {
		v, _ := flags.GetString("name")
		change.DisplayName = &v
	}
	if flags.Changed("anchor-day") {
		v, _ := flags.GetInt("anchor-day")
		if v < 0 || v > 31 {
			return corebilling.Change{}, errors.New("--anchor-day must be between 1 and 31, or 0 to clear it")
		}
		change.AnchorDay = &v
	}
	if flags.Changed("until") {
		raw, _ := flags.GetString("until")
		var endsAt time.Time
		if raw != "" {
			day, err := time.Parse(time.DateOnly, raw)
			if err != nil {
				return corebilling.Change{}, fmt.Errorf("--until must be a YYYY-MM-DD date, got %q", raw)
			}
			// The date an operator types is the last day the deal runs; the resolver's
			// comparison is half-open, so what gets stored is the following midnight.
			endsAt = corebilling.ContractEndExclusive(day)
		}
		change.ContractEndsAt = &endsAt
	}
	if flags.Changed("note") {
		v, _ := flags.GetString("note")
		change.Note = &v
	}
	if flags.Changed("provider-product") {
		v, _ := flags.GetString("provider-product")
		change.ProviderProductID = &v
	}
	return change, nil
}

// grantableSlugs names what --plan accepts for a NEW grant, the list a typo wants
// back. Trial is extend-trial's alone; a retired tier is kept only for its holders,
// and is still settable for an org already on it.
func grantableSlugs() []string {
	out := make([]string, 0, len(corebilling.Plans()))
	for _, p := range corebilling.Plans() {
		if p.Slug == corebilling.SlugTrial || p.Retired {
			continue
		}
		out = append(out, p.Slug)
	}
	return out
}

// billingRunE loads .env and moves the logs to stderr, so a command's report is
// the only thing on stdout and stays pipeable.
func billingRunE(fn func(context.Context, *cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

		return fn(ctx, cmd, args)
	}
}

// mustMarkRequired panics on a flag name that does not exist, which is a wiring
// typo rather than anything an operator can cause.
func mustMarkRequired(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("pug billing: %s has no %s flag: %v", cmd.Name(), name, err))
		}
	}
}
