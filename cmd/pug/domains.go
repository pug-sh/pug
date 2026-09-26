package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	appdomains "github.com/pug-sh/pug/internal/app/domains"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/spf13/cobra"
)

var domainsCmd = newDomainsCmd()

func newDomainsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "Verify and inspect the domains orgs have claimed",
		Long: "Operator commands for org domains. They use only Postgres and never change\n" +
			"an org's settings. `verify` skips DNS, for a server that can't see public DNS\n" +
			"or for a support case.",
	}
	for _, c := range []*cobra.Command{
		{
			Use:   "verify <org-id> <domain>",
			Short: "Mark the org's domain verified, adding it if the org has not",
			Args:  cobra.ExactArgs(2),
			RunE: domainsRunE(func(ctx context.Context, cli *appdomains.CLI, cmd *cobra.Command, args []string) error {
				return cli.Verify(ctx, cmd.OutOrStdout(), args[0], args[1])
			}),
		},
		{
			Use:   "show <domain>",
			Short: "List every org that has added the domain",
			Args:  cobra.ExactArgs(1),
			RunE: domainsRunE(func(ctx context.Context, cli *appdomains.CLI, cmd *cobra.Command, args []string) error {
				return cli.Show(ctx, cmd.OutOrStdout(), args[0])
			}),
		},
		{
			Use:   "release <org-id> <domain>",
			Short: "Drop one org's claim to the domain, e.g. in a dispute",
			Args:  cobra.ExactArgs(2),
			RunE: domainsRunE(func(ctx context.Context, cli *appdomains.CLI, cmd *cobra.Command, args []string) error {
				return cli.Release(ctx, cmd.OutOrStdout(), args[0], args[1])
			}),
		},
	} {
		c.SilenceUsage = true
		cmd.AddCommand(c)
	}
	return cmd
}

// Logs move to stderr so a command's report is the only thing on stdout.
func domainsRunE(fn func(context.Context, *appdomains.CLI, *cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		dotenv.LoadOrExit(ctx)

		cli, err := appdomains.New(ctx)
		if err != nil {
			return err
		}
		defer cli.Close()

		return fn(ctx, cli, cmd, args)
	}
}
