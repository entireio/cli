package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"entire.io/cli/cmd/entire/cli"
	"entire.io/cli/cmd/entire/cli/telemetry"
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

	// Load telemetry preference from settings (ignore errors - nil defaults to disabled)
	var telemetryEnabled *bool
	if settings, err := cli.LoadEntireSettings(); err == nil {
		telemetryEnabled = settings.Telemetry
	}

	// Initialize telemetry client and add to context
	telemetryClient := telemetry.NewClient(cli.Version, telemetryEnabled)
	ctx = telemetry.WithClient(ctx, telemetryClient)
	defer telemetryClient.Close()

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
