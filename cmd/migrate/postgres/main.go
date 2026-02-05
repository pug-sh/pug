package main

import (
	"context"
	"os"

	"github.com/fivebitsio/cotton/internal/commands"
)

func main() {
	if err := commands.MigrateCmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
