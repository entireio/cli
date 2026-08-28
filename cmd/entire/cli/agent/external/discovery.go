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

// infoTimeout bounds a single `info` call. Every candidate binary gets its own
// budget and they all run concurrently, so a scan costs roughly one budget
// rather than one per binary.
//
// Ten seconds covers cold starts even on a saturated machine. This matches the
// old aggregate scan budget, but each binary gets the budget concurrently, so
// several stalled plugins still cost roughly ten seconds rather than N times
// ten seconds. A var keeps timeout-path tests fast and deterministic.
var infoTimeout = 10 * time.Second //nolint:gochecknoglobals // policy knob overridden by timeout tests

// Errors that classify why a discovered binary is unusable. exec reports only
// "signal: killed" when a context kills the child, so without these a budget
// that is too tight is indistinguishable from a plugin that is actually broken.
var (
	// ErrInfoTimeout means the binary did not answer `info` within infoTimeout.
	ErrInfoTimeout = errors.New("external agent info call timed out")
	// ErrNotExecutable means a matching regular file has no executable bit (Unix).
	ErrNotExecutable = errors.New("external agent binary is not executable")
)

//nolint:gochecknoglobals // narrow test seam for lookup failures
var lookPathExternalAgent = exec.LookPath

// candidate is a binary that looks like an external agent and has not been
// ruled out without executing it.
type candidate struct {
	name types.AgentName
	path string
}

// probeResult pairs a candidate with the outcome of asking it for `info`.
type probeResult struct {
	candidate

	ag  agent.Agent
	err error
}

// DiscoverAndRegister scans $PATH for executables matching "entire-agent-<name>",
// calls their "info" subcommand, and registers them in the agent registry.
// Binaries that cannot be loaded are registered as failures so `entire agent
// list` can explain them. Binaries whose name conflicts with an already-registered
// agent are skipped without being executed.
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
	return discoverAndRegisterNamed(ctx, name)
}

// discoverAndRegister runs the three discovery phases: collect candidates from
// $PATH (filesystem only), probe them all concurrently, then register the
// results in collect order.
func discoverAndRegister(ctx context.Context) {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return
	}

	candidates := collectCandidates(ctx, pathEnv)
	if len(candidates) == 0 {
		return
	}

	// A caller whose context is already done is not evidence that a plugin is
	// broken, but the found-on-$PATH set should still be visible rather than
	// silently dropped. Record it and execute nothing.
	if err := ctx.Err(); err != nil {
		for _, c := range candidates {
			agent.RegisterExternalFailure(c.name, c.path,
				fmt.Errorf("discovering external agent %q from binary %q: %w", c.name, c.path, err))
		}
		logging.Debug(ctx, "skipped probing external agents (caller context done)",
			slog.Int("candidates", len(candidates)),
			slog.String("error", err.Error()))
		return
	}

	for _, result := range probeCandidates(ctx, candidates) {
		applyProbeResult(ctx, result)
	}
}

// collectCandidates walks $PATH and returns the binaries worth executing, in
// $PATH order. It executes nothing; binaries ruled out here (a directory, no
// exec bit, an unreadable file) are registered as failures on the spot.
func collectCandidates(ctx context.Context, pathEnv string) []candidate {
	usable := make(map[types.AgentName]bool)
	for _, name := range agent.List() {
		usable[name] = true
	}
	probed := agent.ProbedExternalBinaries()

	seen := make(map[types.AgentName]bool) // first runnable $PATH entry wins
	type staticFailure struct {
		path string
		err  error
	}
	staticFailures := make(map[types.AgentName]staticFailure)
	var candidates []candidate

	for _, dir := range filepath.SplitList(pathEnv) {
		matches, err := filepath.Glob(filepath.Join(dir, binaryPrefix+"*"))
		if err != nil {
			continue // skip unreadable directories
		}
		for _, binPath := range matches {
			// Dedupe on the derived agent name, not the file name: on Windows
			// entire-agent-foo.exe and entire-agent-foo.com both derive agent
			// "foo", and PATHEXT decides which one actually runs.
			name := derivedAgentName(binPath)
			if name == "" || seen[name] {
				continue
			}

			// Already probed this exact binary in this process, ready or failed.
			if _, ok := probed[binPath]; ok {
				seen[name] = true
				continue
			}

			if usable[name] {
				seen[name] = true
				// A built-in always wins, and the colliding binary is never
				// executed. The gated DiscoverAndRegister runs inside the hook
				// trees, so an override would let a binary dropped anywhere on
				// $PATH take over transcript reads and checkpoint writes on
				// every hook. Warn (not Debug) so the attempt is visible.
				logging.Warn(ctx, "ignoring external agent that shadows an existing agent",
					slog.String("binary", binPath),
					slog.String("agent", string(name)))
				continue
			}

			ok, err := inspectBinary(name, binPath)
			if err != nil {
				if _, exists := staticFailures[name]; !exists {
					staticFailures[name] = staticFailure{path: binPath, err: err}
				}
				continue
			}
			if !ok {
				continue
			}
			seen[name] = true
			delete(staticFailures, name)
			candidates = append(candidates, candidate{name: name, path: binPath})
		}
	}
	for name, failure := range staticFailures {
		if !seen[name] {
			agent.RegisterExternalFailure(name, failure.path, failure.err)
		}
	}
	return candidates
}

// derivedAgentName maps a binary path to the registry name it would claim.
func derivedAgentName(binPath string) types.AgentName {
	base := StripExeExt(filepath.Base(binPath))
	return types.AgentName(strings.TrimPrefix(base, binaryPrefix))
}

// inspectBinary reports whether binPath is worth executing. It returns
// (false, nil) when the path has gone away — a binary that vanished between the
// glob and the stat is not a broken plugin — and (false, err) when the file is
// there but cannot be an agent.
func inspectBinary(name types.AgentName, binPath string) (bool, error) {
	// G703: binPath comes from a $PATH glob or exec.LookPath, both of which are
	// process inputs we already trust to name executables.
	finfo, err := os.Stat(binPath) //nolint:gosec // path is a $PATH-resolved binary, not user input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspecting external agent %q binary %q: %w", name, binPath, err)
	}
	if finfo.IsDir() {
		return false, fmt.Errorf("external agent %q path %q is a directory, not an executable", name, binPath)
	}
	// Windows does not set execute bits.
	if runtime.GOOS != osWindows && finfo.Mode()&0o111 == 0 {
		return false, fmt.Errorf("external agent %q binary %q: %w", name, binPath, ErrNotExecutable)
	}
	return true, nil
}

// probeCandidates asks every candidate for its `info` concurrently and returns
// the results in candidate order, so registration stays deterministic even
// though completion order is not.
//
// No worker pool: the realistic candidate count is a handful, and a cap would
// serialize exactly the case this concurrency exists for.
func probeCandidates(ctx context.Context, candidates []candidate) []probeResult {
	results := make(chan probeResult, len(candidates))
	for _, c := range candidates {
		go func() {
			ag, err := loadExternalAgent(ctx, c)
			results <- probeResult{candidate: c, ag: ag, err: err}
		}()
	}

	byPath := make(map[string]probeResult, len(candidates))
	for range candidates {
		r := <-results
		byPath[r.path] = r
	}

	ordered := make([]probeResult, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, byPath[c.path])
	}
	return ordered
}

// loadExternalAgent runs `info` on one binary under its own budget and wraps
// the result. It never touches the registry, so it is safe to run concurrently.
func loadExternalAgent(ctx context.Context, c candidate) (agent.Agent, error) {
	// Derived from the caller, so a tighter caller deadline still wins.
	probeCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	ea, err := New(probeCtx, c.path)
	if err != nil {
		return nil, classifyLoadError(ctx, probeCtx, c, err)
	}
	wrapped, err := Wrap(ea)
	if err != nil {
		return nil, fmt.Errorf("wrapping external agent %q from binary %q: %w", c.name, c.path, err)
	}
	return wrapped, nil
}

// classifyLoadError labels a failed `info` call so the caller can tell a budget
// breach from a genuinely broken plugin.
func classifyLoadError(caller, probe context.Context, c candidate, err error) error {
	wrapped := fmt.Errorf("loading info for external agent %q from binary %q: %w", c.name, c.path, err)
	if callerErr := caller.Err(); callerErr != nil {
		return fmt.Errorf("%w: %w", callerErr, wrapped)
	}
	if probeErr := probe.Err(); probeErr != nil {
		return fmt.Errorf("%w after %s (%w): %w", ErrInfoTimeout, infoTimeout, probeErr, wrapped)
	}
	return wrapped
}

// applyProbeResult records one probe outcome in the registry. Called only from
// the goroutine that owns discovery, so registration order is deterministic.
func applyProbeResult(ctx context.Context, r probeResult) {
	if r.err != nil {
		agent.RegisterExternalFailure(r.name, r.path, r.err)
		logging.Debug(ctx, "external agent found but not loadable",
			slog.String("binary", r.path),
			slog.String("agent", string(r.name)),
			slog.String("error", r.err.Error()))
		return
	}

	agent.RegisterExternal(r.name, r.path, func() agent.Agent { return r.ag })

	// The registry key comes from the file name, and every caller resolves
	// agents by that key, so a binary reporting a different name still works.
	// Say so rather than leaving the inconsistency to surface in logs later.
	if declared := r.ag.Name(); declared != r.name {
		logging.Warn(ctx, "external agent reports a name that differs from its binary",
			slog.String("binary", r.path),
			slog.String("registered", string(r.name)),
			slog.String("declared", string(declared)))
	}

	logging.Debug(ctx, "registered external agent",
		slog.String("name", string(r.name)),
		slog.String("type", string(r.ag.Type())),
		slog.String("binary", r.path))
}

// discoverAndRegisterNamed resolves one named binary through $PATH instead of
// globbing. Unlike the scan, it surfaces the failure to its caller as well as
// recording it, because the caller named this agent explicitly.
func discoverAndRegisterNamed(ctx context.Context, name types.AgentName) error {
	if name == "" {
		return nil
	}
	if strings.ContainsAny(string(name), `/\`) {
		return fmt.Errorf("invalid external agent name %q: contains path separators", name)
	}
	if _, err := agent.Get(name); err == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("discovering external agent %q: %w", name, err)
	}

	binName := binaryPrefix + string(name)
	binPath, err := lookPathExternalAgent(binName)
	if err != nil {
		// A missing binary means "no such plugin, fall through to other
		// resolution" — not a broken agent. Callers depend on the nil.
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("looking up external agent %q binary %q: %w", name, binName, err)
	}
	if failure, ok := agent.ExternalFailureFor(name); ok && failure.Binary == binPath {
		return failure.Err
	}

	c := candidate{name: name, path: binPath}
	ok, err := inspectBinary(name, binPath)
	if err != nil {
		agent.RegisterExternalFailure(name, binPath, err)
		return err
	}
	if !ok {
		return nil
	}

	ag, err := loadExternalAgent(ctx, c)
	if err != nil {
		agent.RegisterExternalFailure(name, binPath, err)
		return err
	}
	applyProbeResult(ctx, probeResult{candidate: c, ag: ag})
	return nil
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
