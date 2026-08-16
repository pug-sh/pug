package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	usagecron "github.com/pug-sh/pug/internal/app/cron/usage"
	"github.com/pug-sh/pug/internal/app/migrate/clickhouse"
	migratenats "github.com/pug-sh/pug/internal/app/migrate/nats"
	migratepostgres "github.com/pug-sh/pug/internal/app/migrate/postgres"
	"github.com/pug-sh/pug/internal/app/seed"
	"github.com/pug-sh/pug/internal/app/server"
	"github.com/pug-sh/pug/internal/app/workers/compliance"
	demoworker "github.com/pug-sh/pug/internal/app/workers/demo"
	emailworker "github.com/pug-sh/pug/internal/app/workers/email"
	eventsworker "github.com/pug-sh/pug/internal/app/workers/events"
	"github.com/pug-sh/pug/internal/app/workers/profiles/alias"
	"github.com/pug-sh/pug/internal/app/workers/profiles/identify"
	"github.com/pug-sh/pug/internal/app/workers/profiles/upsert"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/templates"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	reset  = "\033[0m"
	cyan   = "\033[36m"
	green  = "\033[32m"
	yellow = "\033[33m"
	red    = "\033[31m"
	bold   = "\033[1m"
)

// run creates a signal-aware context, loads .env, and runs fn.
func run(fn func(ctx context.Context) error) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		if err := fn(ctx); err != nil {
			slog.ErrorContext(ctx, "fatal error", slogx.Error(err))
			os.Exit(1)
		}
	}
}

// runMigrate creates a signal-aware context, loads .env, reads --direction and --num flags,
// validates direction, and calls the appropriate up/down function.
func runMigrate(up, down func(ctx context.Context, num int) error) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		direction, _ := cmd.Flags().GetString("direction")
		num, _ := cmd.Flags().GetInt("num")

		var err error
		switch direction {
		case "up":
			err = up(ctx, num)
		case "down":
			err = down(ctx, num)
		default:
			slog.ErrorContext(ctx, "invalid migration direction, must be 'up' or 'down'", slog.String("direction", direction))
			os.Exit(1)
		}
		if err != nil {
			slog.ErrorContext(ctx, "migration error", slogx.Error(err))
			os.Exit(1)
		}
	}
}

func emailDevStatus() (bool, string) {
	if os.Getenv("PUG_DASHBOARD_BASE_URL") == "" {
		return false, "disabled (missing PUG_DASHBOARD_BASE_URL)"
	}
	if os.Getenv("PUG_EMAIL_FROM") == "" {
		return false, "disabled (missing PUG_EMAIL_FROM)"
	}

	provider := strings.TrimSpace(strings.ToLower(os.Getenv("PUG_EMAIL_PROVIDER")))
	if provider == "" {
		provider = "resend"
	}

	switch provider {
	case "resend":
		if os.Getenv("PUG_RESEND_API_KEY") == "" {
			return false, "disabled (missing PUG_RESEND_API_KEY for resend)"
		}
		return true, "email"
	case "ses":
		return true, "email"
	default:
		return false, fmt.Sprintf("disabled (unsupported provider %q)", provider)
	}
}

var rootCmd = &cobra.Command{
	Use:   "pug",
	Short: "Pug is a unified command line tool for managing the Pug application",
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Pug server",
	Run:   run(server.Run),
}

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Worker related commands",
}

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Profile worker related commands",
}

var profileIdentifyCmd = &cobra.Command{
	Use:   "identify",
	Short: "Start the profile identify worker",
	Run:   run(identify.Run),
}

var profileAliasCmd = &cobra.Command{
	Use:   "alias",
	Short: "Start the profile alias worker",
	Run:   run(alias.Run),
}

var profileUpsertCmd = &cobra.Command{
	Use:   "upsert",
	Short: "Start the profile upsert worker",
	Run:   run(upsert.Run),
}

var complianceCmd = &cobra.Command{
	Use:   "compliance",
	Short: "Start the compliance worker (GDPR/DPDP erasure, export, retention)",
	Run:   run(compliance.Run),
}

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Start the events worker",
	Run:   run(eventsworker.Run),
}

var demoWorkerCmd = &cobra.Command{
	Use:   "demo",
	Short: "Start the rolling demo-traffic generator (seeds the demo project on first run)",
	Run:   run(demoworker.Run),
}

var emailCmd = &cobra.Command{
	Use:   "email",
	Short: "Start the transactional email worker",
	Run:   run(emailworker.Run),
}

// Scheduled jobs. Unlike `pug worker`, these run once and exit — non-zero when
// the pass failed, which is a k8s CronJob's only success signal.
var cronCmd = &cobra.Command{
	Use:   "cron",
	Short: "Run a scheduled job once and exit",
}

var cronUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Run an event usage metering pass",
	Run:   run(usagecron.Run),
}

var (
	emailPreviewText bool
	emailPreviewOut  string
)

// emailToolCmd is a top-level group distinct from `worker email`.
var emailToolCmd = &cobra.Command{
	Use:   "email",
	Short: "Email tooling",
}

var emailPreviewCmd = &cobra.Command{
	Use:   "preview <magic_link|invite|provider_test>",
	Short: "Render a transactional email to HTML (or --text) for preview",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dashboardURL := os.Getenv("PUG_DASHBOARD_BASE_URL")
		if dashboardURL == "" {
			dashboardURL = "https://app.pug.sh"
		}
		brand := coreemail.Brand{
			ProductName:  templates.ProductName,
			LogoURL:      os.Getenv("PUG_EMAIL_LOGO_URL"),
			DashboardURL: strings.TrimRight(dashboardURL, "/"),
		}
		r := coreemail.NewRenderer(brand)
		sampleLink := brand.DashboardURL + "/magic-link?token=sample-token-1234567890"

		html, text, err := renderEmailPreview(cmd.Context(), r, args[0], sampleLink)
		if err != nil {
			return err
		}

		out := html
		if emailPreviewText {
			out = text
		}
		if emailPreviewOut != "" {
			return os.WriteFile(emailPreviewOut, []byte(out), 0o644)
		}
		_, err = os.Stdout.WriteString(out)
		return err
	},
}

// renderEmailPreview dispatches to the renderer for the named email kind. It is
// kept separate from the cobra wiring so the kind->renderer mapping is
// unit-testable.
func renderEmailPreview(ctx context.Context, r *coreemail.Renderer, kind, sampleLink string) (html, text string, err error) {
	switch kind {
	case "magic_link":
		return r.MagicLink(ctx, sampleLink)
	case "invite":
		return r.Invite(ctx, "Acme Inc", "Alice", sampleLink)
	case "provider_test":
		return r.ProviderTest(ctx)
	default:
		return "", "", fmt.Errorf("unknown email %q (want magic_link|invite|provider_test)", kind)
	}
}

// billingCmd is the operator tool for an org's entitlement. Postgres only, and
// deliberately not gated on PUG_BILLING_ENABLED: that flag hides billing from a
// deployment's users, and an operator still has to be able to prepare rows
// before it goes on.
var billingCmd = &cobra.Command{
	Use:   "billing",
	Short: "Inspect and grant org entitlements (plans, quotas, trials)",
	Long: "Operator tool for an org's entitlement. Postgres only — no payments\n" +
		"provider is involved, and nothing here charges anybody. Every write is\n" +
		"recorded in the entitlement history, attributed to the --actor you pass.",
}

// Both the resolved entitlement and the row behind it, because the interesting
// bugs live in the gap between them.
var billingShowCmd = &cobra.Command{
	Use:   "show <org-id>",
	Short: "Print the resolved entitlement, and optionally its history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return withBilling(cmd, func(ctx context.Context, svc *corebilling.Service) error {
			ent, err := svc.GetEntitlement(ctx, args[0], time.Now())
			if err != nil {
				return err
			}
			rec, err := svc.StoredRecord(ctx, args[0])
			if err != nil {
				return err
			}
			printEntitlement(args[0], ent)
			printStoredRecord(args[0], rec)
			printBillingFlagNote(ctx)

			if withHistory, _ := cmd.Flags().GetBool("history"); !withHistory {
				return nil
			}
			entries, err := svc.History(ctx, args[0])
			if err != nil {
				return err
			}
			printBillingHistory(entries)
			return nil
		})
	},
}

var billingSetCmd = &cobra.Command{
	Use:   "set <org-id> --plan <slug>",
	Short: "Grant a plan, with optional negotiated overrides",
	Long: "Grants a plan. A flag that is not passed leaves the stored value alone,\n" +
		"because the common re-set is a renewal — a new --until on terms that have\n" +
		"not changed — and silently reverting a negotiated quota to a catalog\n" +
		"number is the worst thing this command could do. Pass the empty value\n" +
		"(--events 0, --name \"\", --anchor-day 0, --until \"\") to clear an override.\n\n" +
		"This grants the QUOTA. What the deal is charged lives in the payments\n" +
		"provider and is not stored here — record it in --note if you want it\n" +
		"written down.\n\n" +
		"A custom deal is one call:\n" +
		"  pug billing set o_2f9k --plan custom --events 5000000 \\\n" +
		"      --name \"Acme Enterprise\" --until 2027-01-01 \\\n" +
		"      --note \"$400/mo, INV-123\" --actor rita@pug.sh",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Before the pools: a flag typo should not need a reachable database.
		actor, err := billingActor(cmd)
		if err != nil {
			return err
		}
		change, err := billingChange(cmd)
		if err != nil {
			return err
		}
		return withBilling(cmd, func(ctx context.Context, svc *corebilling.Service) error {
			rec, err := svc.SetPlan(ctx, args[0], actor, change)
			if err != nil {
				return err
			}
			printStoredRecord(args[0], rec)
			printBillingFlagNote(ctx)
			return nil
		})
	},
}

var billingExtendTrialCmd = &cobra.Command{
	Use:   "extend-trial <org-id> --days <n>",
	Short: "Move the org's trial end to N days from now",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Before the pools: a flag typo should not need a reachable database.
		actor, err := billingActor(cmd)
		if err != nil {
			return err
		}
		days, _ := cmd.Flags().GetInt("days")
		if days <= 0 || days > corebilling.MaxTrialDays {
			return fmt.Errorf("--days must be between 1 and %d", corebilling.MaxTrialDays)
		}
		return withBilling(cmd, func(ctx context.Context, svc *corebilling.Service) error {
			rec, err := svc.ExtendTrial(ctx, args[0], actor, days, time.Now())
			if err != nil {
				return err
			}
			printStoredRecord(args[0], rec)
			printBillingFlagNote(ctx)
			return nil
		})
	},
}

var billingClearCmd = &cobra.Command{
	Use:   "clear <org-id>",
	Short: "Delete the entitlement, returning the org to the derived floors",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		actor, err := billingActor(cmd)
		if err != nil {
			return err
		}
		return withBilling(cmd, func(ctx context.Context, svc *corebilling.Service) error {
			if err := svc.Clear(ctx, args[0], actor); err != nil {
				if errors.Is(err, corebilling.ErrNoEntitlement) {
					fmt.Printf("%s: nothing stored; the org was already on the derived floors\n", args[0])
					return nil
				}
				return err
			}
			fmt.Printf("%s: entitlement cleared; the org is back on the derived floors\n", args[0])
			printBillingFlagNote(ctx)
			return nil
		})
	},
}

// withBilling gives one command a signal-aware context, a loaded .env and a
// service over pools it tears down after — the shape run() gives the
// long-running commands, which these cannot use because they take arguments.
//
// billingEnabled is true here regardless of PUG_BILLING_ENABLED: resolving every
// org as "no quota" would leave this tool unable to show an operator what it
// just wrote.
func withBilling(cmd *cobra.Command, fn func(context.Context, *corebilling.Service) error) error {
	ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return fmt.Errorf("postgres config: %w", err)
	}
	// One pool, serving as both reader and writer: every command here writes and
	// then prints what it wrote, so reading from a replica would let `set` report
	// the row it just replaced.
	pool, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()

	svc, err := corebilling.NewService(pool, pool, true)
	if err != nil {
		return err
	}
	return fn(ctx, svc)
}

// billingChange maps `set`'s flags onto the change. A flag that was not passed
// stays nil and leaves the stored value alone; the empty value clears it. Kept
// out of RunE so the mapping is testable without a database.
func billingChange(cmd *cobra.Command) (corebilling.Change, error) {
	var change corebilling.Change
	plan, _ := cmd.Flags().GetString("plan")
	if plan == "" {
		return change, errors.New("--plan is required")
	}
	change.PlanSlug = plan

	if cmd.Flags().Changed("events") {
		v, _ := cmd.Flags().GetInt64("events")
		// Only 0 clears. A negative is a typo, and treating it as a clear would
		// silently revert a negotiated quota to the catalog number.
		if v < 0 {
			return change, errors.New("--events must be 0 or more (0 clears the override)")
		}
		change.IncludedEvents = new(v)
	}
	if cmd.Flags().Changed("name") {
		v, _ := cmd.Flags().GetString("name")
		// Mirrors display_name_override's varchar(150), which would otherwise fail
		// inside the transaction and be logged as a pug fault rather than a typo.
		if len(v) > billingNameMaxLen {
			return change, fmt.Errorf("--name is limited to %d characters", billingNameMaxLen)
		}
		change.DisplayName = new(v)
	}
	if cmd.Flags().Changed("anchor-day") {
		v, _ := cmd.Flags().GetInt("anchor-day")
		if v < 0 || v > 31 {
			return change, errors.New("--anchor-day must be between 1 and 31 (0 clears it)")
		}
		change.AnchorDay = new(v)
	}
	if cmd.Flags().Changed("until") {
		v, _ := cmd.Flags().GetString("until")
		if v == "" {
			change.ContractEndsAt = new(time.Time)
		} else {
			at, err := time.Parse(time.DateOnly, v)
			if err != nil {
				return change, fmt.Errorf("--until must be YYYY-MM-DD: %w", err)
			}
			end := corebilling.ContractEndExclusive(at)
			// A past date stores a grant that resolves straight back to free, and
			// `set` echoes only the stored row, so the operator would see success.
			if !end.After(time.Now()) {
				return change, fmt.Errorf("--until %s has already passed; the grant would lapse immediately", v)
			}
			change.ContractEndsAt = &end
		}
	}
	if cmd.Flags().Changed("note") {
		v, _ := cmd.Flags().GetString("note")
		change.Note = new(v)
	}
	return change, nil
}

// billingSetFlags is shared with the tests, so a flag can't be exercised in a
// shape the real command never offers.
func billingSetFlags(cmd *cobra.Command) {
	cmd.Flags().String("plan", "", "plan slug ("+billingPlanSlugs()+")")
	cmd.Flags().Int64("events", 0, "negotiated monthly event quota (0 clears the override)")
	cmd.Flags().String("name", "", "negotiated plan name shown to the org (empty clears)")
	cmd.Flags().Int("anchor-day", 0, "day of month the quota window starts (0 uses the org's signup day)")
	cmd.Flags().String("until", "", "last day the plan runs before lapsing back to free, YYYY-MM-DD (empty clears)")
	cmd.Flags().String("note", "", "operator remark recorded on the row and in its history")
}

// billingPlanSlugs keeps --plan's help from drifting off the catalog. Trial is
// omitted: it is derived, never granted.
func billingPlanSlugs() string {
	out := make([]string, 0, len(corebilling.Plans()))
	for _, p := range corebilling.Plans() {
		if p.Slug != corebilling.SlugTrial {
			out = append(out, p.Slug)
		}
	}
	return strings.Join(out, ", ")
}

// billingActor is stated rather than detected: this runs from a pod, where the
// OS user is the image's uid and reads the same for every operator. Required, so
// a commercial agreement cannot change hands unattributed.
func billingActor(cmd *cobra.Command) (string, error) {
	actor, _ := cmd.Flags().GetString("actor")
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("--actor must say who is making this change")
	}
	// Rejected rather than truncated: the history insert shares the change's
	// transaction, so an over-long actor would roll the change itself back.
	if len(actor) > billingActorMaxLen {
		return "", fmt.Errorf("--actor is limited to %d characters", billingActorMaxLen)
	}
	return actor, nil
}

// Matches billing_entitlement_history.actor.
const billingActorMaxLen = 150

// Matches billing_entitlements.display_name_override.
const billingNameMaxLen = 150

// billingActorFlag is registered on every mutating command; cobra enforces its
// presence, billingActor enforces its shape.
func billingActorFlag(cmd *cobra.Command) {
	cmd.Flags().String("actor", "", "who is making this change, recorded in the history (required)")
	_ = cmd.MarkFlagRequired("actor")
}

func printEntitlement(orgID string, ent corebilling.Entitlement) {
	fmt.Printf("org           %s\n", orgID)
	fmt.Printf("plan          %s (%s)\n", ent.DisplayName, ent.Slug)
	fmt.Printf("status        %s\n", ent.Status)
	fmt.Printf("quota         %s\n", quotaText(ent.IncludedEvents))
	// List price, not what this org is charged — a negotiated amount lives in the
	// payments provider and pug does not store it.
	fmt.Printf("list price    %s\n", priceText(ent.PriceCents, ent.Currency))
	fmt.Printf("period        %s -> %s\n", isoDay(ent.PeriodStart), isoDay(ent.PeriodEnd))
	if !ent.TrialEndsAt.IsZero() {
		fmt.Printf("trial ends    %s\n", isoDay(ent.TrialEndsAt))
	}
	if !ent.ContractEndsAt.IsZero() {
		fmt.Printf("contract ends %s\n", isoDay(ent.ContractEndsAt))
	}
}

// printStoredRecord shows the row behind the resolved answer, and is what a write
// echoes back. An override that is not in force today still applies to the next
// `set`, so an operator has to see it either way.
func printStoredRecord(orgID string, rec corebilling.Record) {
	fmt.Printf("\nstored row (%s)\n", orgID)
	if !rec.Present {
		fmt.Printf("  none — the org resolves from its age alone\n")
		return
	}
	fmt.Printf("  plan          %s\n", rec.PlanSlug)
	fmt.Printf("  events        %s\n", overrideText(rec.IncludedEventsOverride > 0, rec.IncludedEventsOverride))
	fmt.Printf("  name          %s\n", textOrNone(rec.DisplayNameOverride))
	fmt.Printf("  anchor day    %s\n", overrideText(rec.AnchorDay > 0, int64(rec.AnchorDay)))
	fmt.Printf("  contract ends %s\n", dateOrNone(rec.ContractEndsAt))
	fmt.Printf("  trial ends    %s\n", dateOrNone(rec.TrialEndsAt))
	fmt.Printf("  note          %s\n", textOrNone(rec.Note))
}

// printBillingFlagNote reads PUG_BILLING_ENABLED for display only — this tool
// always resolves as if billing were on, so the flag cannot be inferred from the
// answer above, and an operator preparing rows needs to know the server is
// ignoring them.
func printBillingFlagNote(ctx context.Context) {
	var cfg corebilling.Config
	// A value the server refuses to start on must not read here as "billing is on".
	if err := envconfig.Process(ctx, &cfg); err != nil {
		fmt.Printf("\nnote: PUG_BILLING_ENABLED could not be read (%v), so what the server\n"+
			"      resolves is unknown — and it will refuse to start on this value.\n", err)
		return
	}
	if cfg.Enabled {
		return
	}
	fmt.Printf("\nnote: PUG_BILLING_ENABLED is off, so the server resolves every org as\n" +
		"      no-quota regardless of what is stored. The above is what it would\n" +
		"      report once the switch is on.\n")
}

func printBillingHistory(entries []corebilling.HistoryEntry) {
	fmt.Printf("\nhistory (%d, newest first)\n", len(entries))
	for _, e := range entries {
		var b strings.Builder
		fmt.Fprintf(&b, "  %s  %-24s ", e.ChangedAt.UTC().Format(time.RFC3339), e.Actor)
		if !e.Record.Present {
			b.WriteString("cleared")
		} else {
			fmt.Fprintf(&b, "plan=%s", e.Record.PlanSlug)
			if e.Record.IncludedEventsOverride > 0 {
				fmt.Fprintf(&b, " events=%d", e.Record.IncludedEventsOverride)
			}
			if e.Record.Note != "" {
				fmt.Fprintf(&b, " note=%q", e.Record.Note)
			}
		}
		fmt.Println(b.String())
	}
}

// quotaText spells out the absence rather than printing a bare number, because
// "no quota" and "a quota of zero" are the one distinction this subsystem exists
// to keep.
func quotaText(v *int64) string {
	if v == nil {
		return "none (no quota is enforced or displayed)"
	}
	return fmt.Sprintf("%d events / period", *v)
}

func priceText(cents *int64, currency string) string {
	if cents == nil {
		return "none recorded"
	}
	return fmt.Sprintf("%d %s (minor units)", *cents, currency)
}

func overrideText(set bool, v int64) string {
	if !set {
		return "none (the plan's)"
	}
	return fmt.Sprintf("%d", v)
}

func textOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func dateOrNone(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return isoDay(t)
}

func isoDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }

var devCmd = &cobra.Command{
	Use:     "dev",
	Aliases: []string{"start"},
	Short:   "Start the Pug server and workers",
	Run: func(cmd *cobra.Command, args []string) {
		sigCtx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(sigCtx, "No .env file found, relying on environment variables")
		}

		// `pug dev` runs every worker in one local process; the health/readiness
		// endpoints are for orchestrated deployments, so force them off here to
		// avoid binding an unneeded listener in local dev.
		if err := os.Setenv(natsworker.HealthAddrEnv, "off"); err != nil {
			slog.WarnContext(sigCtx, "failed to disable worker health endpoint for dev",
				slog.String("env", natsworker.HealthAddrEnv), slogx.Error(err))
		}

		port := os.Getenv("PUG_SERVER_PORT")
		if port == "" {
			port = "3000"
		}

		fmt.Println()
		fmt.Println(cyan + bold + "8b,dPPYba,  88       88  ,adPPYb,d8" + reset)
		fmt.Println(cyan + "88P'    \"8a 88       88 a8\"    `Y88" + reset)
		fmt.Println(cyan + "88       d8 88       88 8b       88" + reset)
		fmt.Println(cyan + "88b,   ,a8\" \"8a,   ,a88 \"8a,   ,d88" + reset)
		fmt.Println(cyan + "88`YbbdP\"'   `\"YbbdP'Y8  `\"YbbdP\"Y8" + reset)
		fmt.Println(cyan + "88                       aa,    ,88" + reset)
		fmt.Println(cyan + "88                        \"Y8bbdP\"" + reset)
		fmt.Println()

		fmt.Println(bold+"Server:"+reset, green+"http://localhost:"+port+reset)
		fmt.Println()

		printInfrastructure(sigCtx)

		fmt.Println(bold + "Workers:" + reset)
		fmt.Println("  "+yellow+"Profiles:"+reset, "identify, alias, upsert")
		fmt.Println("  "+yellow+"Compliance:"+reset, "erase")
		fmt.Println("  "+yellow+"Events:"+reset, "events")
		emailEnabled, emailStatus := emailDevStatus()
		fmt.Println("  "+yellow+"Email:"+reset, emailStatus)
		demoEnabled := demoworker.Enabled(sigCtx)
		if demoEnabled {
			fmt.Println("  "+yellow+"Demo:"+reset, "rolling traffic for the demo project (seeds on first run)")
		} else {
			fmt.Println("  "+yellow+"Demo:"+reset, "disabled (set PUG_DEMO_ENABLED=true to enable)")
		}
		fmt.Println()

		// Listed but not started: metering is a CronJob in deploys, so dev has to
		// say so or usage silently reads back as "never metered".
		fmt.Println(bold + "Jobs:" + reset)
		fmt.Println("  "+yellow+"Usage metering:"+reset, "not scheduled — run", cyan+"pug cron usage"+reset, "for one pass")
		fmt.Println()

		fmt.Println(green + "  Press Ctrl+C to stop" + reset)
		fmt.Println()

		g, ctx := errgroup.WithContext(sigCtx)
		g.Go(func() error { return eventsworker.Run(ctx) })
		if emailEnabled {
			g.Go(func() error { return emailworker.Run(ctx) })
		}
		if demoEnabled {
			g.Go(func() error { return demoworker.Run(ctx) })
		}
		g.Go(func() error { return identify.Run(ctx) })
		g.Go(func() error { return alias.Run(ctx) })
		g.Go(func() error { return upsert.Run(ctx) })
		g.Go(func() error { return compliance.Run(ctx) })
		g.Go(func() error { return server.Run(ctx) })

		if err := g.Wait(); err != nil {
			slog.ErrorContext(sigCtx, "component stopped", slogx.Error(err))
		}

		slog.InfoContext(sigCtx, "Shutting down...")
	},
}

var postgresMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for postgres",
	Run:   runMigrate(migratepostgres.Up, migratepostgres.Down),
}

var natsMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Initialize NATS streams and consumers",
	Run:   run(migratenats.Run),
}

var clickhouseMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for clickhouse",
	Run:   runMigrate(clickhouse.Up, clickhouse.Down),
}

var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed the demo project (events first, then profiles for exactly those users)",
	Long: "Resets Postgres and ClickHouse (unless --no-reset), then runs the same\n" +
		"event-gated flow as the demo worker: ensure the demo account, backfill\n" +
		"events, seed Postgres profiles for only the users that produced events,\n" +
		"and copy them into ClickHouse. Profiles therefore exist only for users\n" +
		"with events — matching `pug worker demo`.",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		count, _ := cmd.Flags().GetInt64("count")
		batchSize, _ := cmd.Flags().GetInt("batch")
		noReset, _ := cmd.Flags().GetBool("no-reset")

		if err := seed.Run(ctx, seed.Options{Count: count, BatchSize: batchSize, NoReset: noReset}); err != nil {
			slog.ErrorContext(ctx, "seed error", slogx.Error(err))
			os.Exit(1)
		}
	},
}

func init() {
	profileCmd.AddCommand(profileIdentifyCmd)
	profileCmd.AddCommand(profileAliasCmd)
	profileCmd.AddCommand(profileUpsertCmd)
	workerCmd.AddCommand(profileCmd)
	workerCmd.AddCommand(eventsCmd)
	workerCmd.AddCommand(emailCmd)
	workerCmd.AddCommand(demoWorkerCmd)
	workerCmd.AddCommand(complianceCmd)

	cronCmd.AddCommand(cronUsageCmd)
	rootCmd.AddCommand(cronCmd)

	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(workerCmd)
	rootCmd.AddCommand(devCmd)

	seedCmd.Flags().Int64P("count", "c", 500_000, "total number of events to generate")
	seedCmd.Flags().IntP("batch", "b", 10_000, "number of events per ClickHouse batch")
	seedCmd.Flags().Bool("no-reset", false, "skip migrate down/up; truncate the demo tables and re-seed instead")
	rootCmd.AddCommand(seedCmd)

	emailPreviewCmd.Flags().BoolVar(&emailPreviewText, "text", false, "render the plaintext twin instead of HTML")
	emailPreviewCmd.Flags().StringVar(&emailPreviewOut, "out", "", "write output to a file instead of stdout")
	emailToolCmd.AddCommand(emailPreviewCmd)
	rootCmd.AddCommand(emailToolCmd)

	billingShowCmd.Flags().Bool("history", false, "also print the entitlement's change history")
	billingSetFlags(billingSetCmd)
	billingExtendTrialCmd.Flags().Int("days", 14, "days from now the trial should end")
	billingActorFlag(billingSetCmd)
	billingActorFlag(billingExtendTrialCmd)
	billingActorFlag(billingClearCmd)
	billingCmd.AddCommand(billingShowCmd)
	billingCmd.AddCommand(billingSetCmd)
	billingCmd.AddCommand(billingExtendTrialCmd)
	billingCmd.AddCommand(billingClearCmd)
	rootCmd.AddCommand(billingCmd)

	postgresMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	postgresMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	postgresCmd := &cobra.Command{
		Use:   "postgres",
		Short: "PostgreSQL related commands",
	}
	postgresCmd.AddCommand(postgresMigrateCmd)

	natsCmd := &cobra.Command{
		Use:   "nats",
		Short: "NATS related commands",
	}
	natsCmd.AddCommand(natsMigrateCmd)

	clickhouseMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	clickhouseMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	clickhouseCmd := &cobra.Command{
		Use:   "clickhouse",
		Short: "ClickHouse related commands",
	}
	clickhouseCmd.AddCommand(clickhouseMigrateCmd)

	rootCmd.AddCommand(postgresCmd)
	rootCmd.AddCommand(natsCmd)
	rootCmd.AddCommand(clickhouseCmd)
}

// redactURL replaces the user:password@ portion of a URL with xxxxx:xxxxx@.
// URLs without userinfo and URLs that fail to parse are returned unchanged.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.UserPassword("xxxxx", "xxxxx")
	return u.String()
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
