package main

import (
	"context"
	"os"

	"github.com/fivebitsio/cotton/internal/commands"
)

func main() {
	if err := commands.SubscriptionWorkerCmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
