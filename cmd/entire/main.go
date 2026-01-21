package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"entire.io/cli/cmd/entire/cli"
)

func main() {
	// Create context that cancels on interrupt
	ctx, cancel := context.WithCancel(context.Background())

	// Handle interrupt signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// Create and execute root command
	rootCmd := cli.NewRootCmd()
	err := rootCmd.ExecuteContext(ctx)

	if err != nil {
		// Don't print if the command already handled its own error output
		var silent *cli.SilentError
		if !errors.As(err, &silent) {
			fmt.Fprintln(os.Stderr, err)
		}
		cancel()
		os.Exit(1)
	}
	cancel() // Cleanup on successful exit
}
