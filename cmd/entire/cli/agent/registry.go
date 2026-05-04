package agent

import (
	"context"
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
	registry   = make(map[types.AgentName]Factory)
)

// Factory creates a new agent instance
type Factory func() Agent

// Register adds an agent factory to the registry.
// Called from init() in each agent implementation.
func Register(name types.AgentName, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Get retrieves an agent by name.
//

func Get(name types.AgentName) (Agent, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent: %s (available: %v)", name, List())
	}
	return factory(), nil
}

// List returns all registered agent names in sorted order.
func List() []types.AgentName {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]types.AgentName, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// StringList returns user-facing agent names, excluding test-only agents.
func StringList() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name, factory := range registry {
		if to, ok := factory().(TestOnly); ok && to.IsTestOnly() {
			continue
		}
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
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

// Detect attempts to auto-detect which agent is being used.
// Iterates registered agents in sorted name order for deterministic results.
// Returns the first agent whose DetectPresence reports true.
func Detect(ctx context.Context) (Agent, error) {
	detected := DetectAll(ctx)
	if len(detected) == 0 {
		return nil, fmt.Errorf("no agent detected (available: %v)", List())
	}
	return detected[0], nil
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

	for _, factory := range registry {
		ag := factory()
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
	for _, f := range registry {
		factories = append(factories, f)
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
	for _, f := range registry {
		factories = append(factories, f)
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

// Default returns the default agent.
// Returns nil if the default agent is not registered.
//
//nolint:errcheck // Factory pattern returns interface; error is acceptable to ignore for default
func Default() Agent {
	a, _ := Get(DefaultAgentName)
	return a
}
