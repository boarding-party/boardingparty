// Command boardingparty is the operator CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/boarding-party/boardingparty/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := cli.NewRoot().ExecuteContext(ctx)
	stop()
	var ee cli.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", cli.FormatError(err))
		os.Exit(1)
	}
}
