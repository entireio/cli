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
	"slices"
	"strings"
	"sync"
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

// infoTimeout caps how long a single external agent binary may take to answer
// its "info" subcommand.
//
// The budget is per binary, not per scan: binaries are loaded concurrently, so a
// whole scan costs roughly one infoTimeout no matter how many plugins are
// installed. That is the point of the concurrency — before it, one stalled
// plugin consumed a shared budget and every binary after it was dropped.
//
// The value looks enormous for a call that returns static metadata (a healthy,
// warm plugin answers in ~10ms) because it is sized for the worst legitimate
// case, not the typical one: a binary's FIRST execution on a loaded machine.
// Measured on macOS, forking a freshly written executable costs 320-394ms
// against 7-12ms once the same file has been run before, because a newly
// installed binary pays code signing/Gatekeeper validation and cold page-in on
// first use — and on a saturated machine (a full `-race` test suite) that same
// cold fork was observed to exceed two seconds. Node- or Python-wrapped plugins
// start slower still.
//
// Undersizing this is not a benign performance choice: it turns "the user just
// installed a plugin" into a broken-agent entry on the next command, which then
// works forever after — the worst kind of intermittent. Since discovery is
// concurrent and a healthy plugin never approaches the budget, a generous value
// costs nothing in the common case and only bounds a genuinely hung binary.
//
// The value deliberately matches the total budget the serial implementation used
// to allow for the whole scan, so the worst case a user can wait is unchanged
// while the typical case collapses from N budgets to one. Picking anything
// smaller would make discovery stricter than what already shipped: at 5s, a cold
// exec on a machine running a full parallel test suite was still being killed
// mid-flight.
//
// It is a var so tests can tighten it (to exercise the timeout path quickly) or
// relax it (so a happy-path assertion doesn't depend on machine load).
var infoTimeout = 10 * time.Second //nolint:gochecknoglobals // tunable policy knob; test seam

var (
	// ErrInfoTimeout marks a binary whose "info" call exceeded infoTimeout.
	// It exists because exec reports a context-killed child only as
	// "signal: killed", which is indistinguishable from a crash; callers need to
	// tell "too slow" from "broken" without matching on strings.
	ErrInfoTimeout = errors.New("external agent info command timed out")

	// ErrNotExecutable marks a matching file that is missing its execute bit —
	// the forgot-chmod case, which used to fail invisibly.
	ErrNotExecutable = errors.New("external agent binary is not executable")
)

// lookPathExternalAgent is a narrow test seam for named lookup.
var lookPathExternalAgent = exec.LookPath //nolint:gochecknoglobals // narrow test seam

// BrokenAgent is an external agent binary that exists on $PATH but could not be
// loaded. Keeping these lets the CLI report "installed but unusable" instead of
// behaving as though the plugin were never installed.
type BrokenAgent struct {
	Name       types.AgentName
	BinaryPath string
	Err        error
}

// Discovery results live in two registries. Ready agents are additionally
// registered in the agent package, which must keep meaning "agents you can
// actually use" — so broken entries are recorded here only.
var (
	discoveryMu  sync.RWMutex                            //nolint:gochecknoglobals // process-wide discovery result
	readyAgents  = make(map[types.AgentName]agent.Agent) //nolint:gochecknoglobals // process-wide discovery result
	brokenAgents = make(map[types.AgentName]BrokenAgent) //nolint:gochecknoglobals // process-wide discovery result
)

// candidate is a binary that matched the external agent naming pattern and
// passed the cheap filesystem checks — i.e. one worth executing.
type candidate struct {
	name    types.AgentName
	binPath string
}

// DiscoverAndRegister scans $PATH for executables matching "entire-agent-<name>",
// calls their "info" subcommand concurrently, and registers the ones that answer.
// Binaries that fail are recorded (see BrokenAgents) rather than aborting the
// scan. Discovery is skipped when the external_agents setting is not enabled.
func DiscoverAndRegister(ctx context.Context) {
	if !settings.IsExternalAgentsEnabled(ctx) {
		logging.Debug(ctx, "external agent discovery disabled (external_agents not enabled in settings)")
		return
	}
	discoverAndRegister(ctx)
}

// DiscoverAndRegisterAlways is like DiscoverAndRegister but bypasses the
// external_agents settings check. Use this in interactive setup flows where the
// user explicitly chooses agents and the setting may not exist yet.
func DiscoverAndRegisterAlways(ctx context.Context) {
	discoverAndRegister(ctx)
}

// DiscoverAndRegisterNamedAlways discovers and registers only the external agent
// binary matching name. It bypasses the external_agents setting for explicit,
// one-invocation selections without executing unrelated plugins.
//
// A binary that isn't installed is not an error: callers rely on a nil return to
// mean "no such plugin, fall through to other resolution".
func DiscoverAndRegisterNamedAlways(ctx context.Context, name types.AgentName) error {
	if name == "" {
		return nil
	}
	// Checked before lookup so a traversal attempt never reaches exec.LookPath.
	if strings.ContainsAny(string(name), `/\`) {
		return fmt.Errorf("invalid external agent name %q: contains path separators", name)
	}
	if _, err := agent.Get(name); err == nil {
		return nil
	}

	binName := binaryPrefix + string(name)
	binPath, err := lookPathExternalAgent(binName)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("looking up external agent %q binary %q: %w", name, binName, err)
	}

	cand := candidate{name: name, binPath: binPath}
	usable, err := inspectBinary(binPath)
	if err != nil {
		recordBroken(ctx, cand, err)
		return err
	}
	if !usable {
		return nil
	}

	// A dead caller is reported as cancellation, not as a plugin failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		err := fmt.Errorf("discovering external agent %q: %w", name, ctxErr)
		recordBroken(ctx, cand, err)
		return err
	}

	ag, err := loadExternalAgent(ctx, cand)
	if err != nil {
		recordBroken(ctx, cand, err)
		return err
	}
	recordReady(ctx, cand, ag)
	return nil
}

// discoverAndRegister is the shared scan: a cheap $PATH pass to collect
// candidates, then one goroutine per candidate to load it.
func discoverAndRegister(ctx context.Context) {
	candidates := scanPath(ctx)
	if len(candidates) == 0 {
		return
	}

	// A cancelled caller is not evidence that any plugin is broken, but the set
	// found on $PATH is still worth surfacing, so record it rather than dropping
	// it silently. Nothing is executed.
	if ctxErr := ctx.Err(); ctxErr != nil {
		logging.Debug(ctx, "external agent discovery canceled before loading candidates",
			slog.Int("candidates", len(candidates)),
			slog.String("error", ctxErr.Error()))
		for _, cand := range candidates {
			recordBroken(ctx, cand, fmt.Errorf("discovering external agent %q: %w", cand.name, ctxErr))
		}
		return
	}

	results := make(chan loadResult, len(candidates))
	var wg sync.WaitGroup
	for i, cand := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ag, err := loadExternalAgent(ctx, cand)
			results <- loadResult{index: i, agent: ag, err: err}
		}()
	}
	wg.Wait()
	close(results)

	// Apply in $PATH order rather than completion order, so registration stays
	// deterministic even though loading is concurrent.
	ordered := make([]loadResult, len(candidates))
	for res := range results {
		ordered[res.index] = res
	}
	for i, res := range ordered {
		if res.err != nil {
			recordBroken(ctx, candidates[i], res.err)
			continue
		}
		recordReady(ctx, candidates[i], res.agent)
	}
}

// loadResult carries one candidate's outcome back to the collector. index is the
// candidate's position in $PATH order.
type loadResult struct {
	index int
	agent agent.Agent
	err   error
}

// scanPath walks $PATH and returns the binaries worth executing, in $PATH order.
// Everything that can be decided without running anything is decided here:
// non-agents are dropped, unusable files are recorded as broken.
func scanPath(ctx context.Context) []candidate {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return nil
	}

	registered := make(map[types.AgentName]bool)
	for _, name := range agent.List() {
		registered[name] = true
	}

	var candidates []candidate
	seen := make(map[types.AgentName]bool) // first $PATH dir wins
	for _, dir := range filepath.SplitList(pathEnv) {
		matches, err := filepath.Glob(filepath.Join(dir, binaryPrefix+"*"))
		if err != nil {
			continue // unreadable directory
		}
		for _, binPath := range matches {
			// Strip Windows executable extensions (.exe, .bat, …) before deriving
			// the agent name. On Unix this is a no-op.
			base := StripExeExt(filepath.Base(binPath))
			name := types.AgentName(strings.TrimPrefix(base, binaryPrefix))
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true

			// Already resolved in this process: don't pay for the exec again.
			// setup.go reaches discovery from several independent entry points,
			// so the redundant calls must cost a glob and nothing more.
			if isKnown(name) {
				continue
			}

			// A registered agent always wins a name collision, and the shadowing
			// binary is not executed. agent.Register overwrites process-wide and
			// the gated scan runs inside git hooks, so allowing an override would
			// let any writable $PATH directory take over the agent that reads
			// transcripts and writes checkpoints.
			if registered[name] {
				logging.Warn(ctx, "ignoring external agent binary that shadows a registered agent",
					slog.String("binary", binPath),
					slog.String("agent", string(name)))
				continue
			}

			cand := candidate{name: name, binPath: binPath}
			usable, err := inspectBinary(binPath)
			if err != nil {
				recordBroken(ctx, cand, err)
				continue
			}
			if !usable {
				continue
			}
			candidates = append(candidates, cand)
		}
	}
	return candidates
}

// inspectBinary reports whether binPath is worth executing. It returns
// (false, nil) for entries that aren't plugins at all — a directory, or a file
// that vanished between glob and stat — and an error for a file that looks like
// a plugin but cannot be run.
func inspectBinary(binPath string) (bool, error) {
	// binPath is not user input: it comes from a $PATH glob or exec.LookPath, and
	// a name carrying a path separator is rejected before lookup.
	finfo, err := os.Stat(binPath) //nolint:gosec // G703: path comes from $PATH, not from user input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspecting external agent binary %q: %w", binPath, err)
	}
	if finfo.IsDir() {
		return false, nil
	}
	// Windows doesn't set execute bits; executability there comes from PATHEXT.
	if runtime.GOOS != osWindows && finfo.Mode()&0o111 == 0 {
		return false, fmt.Errorf("%w: %q", ErrNotExecutable, binPath)
	}
	return true, nil
}

// loadExternalAgent runs the binary's "info" subcommand under the per-binary
// budget and wraps the result. It never touches the registries, so callers
// decide whether a failure is returned, recorded, or both.
func loadExternalAgent(ctx context.Context, cand candidate) (agent.Agent, error) {
	// ctx stays the CALLER's; the budget gets its own name. Shadowing ctx here
	// would hide caller cancellation behind the derived deadline.
	runCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	ea, err := New(runCtx, cand.binPath)
	if err != nil {
		return nil, fmt.Errorf("loading info for external agent %q from binary %q: %w",
			cand.name, cand.binPath, classifyLoadError(ctx, runCtx, err))
	}

	wrapped, err := Wrap(ea)
	if err != nil {
		return nil, fmt.Errorf("wrapping external agent %q from binary %q: %w", cand.name, cand.binPath, err)
	}
	return wrapped, nil
}

// classifyLoadError labels a failed "info" call so callers can tell a timeout
// from a genuine failure; exec surfaces a killed child only as "signal: killed".
//
// ErrInfoTimeout is attached only when the budget expired on its own: if the
// caller was cancelled the binary never got its full budget, so calling it slow
// would be a lie.
func classifyLoadError(caller, runCtx context.Context, err error) error {
	runErr := runCtx.Err()
	if runErr == nil {
		return err
	}
	if caller.Err() != nil {
		return errors.Join(runErr, err)
	}
	return errors.Join(ErrInfoTimeout, runErr, err)
}

// isKnown reports whether discovery already has a verdict for name.
func isKnown(name types.AgentName) bool {
	discoveryMu.RLock()
	defer discoveryMu.RUnlock()
	_, ready := readyAgents[name]
	_, broken := brokenAgents[name]
	return ready || broken
}

func recordReady(ctx context.Context, cand candidate, ag agent.Agent) {
	discoveryMu.Lock()
	readyAgents[cand.name] = ag
	delete(brokenAgents, cand.name)
	discoveryMu.Unlock()

	agent.Register(cand.name, func() agent.Agent { return ag })

	logging.Debug(ctx, "registered external agent",
		slog.String("name", string(cand.name)),
		slog.String("type", string(ag.Type())),
		slog.String("binary", cand.binPath))
}

func recordBroken(ctx context.Context, cand candidate, err error) {
	discoveryMu.Lock()
	brokenAgents[cand.name] = BrokenAgent{Name: cand.name, BinaryPath: cand.binPath, Err: err}
	discoveryMu.Unlock()

	logging.Debug(ctx, "external agent binary could not be loaded",
		slog.String("name", string(cand.name)),
		slog.String("binary", cand.binPath),
		slog.String("error", err.Error()))
}

// Get returns a discovered external agent by name. A binary that was found but
// failed to load returns its load error, so callers can explain why a plugin the
// user has installed isn't available.
func Get(name types.AgentName) (agent.Agent, error) {
	discoveryMu.RLock()
	defer discoveryMu.RUnlock()

	if ag, ok := readyAgents[name]; ok {
		return ag, nil
	}
	if b, ok := brokenAgents[name]; ok {
		return nil, b.Err
	}
	return nil, fmt.Errorf("no external agent %q discovered", name)
}

// ReadyAgents returns the names of external agents that loaded successfully,
// sorted.
func ReadyAgents() []types.AgentName {
	discoveryMu.RLock()
	defer discoveryMu.RUnlock()

	names := make([]types.AgentName, 0, len(readyAgents))
	for name := range readyAgents {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// BrokenAgents returns the external agent binaries that were found on $PATH but
// could not be loaded, sorted by name.
func BrokenAgents() []BrokenAgent {
	discoveryMu.RLock()
	defer discoveryMu.RUnlock()

	broken := make([]BrokenAgent, 0, len(brokenAgents))
	for _, b := range brokenAgents {
		broken = append(broken, b)
	}
	slices.SortFunc(broken, func(a, b BrokenAgent) int {
		return strings.Compare(string(a.Name), string(b.Name))
	})
	return broken
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
