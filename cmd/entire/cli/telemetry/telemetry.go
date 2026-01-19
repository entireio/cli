package telemetry

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/posthog/posthog-go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	// PostHogAPIKey is set at build time for production
	PostHogAPIKey = "phc_development_key"
	// PostHogEndpoint is set at build time for production
	PostHogEndpoint = "https://eu.i.posthog.com"
)

// Client defines the telemetry interface
type Client interface {
	TrackCommand(cmd *cobra.Command)
	Close()
}

// contextKey is used for storing the telemetry client in context
type contextKey struct{}

// WithClient returns a new context with the telemetry client attached
func WithClient(ctx context.Context, client Client) context.Context {
	return context.WithValue(ctx, contextKey{}, client)
}

// GetClient retrieves the telemetry client from context
//
//nolint:ireturn // Returns interface by design - retrieves from context
func GetClient(ctx context.Context) Client {
	if client, ok := ctx.Value(contextKey{}).(Client); ok {
		return client
	}
	return &NoOpClient{}
}

// NoOpClient is a no-op implementation for when telemetry is disabled
type NoOpClient struct{}

func (n *NoOpClient) TrackCommand(_ *cobra.Command) {}
func (n *NoOpClient) Close()                        {}

// silentLogger suppresses PostHog log output - expected for CLI best-effort telemetry
type silentLogger struct{}

func (silentLogger) Logf(_ string, _ ...interface{})   {}
func (silentLogger) Debugf(_ string, _ ...interface{}) {}
func (silentLogger) Warnf(_ string, _ ...interface{})  {}
func (silentLogger) Errorf(_ string, _ ...interface{}) {}

// PostHogClient is the real telemetry client
type PostHogClient struct {
	client     posthog.Client
	machineID  string
	cliVersion string
	mu         sync.RWMutex
}

// NewClient creates a new telemetry client based on opt-out settings.
// The telemetryEnabled parameter comes from settings; nil means not configured (default to disabled).
//
//nolint:ireturn // Factory function - returns NoOpClient or PostHogClient based on settings
func NewClient(version string, telemetryEnabled *bool) Client {
	// Environment variable takes priority
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return &NoOpClient{}
	}

	// Check settings preference (nil = not set, default to disabled)
	if telemetryEnabled == nil || !*telemetryEnabled {
		return &NoOpClient{}
	}

	id, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return &NoOpClient{}
	}

	// Use a fast-timeout HTTP transport for telemetry - we don't want to block CLI exit
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 100 * time.Millisecond,
		}).DialContext,
		TLSHandshakeTimeout:   100 * time.Millisecond,
		ResponseHeaderTimeout: 100 * time.Millisecond,
	}

	client, err := posthog.NewWithConfig(PostHogAPIKey, posthog.Config{
		Endpoint:           PostHogEndpoint,
		ShutdownTimeout:    100 * time.Millisecond, // Don't block CLI exit waiting for telemetry
		BatchUploadTimeout: 200 * time.Millisecond, // Fast timeout - telemetry is best-effort
		Transport:          transport,
		Logger:             silentLogger{}, // Suppress warnings on timeout (expected)
		DisableGeoIP:       posthog.Ptr(true),
		DefaultEventProperties: posthog.NewProperties().
			Set("cli_version", version).
			Set("os", runtime.GOOS).
			Set("arch", runtime.GOARCH),
	})
	if err != nil {
		return &NoOpClient{}
	}

	return &PostHogClient{
		client:     client,
		machineID:  id,
		cliVersion: version,
	}
}

// TrackCommand records the command execution
func (p *PostHogClient) TrackCommand(cmd *cobra.Command) {
	if cmd == nil {
		return
	}

	// Skip hidden commands
	if cmd.Hidden {
		return
	}

	p.mu.RLock()
	id := p.machineID
	c := p.client
	p.mu.RUnlock()

	if c == nil {
		return
	}

	// Collect flag names (not values) for privacy
	var flags []string
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		flags = append(flags, flag.Name)
	})

	props := posthog.NewProperties().
		Set("command", cmd.CommandPath())

	if len(flags) > 0 {
		props.Set("flags", strings.Join(flags, ","))
	}

	//nolint:errcheck // Best-effort telemetry, failures should not affect CLI
	_ = c.Enqueue(posthog.Capture{
		DistinctId: id,
		Event:      "cli_command_executed",
		Properties: props,
	})
}

// Close flushes pending events
func (p *PostHogClient) Close() {
	p.mu.RLock()
	c := p.client
	p.mu.RUnlock()

	if c != nil {
		_ = c.Close()
	}
}
