package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"acs.local/cli/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "acs:", err); writeErr != nil {
			os.Exit(1)
		}
		if ctx.Err() != nil {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
