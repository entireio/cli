package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const (
	binaryPrefix = "entire-agent-"
	osWindows    = "windows"
)

// discoveryTimeout caps the total time spent scanning $PATH for external agents.
const discoveryTimeout = 10 * time.Second

var (
	statExternalAgent     = os.Stat       //nolint:gochecknoglobals // narrow test seam for stat failures
	lookPathExternalAgent = exec.LookPath //nolint:gochecknoglobals // narrow test seam for lookup failures
)

// DiscoverAndRegister scans $PATH for executables matching "entire-agent-<name>",
// calls their "info" subcommand, and registers them in the agent registry.
// Binaries whose name conflicts with an already-registered agent are skipped.
// Errors during discovery are logged but do not prevent other agents from loading.
// Discovery is skipped when the external_agents setting is not enabled.
func DiscoverAndRegister(ctx context.Context) {
	if !settings.IsExternalAgentsEnabled(ctx) {
		logging.Debug(ctx, "external agent discovery disabled (external_agents not enabled in settings)")
		return
	}
	discoverAndRegister(ctx)
}

// DiscoverAndRegisterAlways is like DiscoverAndRegister but bypasses the
// external_agents settings check. Use this in interactive setup flows where
// the user explicitly chooses agents.
func DiscoverAndRegisterAlways(ctx context.Context) {
	discoverAndRegister(ctx)
}

// DiscoverAndRegisterNamedAlways discovers and registers only the external
// agent binary matching name. It bypasses the external_agents setting for
// explicit, one-invocation selections without executing unrelated plugins.
func DiscoverAndRegisterNamedAlways(ctx context.Context, name types.AgentName) error {
	return discoverAndRegisterNamed(ctx, name, discoveryTimeout)
}

func discoverAndRegisterNamed(ctx context.Context, name types.AgentName, timeout time.Duration) error {
	if name == "" {
		return nil
	}
	if strings.ContainsAny(string(name), `/\`) {
		return fmt.Errorf("invalid external agent name %q: contains path separators", name)
	}
	if _, err := agent.Get(name); err == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("discovering external agent %q: %w", name, err)
	}

	binName := binaryPrefix + string(name)
	binPath, err := lookPathExternalAgent(binName)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("looking up external agent %q binary %q: %w", name, binName, ctxErr)
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("looking up external agent %q binary %q: %w", name, binName, err)
	}
	registered, err := registerExternalAgent(ctx, binPath, name)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("external agent %q binary %q was found but could not be registered", name, binPath)
	}
	return nil
}

// discoverAndRegister contains the shared scanning logic for external agent discovery.
func discoverAndRegister(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

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
		if ctx.Err() != nil {
			logging.Debug(ctx, "external agent discovery timed out")
			return
		}

		matches, err := filepath.Glob(filepath.Join(dir, binaryPrefix+"*"))
		if err != nil {
			continue // skip unreadable directories
		}
		for _, binPath := range matches {
			if ctx.Err() != nil {
				logging.Debug(ctx, "external agent discovery timed out")
				return
			}

			name := filepath.Base(binPath)
			if seen[name] {
				continue
			}
			seen[name] = true

			// Strip Windows executable extensions (.exe, .bat) before deriving agent name.
			// On Unix, binaries have no extension, so this is a no-op.
			cleanName := StripExeExt(name)
			agentName := types.AgentName(strings.TrimPrefix(cleanName, binaryPrefix))
			if registered[agentName] {
				logging.Debug(ctx, "skipping external agent (name conflict with built-in)",
					slog.String("binary", name),
					slog.String("agent", string(agentName)))
				continue
			}

			registeredAgent, err := registerExternalAgent(ctx, binPath, agentName)
			if err != nil {
				logging.Debug(ctx, "skipping external agent (registration failed)",
					slog.String("binary", binPath),
					slog.String("agent", string(agentName)),
					slog.String("error", err.Error()))
				continue
			}
			if registeredAgent {
				registered[agentName] = true
			}
		}
	}
}

func registerExternalAgent(ctx context.Context, binPath string, name types.AgentName) (bool, error) {
	finfo, err := statExternalAgent(binPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspecting external agent %q binary %q: %w", name, binPath, err)
	}
	if finfo.IsDir() {
		return false, nil
	}
	// Check executable bit (on Unix; Windows doesn't set execute bits).
	if runtime.GOOS != osWindows && finfo.Mode()&0o111 == 0 {
		return false, nil
	}

	ea, err := New(ctx, binPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("loading info for external agent %q from binary %q: %w: %w", name, binPath, ctxErr, err)
		}
		return false, fmt.Errorf("loading info for external agent %q from binary %q: %w", name, binPath, err)
	}

	wrapped, err := Wrap(ea)
	if err != nil {
		return false, fmt.Errorf("wrapping external agent %q from binary %q: %w", name, binPath, err)
	}
	agent.Register(name, func() agent.Agent {
		return wrapped
	})

	logging.Debug(ctx, "registered external agent",
		slog.String("name", string(name)),
		slog.String("type", string(ea.Type())),
		slog.String("binary", binPath))
	return true, nil
}

// StripExeExt removes Windows executable extensions (.exe, .bat, .cmd, .com)
// from a file name so that the derived name matches on all platforms. On Unix
// this is effectively a no-op because binaries have no extension.
//
// .com is included because Windows PATHEXT defaults to ".COM;.EXE;.BAT;.CMD;…",
// so exec.LookPath can resolve a `.com` next to a `.exe`. Without stripping
// it, a managed-plugin or agent-binary installer would treat foo.exe and
// foo.com as distinct names while PATHEXT silently picks one.
func StripExeExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".bat", ".cmd", ".com":
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name
}
