package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	appprojects "github.com/pug-sh/pug/internal/app/projects"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/spf13/cobra"
)

var projectsCmd = newProjectsCmd()

func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "Delete projects as an operator",
	}
	del := &cobra.Command{
		Use:   "delete <project-id>",
		Short: "Delete a project without the admin check, or queue an id with no projects row",
		Long: "Does what an admin's delete does: hides the project, revokes its keys, share\n" +
			"links, campaigns and push credential, and queues its data for erasure. An id\n" +
			"with no projects row, such as a project deleted before deletions were\n" +
			"recorded, is only queued, so the purge job erases what ClickHouse holds.\n" +
			"Running it again changes nothing while the deletion is open, and reopens a\n" +
			"finished one. Run it with the server's environment: it clears the API-key\n" +
			"cache in the server's Redis.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, done := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer done()

			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
			dotenv.LoadOrExit(ctx)

			cli, err := appprojects.New(ctx)
			if err != nil {
				return err
			}
			defer cli.Close(ctx)

			actor, _ := cmd.Flags().GetString("actor")
			return cli.Delete(ctx, cmd.OutOrStdout(), args[0], actor)
		},
	}
	del.Flags().String("actor", "", "who is deleting it, recorded on the deletion it creates (e.g. \"praveen/TICKET-1\")")
	mustMarkRequired(del, "actor")
	cmd.AddCommand(del)
	return cmd
}
