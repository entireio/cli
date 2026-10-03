package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// resolveTranscriptPath determines the correct file path for an agent's session transcript.
// Computes the path dynamically from the current repo location for cross-machine portability.
func resolveTranscriptPath(ctx context.Context, sessionID string, agent agentpkg.Agent) (string, error) {
	// Session IDs reaching this restore path can originate from checkpoint
	// metadata on the shared entire/checkpoints/v1 branch, which is attacker-
	// influenceable. Reject path separators/absolute paths before they reach
	// agent.ResolveSessionFile (some agents return absolute IDs verbatim),
	// preventing transcript writes outside the agent session directory.
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID for transcript path: %w", err)
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get worktree root: %w", err)
	}

	// Through the agent's session store rather than a bare join: SessionFile
	// converts the agent's own layout back into a name inside the store and
	// rejects a session ID that walked out of it. ValidateSessionID above is the
	// first gate; this is the one that does not depend on every future caller
	// remembering it.
	store, err := agentpkg.OpenSessionStore(agent, repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get agent session directory: %w", err)
	}
	_, absPath, err := store.SessionFile(sessionID)
	if err != nil {
		return "", fmt.Errorf("resolve transcript path: %w", err)
	}
	return absPath, nil
}

// discoverActiveTranscript uses the same candidate acceptance as historical
// discovery. Its home boundary includes sibling archives. Explicit stores and
// linked session directories retain the established active-store protocol.
func discoverActiveTranscript(ctx context.Context, id string, ag agentpkg.Agent) (string, error) {
	if err := validation.ValidateSessionID(id); err != nil {
		return "", fmt.Errorf("invalid session ID for transcript path: %w", err)
	}
	worktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve active worktree: %w", err)
	}
	store, err := agentpkg.OpenSessionStore(ag, worktree)
	if err != nil {
		return "", fmt.Errorf("open active session store: %w", err)
	}
	dir := store.Dir()
	if provider, ok := agentpkg.AsAgentHomeProvider(ag); ok {
		if home, err := provider.SessionHome(); err == nil && agentpkg.PathHasDirPrefix(dir, provider.SessionBaseDirUnder(home)) {
			homeStore, err := agentpkg.OpenSessionStoreAt(ag, home)
			if err != nil {
				return "", fmt.Errorf("open active agent home: %w", err)
			}
			name, err := homeStore.Name(dir)
			if err != nil {
				return "", fmt.Errorf("resolve active session directory: %w", err)
			}
			info, statErr := homeStore.Lstat(name)
			if (statErr == nil && info.IsDir()) || errors.Is(statErr, os.ErrNotExist) {
				return discoverSessionFile(homeStore, ag, dir, id, home), nil
			}
			// Only linked session directories justify the legacy root. Permission
			// or I/O errors must not downgrade an otherwise confined store.
			linked := errors.Is(statErr, osroot.ErrSymlinkedPath) || (statErr == nil && info.Mode()&os.ModeSymlink != 0)
			if !linked {
				return "", nil
			}
		}
	}
	return discoverSessionFile(store, ag, dir, id, ""), nil
}

// searchTranscriptInProjectDirs searches for a session transcript across an agent's
// project directories that could plausibly belong to the current repository.
// Agents like Claude Code derive the project directory from the cwd,
// so the transcript may be stored under a different project directory if the session
// was started from a different working directory.
//
// Agents implementing SessionBaseDirProvider are searched from the active base
// directory, walking at most three directory levels below each base. Agents
// implementing AgentHomeProvider also try recorded homes: walked the same way
// when the agent has per-project directories, or resolved directly beneath
// SessionBaseDirUnder when its layout is keyed by session ID alone (Codex,
// Copilot CLI), whose active store discoverActiveTranscript already covered.
//
// The second result is the recorded alternate home, or an empty string for
// a transcript found under the active base directory.
func searchTranscriptInProjectDirs(sessionID string, ag agentpkg.Agent) (found, foundUnderHome string, err error) {
	baseProvider, walksProjects := agentpkg.AsSessionBaseDirProvider(ag)
	homeProvider, hasHomes := agentpkg.AsAgentHomeProvider(ag)
	if !walksProjects && !hasHomes {
		return "", "", fmt.Errorf("fallback transcript search not supported for agent %q", ag.Name())
	}

	type candidate struct {
		dir  string
		home string // "" for the active base dir
	}
	var candidates []candidate
	if walksProjects {
		baseDir, baseErr := baseProvider.GetSessionBaseDir()
		if baseErr != nil {
			return "", "", fmt.Errorf("failed to get base directory: %w", baseErr)
		}
		candidates = append(candidates, candidate{dir: baseDir})
	}

	if hasHomes {
		activeHome, homeErr := homeProvider.SessionHome()
		if homeErr != nil {
			activeHome = ""
		} else {
			activeHome = filepath.Clean(activeHome)
		}
		for _, home := range agentpkg.KnownAgentHomes(ag.Type()) {
			if home == activeHome {
				continue
			}
			candidates = append(candidates, candidate{
				dir:  homeProvider.SessionBaseDirUnder(home),
				home: home,
			})
		}
	}
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("transcript not found under any recorded %s home", ag.Name())
	}

	var searchedDirs []string
	for _, c := range candidates {
		searchedDirs = append(searchedDirs, c.dir)
		var hit string
		if walksProjects {
			var walkErr error
			hit, walkErr = searchOneSessionBaseDir(c.dir, ag, sessionID, c.home)
			if walkErr != nil {
				return "", "", fmt.Errorf("failed to search project directories: %w", walkErr)
			}
		} else {
			hit = probeSessionHome(c.home, homeProvider, sessionID)
		}
		if hit != "" {
			return hit, c.home, nil
		}
	}
	return "", "", fmt.Errorf("transcript not found in any project directory searched (%s)", strings.Join(searchedDirs, ", "))
}

// probeSessionHome resolves an ID using the agent's layout, with the home as
// the read boundary so sibling stores (Codex archived_sessions) are included.
func probeSessionHome(home string, provider agentpkg.AgentHomeProvider, sessionID string) string {
	store, err := agentpkg.OpenSessionStoreAt(provider, home)
	if err != nil {
		return ""
	}
	return discoverSessionFile(store, provider, provider.SessionBaseDirUnder(home), sessionID, home)
}

// searchOneSessionBaseDir finds sessionID under baseDir. It returns an empty
// path without an error if no transcript is found.
func searchOneSessionBaseDir(baseDir string, ag agentpkg.Agent, sessionID, home string) (string, error) {
	// Walk subdirectories with a max depth of 3 (baseDir/project/subdir/file)
	// to avoid scanning unrelated project trees.
	const maxExtraDepth = 3

	var found string
	walkErr := filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip inaccessible dirs (including a baseDir that no longer exists)
		}
		if !d.IsDir() {
			return nil
		}
		// Limit walk depth using relative path from base
		rel, relErr := filepath.Rel(baseDir, path)
		if relErr != nil {
			return filepath.SkipDir
		}
		depth := strings.Count(rel, string(filepath.Separator))
		if depth > maxExtraDepth {
			return filepath.SkipDir
		}
		// Each candidate project directory is its own store: the search is
		// exactly the case where sessionID must not be trusted to stay inside
		// the directory being probed.
		store, storeErr := agentpkg.OpenSessionStoreAt(ag, path)
		if storeErr != nil {
			return nil //nolint:nilerr // an unusable candidate directory is skipped, not fatal to the search
		}
		if hit := discoverSessionFile(store, ag, path, sessionID, home); hit != "" {
			found = hit
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("walk %s: %w", baseDir, walkErr)
	}
	return found, nil
}

// discoverSessionFile accepts only readable regular transcripts and continues
// after rejection. Metadata-only confinement also permits missing transcripts,
// which is useful for lifecycle validation but insufficient for discovery.
func discoverSessionFile(store *agentpkg.SessionStore, ag agentpkg.Agent, dir, id, home string) string {
	candidates, err := store.SessionFileCandidatesIn(dir, id)
	if err != nil {
		return ""
	}
	for _, path := range candidates {
		name, err := store.Name(path)
		if err != nil {
			continue
		}
		var file *os.File
		if home != "" {
			provider, ok := agentpkg.AsAgentHomeProvider(ag)
			if !ok || !agentpkg.HomeConfinesTranscript(provider, home, path) {
				continue
			}
			file, err = agentpkg.OpenTranscriptFileUnderHome(path, home)
		} else if _, _, underEntire := entiredir.Split(path); underEntire {
			// Agent caches use the repository's .entire anchor, even when their
			// session directory is supplied through the legacy store protocol.
			file, err = agentpkg.OpenTranscriptFileUnderHome(path, "")
		} else {
			file, err = store.OpenFile(name)
		}
		if err == nil && file.Close() == nil {
			return path
		}
	}
	return ""
}

// ResolveAgentTranscriptPath returns the path to an existing subagent transcript
// for agentID, or "" when none exists.
//
// It prefers the current layout, paths.SubagentsDir (which is also what the
// turn-end extractor scans), and falls back to the legacy sibling layout —
// agent-<id>.jsonl directly beside the main transcript — so sessions recorded by
// older agent versions still resolve.
//
// Order is the whole point: resolving only the legacy path silently yielded "" for
// every modern Claude Code session, which left task checkpoints without a subagent
// transcript and made file extraction fall back to scanning the main transcript,
// where a subagent's edits never appear.
//
// An empty agentID never resolves — agent-.jsonl is not a real transcript.
//
// strategy.resolveTaskTranscriptPath duplicates this exact layout logic (the
// strategy package cannot import cli, so it cannot call this function
// directly) — a layout change here must be mirrored there.
func ResolveAgentTranscriptPath(transcriptDir, sessionID, agentID string) string {
	if agentID == "" {
		return ""
	}
	name := paths.AgentTranscriptFileName(agentID)
	if nested := filepath.Join(paths.SubagentsDir(transcriptDir, sessionID), name); fileExists(nested) {
		return nested
	}
	if legacy := filepath.Join(transcriptDir, name); fileExists(legacy) {
		return legacy
	}
	return ""
}
