package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/GoldBro233/MaintainedPackages/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
