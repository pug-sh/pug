package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/pug-sh/pug/internal/app/migrate/clickhouse"
	migratenats "github.com/pug-sh/pug/internal/app/migrate/nats"
	"github.com/pug-sh/pug/internal/app/migrate/postgres"
	chseed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	"github.com/pug-sh/pug/internal/app/server"
	// "github.com/pug-sh/pug/internal/app/workers/campaigns"
	// "github.com/pug-sh/pug/internal/app/workers/devices"
	emailworker "github.com/pug-sh/pug/internal/app/workers/email"
	eventsworker "github.com/pug-sh/pug/internal/app/workers/events"
	"github.com/pug-sh/pug/internal/app/workers/profiles/alias"
	"github.com/pug-sh/pug/internal/app/workers/profiles/identify"
	"github.com/pug-sh/pug/internal/app/workers/profiles/upsert"
	// "github.com/pug-sh/pug/internal/app/workers/scheduler"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/templates"
	"github.com/pug-sh/pug/internal/slogx"
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
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

// var deviceCmd = &cobra.Command{
// 	Use:   "device",
// 	Short: "Start the device worker",
// 	Run:   run(devices.Run),
// }

// var campaignCmd = &cobra.Command{
// 	Use:   "campaign",
// 	Short: "Start the campaign worker",
// 	Run:   run(campaigns.Run),
// }

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Start the events worker",
	Run:   run(eventsworker.Run),
}

var emailCmd = &cobra.Command{
	Use:   "email",
	Short: "Start the transactional email worker",
	Run:   run(emailworker.Run),
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

// var schedulerCmd = &cobra.Command{
// 	Use:   "scheduler",
// 	Short: "Start the scheduler worker",
// 	Run:   run(scheduler.Run),
// }

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start the Pug server and workers for development",
	Run: func(cmd *cobra.Command, args []string) {
		sigCtx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(sigCtx, "No .env file found, relying on environment variables")
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
		fmt.Println("  "+yellow+"Events:"+reset, "events")
		// fmt.Println("  "+yellow+"Campaigns:"+reset, "campaigns")
		// fmt.Println("  "+yellow+"Devices:"+reset, "devices")
		emailEnabled, emailStatus := emailDevStatus()
		fmt.Println("  "+yellow+"Email:"+reset, emailStatus)
		// fmt.Println("  "+yellow+"Scheduler:"+reset, "scheduler")
		fmt.Println()

		fmt.Println(green + "  Press Ctrl+C to stop" + reset)
		fmt.Println()

		g, ctx := errgroup.WithContext(sigCtx)
		// g.Go(func() error { return devices.Run(ctx) })
		// g.Go(func() error { return campaigns.Run(ctx) })
		g.Go(func() error { return eventsworker.Run(ctx) })
		if emailEnabled {
			g.Go(func() error { return emailworker.Run(ctx) })
		}
		g.Go(func() error { return identify.Run(ctx) })
		g.Go(func() error { return alias.Run(ctx) })
		g.Go(func() error { return upsert.Run(ctx) })
		// g.Go(func() error { return scheduler.Run(ctx) })
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
	Run:   runMigrate(postgres.Up, postgres.Down),
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

var clickhouseSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed ClickHouse with events for the first user and project",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		count, _ := cmd.Flags().GetInt64("count")
		batchSize, _ := cmd.Flags().GetInt("batch")
		file, _ := cmd.Flags().GetString("file")
		noReset, _ := cmd.Flags().GetBool("no-reset")

		truncate := true
		if !noReset {
			slog.InfoContext(ctx, "rolling back all clickhouse migrations")
			if err := clickhouse.Down(ctx, 0); err != nil {
				slog.ErrorContext(ctx, "migrate down error", slogx.Error(err))
				os.Exit(1)
			}
			slog.InfoContext(ctx, "applying all clickhouse migrations")
			if err := clickhouse.Up(ctx, 0); err != nil {
				slog.ErrorContext(ctx, "migrate up error", slogx.Error(err))
				os.Exit(1)
			}
			truncate = false
		}

		if err := chseed.Run(ctx, count, batchSize, file, truncate); err != nil {
			slog.ErrorContext(ctx, "seed error", slogx.Error(err))
			os.Exit(1)
		}
	},
}

var postgresSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed PostgreSQL with test user and default project",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		noReset, _ := cmd.Flags().GetBool("no-reset")
		if !noReset {
			slog.InfoContext(ctx, "rolling back all postgres migrations")
			if err := postgres.Down(ctx, 0); err != nil {
				slog.ErrorContext(ctx, "migrate down error", slogx.Error(err))
				os.Exit(1)
			}
			slog.InfoContext(ctx, "applying all postgres migrations")
			if err := postgres.Up(ctx, 0); err != nil {
				slog.ErrorContext(ctx, "migrate up error", slogx.Error(err))
				os.Exit(1)
			}
		}

		if err := pgseed.Run(ctx); err != nil {
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
	// workerCmd.AddCommand(deviceCmd)
	// workerCmd.AddCommand(campaignCmd)
	workerCmd.AddCommand(eventsCmd)
	workerCmd.AddCommand(emailCmd)
	// workerCmd.AddCommand(schedulerCmd)

	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(workerCmd)
	rootCmd.AddCommand(devCmd)

	emailPreviewCmd.Flags().BoolVar(&emailPreviewText, "text", false, "render the plaintext twin instead of HTML")
	emailPreviewCmd.Flags().StringVar(&emailPreviewOut, "out", "", "write output to a file instead of stdout")
	emailToolCmd.AddCommand(emailPreviewCmd)
	rootCmd.AddCommand(emailToolCmd)

	postgresMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	postgresMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	postgresCmd := &cobra.Command{
		Use:   "postgres",
		Short: "PostgreSQL related commands",
	}
	postgresCmd.AddCommand(postgresMigrateCmd)
	postgresCmd.AddCommand(postgresSeedCmd)

	natsCmd := &cobra.Command{
		Use:   "nats",
		Short: "NATS related commands",
	}
	natsCmd.AddCommand(natsMigrateCmd)

	clickhouseMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	clickhouseMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	clickhouseSeedCmd.Flags().Int64P("count", "c", 10_000_000, "total number of events to generate (used when no file provided)")
	clickhouseSeedCmd.Flags().IntP("batch", "b", 10_000, "number of events per ClickHouse batch")
	clickhouseSeedCmd.Flags().StringP("file", "f", "", "CSV file to import (REES46 format: event_time,order_id,product_id,category_id,category_code,brand,price,user_id)")
	clickhouseSeedCmd.Flags().Bool("no-reset", false, "skip migrate down/up; truncate events table instead")
	postgresSeedCmd.Flags().Bool("no-reset", false, "skip migrate down/up before seeding")

	clickhouseCmd := &cobra.Command{
		Use:   "clickhouse",
		Short: "ClickHouse related commands",
	}
	clickhouseCmd.AddCommand(clickhouseMigrateCmd)
	clickhouseCmd.AddCommand(clickhouseSeedCmd)

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
