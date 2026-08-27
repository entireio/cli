package paths

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
)

// Directory constants
const (
	EntireDir         = ".entire"
	EntireTmpDir      = ".entire/tmp"
	EntireMetadataDir = ".entire/metadata"

	osWindows = "windows"
	osDarwin  = "darwin"
)

// Metadata file names
const (
	PromptFileName           = "prompt.txt"
	TranscriptFileName       = "full.jsonl"
	TranscriptFileNameLegacy = "full.log"
	// CompactTranscriptFileName is the compact transcript stored alongside
	// full.jsonl. It holds the full compacted session; this checkpoint's slice
	// begins at the session metadata's compact_transcript_start.
	CompactTranscriptFileName = "transcript.jsonl"
	MetadataFileName          = "metadata.json"
	CheckpointFileName        = "checkpoint.json"
	ContentHashFileName       = "content_hash.txt"
	SettingsFileName          = "settings.json"

	// AssetsDir is the per-session subfolder holding externalized transcript
	// assets (e.g. images); AssetsManifestFile indexes them. AssetsDirName is the
	// bare tree-entry name (no trailing slash) used when walking git trees.
	AssetsDirName      = "assets"
	AssetsDir          = "assets/"
	AssetsManifestFile = "assets/manifest.json"
)

// MetadataBranchName is the orphan branch used by manual-commit strategy to store metadata
const MetadataBranchName = "entire/checkpoints/v1"

// TrailsBranchName is the orphan branch used to store trail metadata.
// Trails are branch-centric work tracking abstractions that link to checkpoints by branch name.
const TrailsBranchName = "entire/trails/v1"

// worktreeRootCache caches the worktree root to avoid repeated git commands.
// The cache is keyed by the current working directory to handle directory changes.
var (
	worktreeRootMu       sync.RWMutex
	worktreeRootCache    string
	worktreeRootCacheDir string
)

// WorktreeRoot returns the git worktree root directory.
// Uses 'git rev-parse --show-toplevel' which returns the working tree toplevel.
// In a worktree this is the worktree root, not the main repository root.
// The result is cached per working directory.
// Returns an error if not inside a git repository.
func WorktreeRoot(ctx context.Context) (string, error) {
	// Get current working directory to check cache validity
	cwd, err := os.Getwd() //nolint:forbidigo // already present in codebase
	if err != nil {
		cwd = ""
	}

	// Check cache with read lock first
	worktreeRootMu.RLock()
	if worktreeRootCache != "" && worktreeRootCacheDir == cwd {
		cached := worktreeRootCache
		worktreeRootMu.RUnlock()
		return cached, nil
	}
	worktreeRootMu.RUnlock()

	// Cache miss - get worktree root and update cache with write lock
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git worktree root: %w", err)
	}

	root := strings.TrimSpace(string(output))

	worktreeRootMu.Lock()
	worktreeRootCache = root
	worktreeRootCacheDir = cwd
	worktreeRootMu.Unlock()

	return root, nil
}

// ClearWorktreeRootCache clears the cached worktree root.
// This is primarily useful for testing when changing directories.
func ClearWorktreeRootCache() {
	worktreeRootMu.Lock()
	worktreeRootCache = ""
	worktreeRootCacheDir = ""
	worktreeRootMu.Unlock()
}

// AbsPath returns the absolute path for a relative path within the repository.
// If the path is already absolute, it is returned as-is.
// Uses WorktreeRoot() to resolve paths relative to the worktree root.
func AbsPath(ctx context.Context, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return relPath, nil
	}

	root, err := WorktreeRoot(ctx)
	if err != nil {
		return "", err
	}

	return filepath.Join(root, relPath), nil
}

// IsInfrastructurePath returns true if the path is part of CLI infrastructure
// (i.e., inside the .entire directory). It is used only to EXCLUDE infra paths
// from checkpoints/tracking, so it matches case-insensitively on
// case-insensitive filesystems via IsProtectedSubpath. Do not use it as a
// containment/allow gate.
func IsInfrastructurePath(path string) bool {
	return IsProtectedSubpath(EntireDir, path)
}

// IsSubpath reports whether child is lexically under parent (or equal to it).
// It uses filepath.Rel, which cleans both inputs and is traversal-resistant:
// a crafted child like "/a/b/../../../etc/passwd" that escapes parent will
// produce a relative path starting with ".." and be rejected.
//
// Matching is case-SENSITIVE. This is the correct primitive for fail-closed
// containment/allow checks (e.g. validating an attacker-influenced path stays
// under an Entire-owned dir): on a case-sensitive volume a differently-cased
// path names a different directory, so folding it in would fail open. For
// EXCLUSION decisions that must also catch case variants on Windows/macOS, use
// IsProtectedSubpath instead.
func IsSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return !IsRelativeTraversal(rel)
}

// IsProtectedSubpath reports whether child is under parent for the purpose of
// EXCLUDING protected/infrastructure content from checkpoints and tracking.
// Unlike IsSubpath it honors OS case-insensitivity (see CaseInsensitiveFS), so
// a case variant of a protected dir (".Claude" vs ".claude") is still excluded
// on Windows/macOS.
//
// SECURITY: never use this for allow/containment decisions. Case-folding widens
// what counts as "inside" parent, which is safe only when the effect is to
// exclude more. On a case-sensitive volume under a case-insensitive GOOS it
// over-matches; for a fail-closed gate that would fail open. Use IsSubpath there.
func IsProtectedSubpath(parent, child string) bool {
	if CaseInsensitiveFS() {
		return IsSubpath(strings.ToLower(parent), strings.ToLower(child))
	}
	return IsSubpath(parent, child)
}

// CaseInsensitiveFS reports whether path comparisons should be case-insensitive
// on the host OS. This is OS-based, not volume-based: Windows and macOS default
// to case-insensitive filesystems, Linux to case-sensitive. Keying on GOOS keeps
// the result deterministic. It must only influence EXCLUSION decisions (see
// IsProtectedSubpath / Equal): on an atypical volume (e.g. a case-sensitive
// macOS APFS volume) it treats a differently-cased path as matching, which is
// safe only when the effect is to exclude more, never to widen an allow gate.
func CaseInsensitiveFS() bool {
	return runtime.GOOS == osWindows || runtime.GOOS == osDarwin
}

// Equal reports whether two paths refer to the same location, honoring the host
// OS's case sensitivity (see CaseInsensitiveFS). Both inputs are cleaned and
// slash-normalized before comparison. Like IsProtectedSubpath, this is intended
// for EXCLUSION matching (e.g. protected files), not fail-closed containment.
func Equal(a, b string) bool {
	a = filepath.Clean(filepath.FromSlash(a))
	b = filepath.Clean(filepath.FromSlash(b))
	if CaseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// IsRelativeTraversal reports whether rel escapes its base directory.
// It accepts both OS-native paths and Git-style slash-normalized paths.
func IsRelativeTraversal(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, `..\`)
}

// ToRelativePath converts an absolute path to relative.
// Returns empty string if the path is outside the working directory.
func ToRelativePath(absPath, cwd string) string {
	absPath = normalizeMSYSPath(absPath)
	cwd = normalizeMSYSPath(cwd)

	// On Windows, MSYS/Git Bash sometimes omits the drive letter, producing
	// paths like /Users/... that filepath.IsAbs doesn't recognize. Prepend
	// the drive letter from cwd so filepath.Rel can match them.
	if runtime.GOOS == osWindows && len(absPath) > 0 && absPath[0] == '/' && len(cwd) >= 2 && cwd[1] == ':' {
		absPath = cwd[:2] + absPath
	}

	if !filepath.IsAbs(absPath) {
		return absPath
	}
	relPath, err := filepath.Rel(cwd, absPath)
	if err != nil || IsRelativeTraversal(relPath) {
		return ""
	}

	return relPath
}

// normalizeMSYSPath converts MSYS/Git Bash-style paths (e.g., /c/Users/...)
// to Windows-style paths (e.g., C:/Users/...) so that filepath.IsAbs and
// filepath.Rel work correctly. On non-Windows platforms this is a no-op.
// Claude Code on Windows outputs MSYS paths in its transcript, but Go's
// filepath package only recognizes Windows-style absolute paths.
func normalizeMSYSPath(p string) string {
	if runtime.GOOS != osWindows {
		return p
	}
	// MSYS paths look like /c/Users/... where the second char is a drive letter
	if len(p) >= 3 && p[0] == '/' && unicode.IsLetter(rune(p[1])) && p[2] == '/' {
		return string(unicode.ToUpper(rune(p[1]))) + ":" + p[2:]
	}
	return p
}

// SessionMetadataDirFromSessionID returns the path to a session's metadata directory
// for the given Entire session ID. The sessionID must be the full, already date-prefixed
// Entire session identifier as stored on disk, not an agent-specific or raw Claude ID.
func SessionMetadataDirFromSessionID(sessionID string) string {
	return EntireMetadataDir + "/" + sessionID
}

// SubagentsDir returns the directory an agent stores a session's subagent
// transcripts in: <transcriptDir>/<sessionID>/subagents.
//
// This layout lives here, in the leaf paths package, because it is needed on both
// sides of the import graph — the lifecycle dispatcher and the strategy, review,
// and agentimport packages all resolve it, and those cannot import each other.
// Before it was named it existed as five copies of the same filepath.Join, which
// is how the SubagentEnd path came to disagree with the turn-end path about where
// subagent transcripts live.
//
// sessionID is the *agent's* session ID (the transcript file's own basename), not
// the date-prefixed Entire session ID.
func SubagentsDir(transcriptDir, sessionID string) string {
	return filepath.Join(transcriptDir, sessionID, "subagents")
}

// AgentTranscriptFileName returns the file name an agent writes a subagent's
// transcript under: agent-<agentID>.jsonl.
func AgentTranscriptFileName(agentID string) string {
	return "agent-" + agentID + ".jsonl"
}

// ExtractSessionIDFromTranscriptPath attempts to extract a session ID from a transcript path.
// Claude transcripts are stored at ~/.claude/projects/<project>/sessions/<id>.jsonl
// If the path doesn't match expected format, returns empty string.
func ExtractSessionIDFromTranscriptPath(transcriptPath string) string {
	// Try to extract from typical path: ~/.claude/projects/<project>/sessions/<id>.jsonl
	parts := strings.Split(filepath.ToSlash(transcriptPath), "/")
	for i, part := range parts {
		if part == "sessions" && i+1 < len(parts) {
			// Return filename without extension
			filename := parts[i+1]
			if strings.HasSuffix(filename, ".jsonl") {
				return strings.TrimSuffix(filename, ".jsonl")
			}
			return filename
		}
	}
	return ""
}
