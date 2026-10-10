package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	appretention "github.com/pug-sh/pug/internal/app/retention"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/spf13/cobra"
)

var retentionCmd = newRetentionCmd()

func newRetentionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "Manage event retention as an operator",
	}
	expire := &cobra.Command{
		Use:   "expire <org-id>",
		Short: "Start the 30-day wait before an org that stopped paying drops to free's length",
		Long: "An org that stops paying reports free's length at once, but its deletes keep\n" +
			"its paid length until an operator runs this. It then waits 30 days, and paying\n" +
			"again before they end keeps the paid length. Refuses an org that still pays or\n" +
			"has nothing waiting. Run it with the server's environment: it reads the\n" +
			"billing switch to tell whether the org pays.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer done()

			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
			dotenv.LoadOrExit(ctx)

			cli, err := appretention.New(ctx)
			if err != nil {
				return err
			}
			defer cli.Close()

			actor, _ := cmd.Flags().GetString("actor")
			return cli.Expire(ctx, cmd.OutOrStdout(), args[0], actor)
		},
	}
	expire.Flags().String("actor", "", "who is expiring it, recorded on its retention state (e.g. \"praveen/TICKET-1\")")
	mustMarkRequired(expire, "actor")
	cmd.AddCommand(expire)
	return cmd
}
