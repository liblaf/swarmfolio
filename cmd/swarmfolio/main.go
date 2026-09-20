package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	_ "time/tzdata"

	"github.com/liblaf/swarmfolio/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer cancel()
	command := cli.New(os.Stdout, os.Stderr)
	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "swarmfolio:", err)
		os.Exit(1)
	}
}
