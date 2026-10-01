package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yasyf/cc-remote/internal/cli"
	applog "github.com/yasyf/cc-remote/internal/log"
)

func main() {
	applog.Setup()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cli.NewRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "cc-remote:", err)
		os.Exit(1)
	}
}
