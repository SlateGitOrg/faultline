// Command faultline is a fault-injection toolkit: L4/L7 proxies driven by a
// control API, and a chaos experiment runner. See 'faultline help'.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/SlateGitOrg/faultline/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
