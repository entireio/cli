package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var (
	registryMu sync.RWMutex
	registry   = make(map[types.AgentName]entry)
)

// Factory creates a new agent instance
type Factory func() Agent

// entry is one slot in the registry. Exactly one of factory and err is set: a
// usable agent has a factory, while an external binary that was found on $PATH
// but could not be loaded carries the reason instead. binary is set for
// external agents only, and is what lets discovery skip a binary it has
// already asked for info.
type entry struct {
	factory Factory
	binary  string
	err     error
}

// ExternalFailure is an external agent binary that exists on $PATH but could
// not be loaded. Listing commands use it to answer "why isn't my plugin
// showing up?", which used to be a Debug log line and nothing else.
type ExternalFailure struct {
	Name   types.AgentName
	Binary string
	Err    error
}

// ErrUnknownAgent marks a name that has no registry entry. Callers use it to
// distinguish a typo from an installed external agent that failed to load.
var ErrUnknownAgent = errors.New("unknown agent")

// Register adds an agent factory to the registry.
// Called from init() in each agent implementation.
func Register(name types.AgentName, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = entry{factory: factory}
}

// RegisterExternal adds a usable external agent, remembering which binary it
// came from so discovery can skip that binary on a later pass.
func RegisterExternal(name types.AgentName, binary string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = entry{factory: factory, binary: binary}
}

// RegisterExternalFailure records an external agent binary that was found but
// could not be loaded, so the reason survives until something lists it.
//
// A failure can never displace a working built-in: discovery skips any binary
// whose derived name collides with an already-registered agent, without
// executing it.
func RegisterExternalFailure(name types.AgentName, binary string, err error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = entry{binary: binary, err: err}
}

// Get retrieves an agent by name. An external agent that failed to load
// returns its load error rather than looking like it was never installed.
func Get(name types.AgentName) (Agent, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	e, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s (available: %v)", ErrUnknownAgent, name, usableNamesLocked())
	}
	if e.err != nil {
		return nil, fmt.Errorf("external agent %q (%s) is not usable: %w", name, e.binary, e.err)
	}
	return e.factory(), nil
}

// usableNamesLocked returns the sorted usable agent names. The caller must
// already hold registryMu. List cannot be reused here: re-entering RLock
// deadlocks whenever a writer queues between the two acquisitions.
func usableNamesLocked() []types.AgentName {
	names := make([]types.AgentName, 0, len(registry))
	for name, e := range registry {
		if e.factory == nil {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// List returns all usable agent names in sorted order. An external agent whose
// binary failed to load is not usable and is reported by ExternalFailures
// instead, so every List consumer keeps meaning "agents you can actually use".
func List() []types.AgentName {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return usableNamesLocked()
}

// StringList returns user-facing agent names, excluding test-only agents.
func StringList() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name, e := range registry {
		if e.factory == nil {
			continue
		}
		if to, ok := e.factory().(TestOnly); ok && to.IsTestOnly() {
			continue
		}
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

// ExternalFailures returns every discovered external agent binary that could
// not be loaded, sorted by name. The result is a copy.
func ExternalFailures() []ExternalFailure {
	registryMu.RLock()
	defer registryMu.RUnlock()

	failures := make([]ExternalFailure, 0, len(registry))
	for name, e := range registry {
		if e.err == nil {
			continue
		}
		failures = append(failures, ExternalFailure{Name: name, Binary: e.binary, Err: e.err})
	}
	slices.SortFunc(failures, func(a, b ExternalFailure) int {
		return strings.Compare(string(a.Name), string(b.Name))
	})
	return failures
}

// ExternalFailureFor returns the recorded failure for name, if any.
func ExternalFailureFor(name types.AgentName) (ExternalFailure, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	e, ok := registry[name]
	if !ok || e.err == nil {
		return ExternalFailure{}, false
	}
	return ExternalFailure{Name: name, Binary: e.binary, Err: e.err}, true
}

// ProbedExternalBinaries returns the set of external binary paths already
// probed in this process, loaded or not. Discovery uses it to avoid re-running
// info on a binary it has already asked, which matters because one setup flow
// calls discovery several times.
func ProbedExternalBinaries() map[string]struct{} {
	registryMu.RLock()
	defer registryMu.RUnlock()

	probed := make(map[string]struct{})
	for _, e := range registry {
		if e.binary != "" {
			probed[e.binary] = struct{}{}
		}
	}
	return probed
}

// ResetExternalsForTesting drops every external agent entry, ready or failed.
// Both the registry and the probe memo are process-wide, so without this
// discovery results leak between tests and the memo makes failures depend on
// test order. Built-in entries are left alone.
func ResetExternalsForTesting() {
	registryMu.Lock()
	defer registryMu.Unlock()
	for name, e := range registry {
		if e.binary != "" {
			delete(registry, name)
		}
	}
}

// DetectAll returns all agents whose DetectPresence reports true.
// Agents are checked in sorted name order (via List()) for deterministic results.
// Returns an empty slice when no agent is detected.
func DetectAll(ctx context.Context) []Agent {
	names := List() // sorted, lock-safe

	var detected []Agent
	for _, name := range names {
		ag, err := Get(name)
		if err != nil {
			continue
		}
		if present, err := ag.DetectPresence(ctx); err == nil && present {
			detected = append(detected, ag)
		}
	}
	return detected
}

// AgentForTranscriptPath returns the registered agent whose session directory
// for repoPath contains the given transcript path. Used to disambiguate which
// agent owns a session when multiple agents' hooks fire for the same session
// ID — a Cursor transcript path uniquely identifies a Cursor session even
// when Claude Code's hook is the one firing.
//
// Returns (nil, false) if transcriptPath is empty, no agent claims it, or any
// registry lookup fails. Match is by directory prefix (with a separator) so
// "/x/.claude/projects/abc.jsonl" doesn't accidentally match an agent rooted
// at "/x/.claude/projects/ab".
//
//nolint:revive // AgentForTranscriptPath: stutter is intentional for package callers (agent.AgentForTranscriptPath reads naturally)
func AgentForTranscriptPath(transcriptPath, repoPath string) (Agent, bool) {
	if transcriptPath == "" {
		return nil, false
	}
	abs, err := filepath.Abs(transcriptPath)
	if err != nil {
		abs = transcriptPath
	}
	for _, name := range List() {
		ag, err := Get(name)
		if err != nil {
			continue
		}
		dir, err := ag.GetSessionDir(repoPath)
		if err != nil || dir == "" {
			continue
		}
		dirAbs, err := filepath.Abs(dir)
		if err != nil {
			dirAbs = dir
		}
		if pathHasDirPrefix(abs, dirAbs) {
			return ag, true
		}
	}
	return nil, false
}

// pathHasDirPrefix reports whether path is contained within dir (or equals it).
// Adds a trailing separator before prefix-matching so /a/bc doesn't match /a/b.
//
// On Windows, comparison is case-insensitive: NTFS/ReFS treat paths as
// case-insensitive, and filepath.Abs preserves whatever casing the input had,
// so a transcript path like `C:\Users\Bob\.cursor\...` and a session dir like
// `c:\users\bob\.cursor\...` refer to the same location but would not match
// under a byte-wise comparison.
func pathHasDirPrefix(path, dir string) bool {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		dir = strings.ToLower(dir)
	}
	if path == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(path, dir)
}

// Agent name constants (registry keys)
const (
	AgentNameClaudeCode     types.AgentName = "claude-code"
	AgentNameCodex          types.AgentName = "codex"
	AgentNameCopilotCLI     types.AgentName = "copilot-cli"
	AgentNameCursor         types.AgentName = "cursor"
	AgentNameFactoryAIDroid types.AgentName = "factoryai-droid"
	AgentNameGemini         types.AgentName = "gemini"
	AgentNameOpenCode       types.AgentName = "opencode"
	AgentNamePi             types.AgentName = "pi"
)

// Agent type constants (type identifiers stored in metadata/trailers)
const (
	AgentTypeClaudeCode     types.AgentType = "Claude Code"
	AgentTypeCodex          types.AgentType = "Codex"
	AgentTypeCopilotCLI     types.AgentType = "Copilot CLI"
	AgentTypeCursor         types.AgentType = "Cursor"
	AgentTypeFactoryAIDroid types.AgentType = "Factory AI Droid"
	AgentTypeGemini         types.AgentType = "Gemini CLI"
	AgentTypeOpenCode       types.AgentType = "OpenCode"
	AgentTypePi             types.AgentType = "Pi"
	AgentTypeUnknown        types.AgentType = "Unknown"
)

// DefaultAgentName is the registry key for the default agent.
const DefaultAgentName types.AgentName = AgentNameClaudeCode

// GetByAgentType retrieves an agent by its type identifier.
//
// Note: This uses a linear search that instantiates agents until a match is found.
// This is acceptable because:
//   - Agent count is small (~2-20 agents)
//   - Agent factories are lightweight (empty struct allocation)
//   - Called infrequently (commit hooks, rewind, debug commands - not hot paths)
//   - Cost is ~400ns worst case vs milliseconds for I/O operations
//
// Only optimize if agent count exceeds 100 or profiling shows this as a bottleneck.
func GetByAgentType(agentType types.AgentType) (Agent, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	for _, e := range registry {
		if e.factory == nil {
			continue
		}
		ag := e.factory()
		if ag.Type() == agentType {
			return ag, nil
		}
	}

	return nil, fmt.Errorf("unknown agent type: %s", agentType)
}

// AllProtectedDirs returns the union of ProtectedDirs from all registered agents.
func AllProtectedDirs() []string {
	// Copy factories under the lock, then release before calling external code.
	registryMu.RLock()
	factories := make([]Factory, 0, len(registry))
	for _, e := range registry {
		if e.factory == nil {
			continue
		}
		factories = append(factories, e.factory)
	}
	registryMu.RUnlock()

	seen := make(map[string]struct{})
	var dirs []string
	for _, factory := range factories {
		for _, d := range factory().ProtectedDirs() {
			if _, ok := seen[d]; !ok {
				seen[d] = struct{}{}
				dirs = append(dirs, d)
			}
		}
	}
	slices.Sort(dirs)
	return dirs
}

// AllProtectedFiles returns the union of ProtectedFiles from all registered agents.
func AllProtectedFiles() []string {
	// Copy factories under the lock, then release before calling external code.
	registryMu.RLock()
	factories := make([]Factory, 0, len(registry))
	for _, e := range registry {
		if e.factory == nil {
			continue
		}
		factories = append(factories, e.factory)
	}
	registryMu.RUnlock()

	seen := make(map[string]struct{})
	var files []string
	for _, factory := range factories {
		pf, ok := factory().(ProtectedFilesProvider)
		if !ok {
			continue
		}
		for _, file := range pf.ProtectedFiles() {
			if _, ok := seen[file]; !ok {
				seen[file] = struct{}{}
				files = append(files, file)
			}
		}
	}
	slices.Sort(files)
	return files
}

// LauncherFor returns the Launcher implementation for the given agent name,
// or ok=false if the agent does not support subprocess launching. Callers
// should tell the user to start the agent manually in that case.
func LauncherFor(name types.AgentName) (Launcher, bool) {
	a, err := Get(name)
	if err != nil {
		return nil, false
	}
	l, ok := a.(Launcher)
	return l, ok
}

// Default returns the default agent.
// Returns nil if the default agent is not registered.
//
//nolint:errcheck // Factory pattern returns interface; error is acceptable to ignore for default
func Default() Agent {
	a, _ := Get(DefaultAgentName)
	return a
}
