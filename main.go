package main

import (
	"context"
	"os"

	"github.com/fivebitsio/cotton/internal/commands"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "cotton",
	Short: "Cotton is a unified command line tool for managing the Cotton application",
	Long:  `Cotton is a unified command line tool for managing the Cotton application.`,
}

func init() {
	rootCmd.AddCommand(commands.ServerCmd)
	rootCmd.AddCommand(commands.WorkerCmd)
	rootCmd.AddCommand(commands.DevCmd)

	postgresCmd := &cobra.Command{
		Use:   "postgres",
		Short: "PostgreSQL related commands",
		Long:  `Commands for managing PostgreSQL database.`,
	}
	postgresCmd.AddCommand(commands.MigrateCmd)

	pulsarCmd := &cobra.Command{
		Use:   "pulsar",
		Short: "Pulsar related commands",
		Long:  `Commands for managing Pulsar messaging system.`,
	}
	pulsarCmd.AddCommand(commands.PulsarMigrateCmd)

	clickhouseCmd := &cobra.Command{
		Use:   "clickhouse",
		Short: "ClickHouse related commands",
		Long:  `Commands for managing ClickHouse database.`,
	}
	clickhouseCmd.AddCommand(commands.ClickhouseMigrateCmd)

	rootCmd.AddCommand(postgresCmd)
	rootCmd.AddCommand(pulsarCmd)
	rootCmd.AddCommand(clickhouseCmd)
}

func main() {
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
