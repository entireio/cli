package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
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

// searchTranscriptInProjectDirs searches for a session transcript across an agent's
// project directories that could plausibly belong to the current repository.
// Agents like Claude Code and Gemini CLI derive the project directory from the cwd,
// so the transcript may be stored under a different project directory if the session
// was started from a different working directory.
//
// The search is scoped to the agent's base directory (e.g., ~/.claude/projects) and only
// walks immediate subdirectories (plus one extra level for agents like Gemini that nest
// chats under <project>/chats/).
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
		name, absPath, resolveErr := store.SessionFile(sessionID)
		if resolveErr != nil {
			// The ID does not resolve inside this candidate — the next one may
			// still hold the session, so keep walking.
			return nil //nolint:nilerr // see comment
		}
		if store.Exists(name) {
			found = absPath
			return filepath.SkipAll
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
