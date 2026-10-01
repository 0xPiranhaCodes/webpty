package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/0xPiranhaCodes/webpty/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.CLI{Lookup: os.LookupEnv, Stdout: os.Stdout, Stderr: os.Stderr}.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
