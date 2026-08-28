package paths

import (
	"bytes"
	"context"
	"errors"
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

// WorktreeRoot returns the git worktree root directory — in a linked worktree
// that worktree's root, not the main repository's. The result is cached per
// working directory. Callers that need to distinguish "outside a repository"
// from "could not find out" match ErrNotARepository; nothing else may be
// read as the benign case.
//
// Everything Entire stores is located from this path — .entire via the entiredir
// package, and the git common dir via gitdir — so a failure here used to be
// papered over by callers falling back to a relative path, which then resolved
// against wherever the process happened to be. From a subdirectory that silently
// meant a second .entire beside the agent instead of the repository's one.
//
// The subprocess fails for more reasons than "no repository" — `git` off $PATH
// is the common one — so those two outcomes are separated rather than merged:
// only ErrNotARepository means "there is nothing here", and it is the only
// failure any caller is allowed to answer with a directory of its own.
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

	root, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return "", err
	}

	worktreeRootMu.Lock()
	worktreeRootCache = root
	worktreeRootCacheDir = cwd
	worktreeRootMu.Unlock()

	return root, nil
}

// resolveWorktreeRoot asks git, and treats every failure to get an answer as a
// failure — it never substitutes a guess of its own.
//
// A walk up for a .git entry was tried here and removed. It looks like a free
// improvement (it is the same search git performs, so it agrees with git
// whenever both can run) but it is not, because it can only run in the cases
// where git did NOT agree, and in every one of those git knew something the walk
// cannot see: dubious ownership fires INSIDE a repository, so walking up finds
// the .git git just refused and hands back a root the user's own git will not
// use; a GIT_DIR pointing elsewhere makes the nearest .git the wrong answer, not
// the right one. Refusing to run on a machine whose git is broken is the cheaper
// mistake, and it is the rule RequireEntireDir already states.
//
// The one thing that must not happen is a failure here becoming a path anyway.
// Callers used to fall back to a path relative to the current directory, which
// from a subdirectory silently meant a second .entire beside the agent instead
// of the repository's one. entiredir.anchor is the surviving fallback and it
// fires only on the positive ErrNotARepository verdict below.
func resolveWorktreeRoot(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	// Force C messages so classifyWorktreeRootError can recognise git's
	// "not a repository" on a localized machine. Built by filtering rather than
	// appending, because a duplicate key in the environment resolves to the
	// first match on Unix, not the last. Everything else is inherited, so the
	// GIT_DIR/GIT_WORK_TREE a hook exports still selects the worktree.
	cmd.Env = envWithCMessages(os.Environ())

	output, err := cmd.Output()
	if err != nil {
		return "", classifyWorktreeRootError(ctx, err)
	}

	root := strings.TrimSpace(string(output))
	// Git printed no path but reported success. Whatever produced that, it is
	// not an answer, and treating "" as a worktree root resolves every repo
	// path against the filesystem root.
	if root == "" {
		return "", errors.New("git rev-parse --show-toplevel reported success but printed no path")
	}
	return root, nil
}

// ErrNotARepository reports that git positively identified the working
// directory as being outside any repository. It is the ONLY worktree-root
// failure that means "there is nothing here to look at" — every other one
// (git missing from PATH, a cancelled context, a permission failure, dubious
// ownership, malformed output) means "we could not find out", which is a
// different thing and must not be treated as the benign case.
var ErrNotARepository = errors.New("not a git repository")

// classifyWorktreeRootError separates git's "not a repository" verdict from
// every other reason `git rev-parse --show-toplevel` can fail.
//
// The distinction is load-bearing rather than cosmetic. Callers skip work when
// there is no repository, and settings resolution falls back to a path relative
// to the current directory when the root cannot be resolved — so folding an
// unexpected failure into "not a repository" turns a broken git into a silent
// read of ./.entire/settings.json, which is exactly the path a `.entire`
// symlink redirects.
//
// Identification is positive, not by elimination: git must have run and exited
// non-zero, and must have said so. Exit code 128 alone is not enough, since git
// also uses it for dubious ownership and permission failures, both of which
// happen INSIDE a repository.
func classifyWorktreeRootError(ctx context.Context, err error) error {
	// Checked first: a cancelled context kills the child, and the resulting
	// exit status carries no verdict about the repository.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("failed to get git worktree root: %w", ctxErr)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if bytes.Contains(bytes.ToLower(exitErr.Stderr), []byte("not a git repository")) {
			return fmt.Errorf("%w: %s", ErrNotARepository, firstLine(exitErr.Stderr))
		}
		// Git's own fatal, not just the exit status. "exit status 128" names no
		// cause, and the causes here are ones the user has to act on -- most
		// often safe.directory's ownership check, whose fix is a git config the
		// message states outright.
		if line := firstLine(exitErr.Stderr); line != "" {
			return fmt.Errorf("failed to get git worktree root: %s", line)
		}
	}

	return fmt.Errorf("failed to get git worktree root: %w", err)
}

// firstLine trims git's stderr to its opening line, which carries the fatal.
func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// envWithCMessages returns env with the locale variables git consults for
// message translation replaced by C, leaving everything else untouched.
func envWithCMessages(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "LC_ALL="),
			strings.HasPrefix(kv, "LC_MESSAGES="),
			strings.HasPrefix(kv, "LANGUAGE="),
			strings.HasPrefix(kv, "LANG="):
			continue
		}
		out = append(out, kv)
	}
	// LANGUAGE overrides LC_ALL for gettext, so both are pinned.
	return append(out, "LC_ALL=C", "LANGUAGE=")
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
