package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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

// discoverTranscript returns the transcript of sessionID in ag's session
// directory for the current worktree: the first of the agent's candidate files
// that is a regular file, following symbolic links as later reads do (the
// fallback search is stricter; see isTranscriptFileInStore). It returns "" and
// no error when no candidate is one.
//
// When the session directory lies in a store of the agent's home layout, the
// candidates may also lie in the home's other stores, such as a Codex rollout
// moved to archived_sessions.
func discoverTranscript(ctx context.Context, sessionID string, ag agentpkg.Agent) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID for transcript path: %w", err)
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get worktree root: %w", err)
	}
	store, err := agentpkg.OpenSessionStore(ag, repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get agent session directory: %w", err)
	}
	sessionDir := store.Dir()
	inStore := func(string) bool { return true }
	if provider, ok := agentpkg.AsHomeLayoutProvider(ag); ok {
		layout := provider.HomeLayout()
		home, homeErr := provider.SessionHome()
		if homeErr != nil {
			// Not fatal: the session directory is still searched, only the
			// home's other stores are not.
			logging.Debug(ctx, "agent home unavailable, searching only the session directory",
				"agent", string(ag.Name()), "error", homeErr)
		} else if _, ok := layout.StoreContaining(home, sessionDir); ok {
			if store, err = agentpkg.OpenSessionStoreAt(ag, home); err != nil {
				return "", fmt.Errorf("failed to open agent home: %w", err)
			}
			inStore = func(path string) bool {
				_, ok := layout.StoreContaining(home, path)
				return ok
			}
		}
	}

	candidates, err := store.SessionFileCandidatesIn(sessionDir, sessionID)
	if err != nil {
		return "", fmt.Errorf("resolve transcript path: %w", err)
	}
	for _, path := range candidates {
		if !inStore(path) {
			continue
		}
		if info, statErr := agentpkg.StatTranscriptFile(path); statErr == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", nil
}

// searchTranscriptInProjectDirs searches for a session transcript across an agent's
// project directories that could plausibly belong to the current repository.
// Agents like Claude Code derive the project directory from the cwd,
// so the transcript may be stored under a different project directory if the session
// was started from a different working directory.
//
// The search is scoped to the agent's base directory (e.g., ~/.claude/projects) and only
// walks immediate subdirectories (plus a few extra levels for agents that nest
// transcripts under a project subdirectory).
// Only agents implementing SessionBaseDirProvider support this fallback search.
func searchTranscriptInProjectDirs(sessionID string, ag agentpkg.Agent) (string, error) {
	provider, ok := agentpkg.AsSessionBaseDirProvider(ag)
	if !ok {
		return "", fmt.Errorf("fallback transcript search not supported for agent %q", ag.Name())
	}
	baseDir, err := provider.GetSessionBaseDir()
	if err != nil {
		return "", fmt.Errorf("failed to get base directory: %w", err)
	}

	// Walk subdirectories with a max depth of 3 (baseDir/project/subdir/file)
	// to avoid scanning unrelated project trees.
	const maxExtraDepth = 3

	var found string
	walkErr := filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip inaccessible dirs
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
		candidates, resolveErr := store.SessionFileCandidatesIn(path, sessionID)
		if resolveErr != nil {
			// The ID does not resolve inside this candidate — the next one may
			// still hold the session, so keep walking.
			return nil //nolint:nilerr // see comment
		}
		for _, candidate := range candidates {
			name, nameErr := store.Name(candidate)
			if nameErr != nil {
				continue
			}
			if isTranscriptFileInStore(store, name, candidate) {
				found = candidate
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("failed to search project directories: %w", walkErr)
	}
	if found != "" {
		return found, nil
	}
	return "", errors.New("transcript not found in any project directory")
}

// isTranscriptFileInStore reports whether name, the store's name for path, is a
// regular file or a symbolic link to one. Like the store's other reads it
// refuses a path whose directories below the store include a link.
//
// That is stricter than discoverTranscript, which follows a link anywhere on
// the path, as the transcript read after it does. This search reaches each
// store through filepath.WalkDir, which does not follow linked directories, and
// the check keeps it from following one below the store either.
func isTranscriptFileInStore(store *agentpkg.SessionStore, name, path string) bool {
	info, err := store.Lstat(name)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return false
		}
	}
	return info.Mode().IsRegular()
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
