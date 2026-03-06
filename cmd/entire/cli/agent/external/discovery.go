package external

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const binaryPrefix = "entire-agent-"

// DiscoverAndRegister scans $PATH for executables matching "entire-agent-<name>",
// calls their "info" subcommand, and registers them in the agent registry.
// Binaries whose name conflicts with an already-registered agent are skipped.
// Errors during discovery are logged but do not prevent other agents from loading.
func DiscoverAndRegister(ctx context.Context) {
	if !settings.IsExternalAgentsEnabled(ctx) {
		logging.Debug(ctx, "external agent discovery disabled (external_agents not enabled in settings)")
		return
	}

	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return
	}

	// Collect already-registered names to avoid conflicts.
	registered := make(map[types.AgentName]bool)
	for _, name := range agent.List() {
		registered[name] = true
	}

	seen := make(map[string]bool) // deduplicate binaries across PATH dirs
	for _, dir := range filepath.SplitList(pathEnv) {
		matches, err := filepath.Glob(filepath.Join(dir, binaryPrefix+"*"))
		if err != nil {
			continue // skip unreadable directories
		}
		for _, binPath := range matches {
			name := filepath.Base(binPath)
			if seen[name] {
				continue
			}
			seen[name] = true

			agentName := types.AgentName(strings.TrimPrefix(name, binaryPrefix))
			if registered[agentName] {
				logging.Debug(ctx, "skipping external agent (name conflict with built-in)",
					slog.String("binary", name),
					slog.String("agent", string(agentName)))
				continue
			}

			finfo, err := os.Stat(binPath) //nolint:gosec // PATH entries are trusted
			if err != nil || finfo.IsDir() {
				continue
			}
			// Check executable bit (on Unix)
			if finfo.Mode()&0o111 == 0 {
				continue
			}

			ea, err := New(ctx, binPath)
			if err != nil {
				logging.Debug(ctx, "skipping external agent (info failed)",
					slog.String("binary", binPath),
					slog.String("error", err.Error()))
				continue
			}

			// Wrap with capability interfaces and register
			wrapped, err := Wrap(ea)
			if err != nil {
				logging.Debug(ctx, "skipping external agent (wrap failed)",
					slog.String("binary", binPath),
					slog.String("error", err.Error()))
				continue
			}
			agent.Register(agentName, func() agent.Agent {
				return wrapped
			})
			registered[agentName] = true

			logging.Debug(ctx, "registered external agent",
				slog.String("name", string(agentName)),
				slog.String("type", string(ea.Type())),
				slog.String("binary", binPath))
		}
	}
}
