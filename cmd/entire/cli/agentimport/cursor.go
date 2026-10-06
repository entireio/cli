package agentimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// cursorImporter imports Cursor transcripts. Cursor uses the same JSONL line
// format as Claude Code (role-tagged), so it reuses the shared user-prompt
// detection and content extraction. Cursor records neither model nor token
// usage, so imported turns carry an empty model and nil tokens.
type cursorImporter struct{}

func (cursorImporter) Name() string { return string(agent.AgentNameCursor) }

func (cursorImporter) AgentType() types.AgentType { return agent.AgentTypeCursor }

// Discover returns Cursor transcript files for the repo modified within the
// lookback window. Cursor stores sessions either flat (<dir>/<id>.jsonl) or
// nested (<dir>/<id>/<id>.jsonl, the IDE layout); both are discovered.
//
// Cursor's project directory name is a lossy encoding of the repo path, and
// Cursor transcripts record no cwd, so unlike the Claude importer this cannot
// filter per transcript. Instead, when the directory was derived from repoRoot
// (no overridePath), Discover refuses to import if the directory may also
// belong to another path; see cursorProjectOtherPath.
func (cursorImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "cursor", (&cursor.CursorAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	files, err := discoverSessionFiles(dir, now, sessionFilter, func(dir string, e os.DirEntry) (string, string, bool) {
		id, path := cursorSessionFile(dir, e)
		return id, path, path != ""
	}, nil)
	if err != nil || len(files) == 0 || overridePath != "" {
		return files, err
	}
	if other, shared := cursorProjectOtherPath(repoRoot, filepath.Dir(dir), cursorCollisionReadLimit); shared {
		return nil, fmt.Errorf("cursor project directory %s may also hold sessions from %s; "+
			"Cursor transcripts do not record their workspace, so none were imported. "+
			"If every session there belongs to this repository, rerun with: entire import cursor --path %s",
			filepath.Dir(dir), other, dir)
	}
	return files, nil
}

// cursorCollisionReadLimit bounds the directory reads cursorProjectOtherPath
// performs. Only directories whose encoded path is a prefix of the target's are
// read, so real trees need a handful; the bound only guards pathological ones.
const cursorCollisionReadLimit = 2000

// cursorProjectOtherPath reports whether Cursor's project directory for
// repoRoot may also be used by a different path, returning that path (or a
// description when it cannot be determined). It checks, in order:
//
//   - .workspace-trusted, which Cursor writes with the trusted workspacePath
//     (absent is common; present but unreadable is treated as shared);
//   - every existing directory whose Cursor encoding equals repoRoot's, found
//     by walking only the directories whose encoding is a prefix of it.
//
// A colliding repository that has since been deleted or moved is not
// detected. A walk that hits readLimit, or that meets a directory it cannot
// read, is treated as shared (fail closed).
func cursorProjectOtherPath(repoRoot, projectDir string, readLimit int) (string, bool) {
	trusted, ok := cursorTrustedWorkspace(projectDir)
	if !ok {
		return "an unknown path (" + filepath.Join(projectDir, ".workspace-trusted") + " could not be read)", true
	}
	if trusted != "" && !samePath(trusted, repoRoot) {
		return trusted, true
	}
	matches, unscanned := pathsWithEncoding(repoRoot, cursor.SanitizePathForCursor, readLimit)
	for _, m := range matches {
		if !samePath(m, repoRoot) {
			return m, true
		}
	}
	if unscanned != "" {
		return unscanned, true
	}
	return "", false
}

// cursorTrustedWorkspace returns the workspacePath recorded in the project
// directory's .workspace-trusted file, or "" when the file is absent. ok is
// false when the file exists but cannot be read or names no workspace, since
// it may then name another path.
func cursorTrustedWorkspace(projectDir string) (workspace string, ok bool) {
	data, err := os.ReadFile(filepath.Join(projectDir, ".workspace-trusted")) //nolint:gosec // fixed name under the Cursor project dir
	if errors.Is(err, fs.ErrNotExist) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	var trusted struct {
		WorkspacePath string `json:"workspacePath"`
	}
	if json.Unmarshal(data, &trusted) != nil || trusted.WorkspacePath == "" {
		return "", false
	}
	return trusted.WorkspacePath, true
}

// pathsWithEncoding returns every existing directory whose encode(path) equals
// encode(target), target included. encode must map each path character to one
// output character with separators becoming "-" (as the agents' project-dir
// encodings do), so a directory can only lead to a match when its own encoding
// is a prefix of the target's followed by "-"; every other branch is pruned.
//
// unscanned is empty when the walk was complete. Otherwise it describes what
// could not be checked: a directory that could not be listed or a candidate
// that could not be statted (a colliding workspace may sit there unseen, e.g.
// under a traverse-only directory), or the readLimit being exhausted. Entries
// that no longer exist, dangling links and symlink loops are conclusive and
// skipped.
func pathsWithEncoding(target string, encode func(string) string, readLimit int) (matches []string, unscanned string) {
	// Directory listings return canonical names, so the target must be in that
	// spelling too (resolved symlinks; on Windows, long names instead of 8.3
	// short names like RUNNER~1), or no listed entry can lead to it.
	target = normalizePath(target)
	want := encode(target)
	queue := []string{filepath.VolumeName(target) + string(filepath.Separator)}
	for reads := 0; len(queue) > 0; reads++ {
		if reads >= readLimit {
			return matches, "an unscanned path (collision check exceeded its directory budget)"
		}
		dir := queue[0]
		queue = queue[1:]
		// ReadDir returns what it read alongside an error; use it, so a real
		// match is still reported by name.
		entries, err := os.ReadDir(dir)
		if err != nil && !isGone(err) && unscanned == "" {
			unscanned = "a path under " + dir + ", which could not be listed"
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			enc := encode(p)
			if enc != want && !strings.HasPrefix(want, enc+"-") {
				continue
			}
			info, statErr := os.Stat(p)
			if statErr != nil {
				if !isGone(statErr) && !errors.Is(statErr, syscall.ELOOP) && unscanned == "" {
					unscanned = p + ", which could not be checked"
				}
				continue
			}
			if !info.IsDir() {
				continue
			}
			if enc == want {
				matches = append(matches, p)
			} else {
				queue = append(queue, p)
			}
		}
	}
	return matches, unscanned
}

// isGone reports whether err means the path no longer exists as a directory
// (removed, or replaced by a file, between being listed and being read).
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// samePath reports whether a and b name the same location after cleaning and
// best-effort symlink resolution, or, when both exist, are the same directory
// by file identity (which also covers case-insensitive filesystems).
func samePath(a, b string) bool {
	if normalizePath(a) == normalizePath(b) {
		return true
	}
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(ai, bi)
}

// cursorSessionFile maps a directory entry to a (sessionID, transcript path),
// resolving both the flat and nested Cursor layouts. Returns an empty path for
// entries that are not Cursor transcripts.
func cursorSessionFile(dir string, e os.DirEntry) (sessionID, path string) {
	if e.IsDir() {
		nested := filepath.Join(dir, e.Name(), e.Name()+".jsonl")
		if _, err := os.Stat(nested); err == nil {
			return e.Name(), nested
		}
		return "", ""
	}
	if !strings.HasSuffix(e.Name(), ".jsonl") {
		return "", ""
	}
	return strings.TrimSuffix(e.Name(), ".jsonl"), filepath.Join(dir, e.Name())
}

// SplitTurns produces one Turn per user-prompt line, bounded by the next. It
// reuses the package's shared JSONL helpers; Cursor carries no token usage or
// model, so those fields are left zero.
//
// Real Cursor lines carry only role + message — there is no per-turn uuid or
// timestamp (see cursor/AGENT.md). The append-only line index is the stable
// turn key (as the Codex importer does), so each prompt yields a distinct
// checkpoint ID instead of colliding on an empty UUID and dropping every turn
// after the first. The timestamp falls back to the transcript file's modtime
// (as the Factory importer does).
func (cursorImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	var createdAt time.Time
	if info, statErr := os.Stat(sf.Path); statErr == nil {
		createdAt = info.ModTime()
	}
	return splitLineTurns(splitRawLines(full), isUserPromptLine,
		func(rawLines [][]byte, start, _ int, _ []byte) (*Turn, error) {
			var rec struct {
				Message json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal(rawLines[start], &rec); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in isUserPromptLine
				return nil, nil
			}
			return &Turn{
				UUID:      strconv.Itoa(start),
				Prompt:    transcript.ExtractUserContent(rec.Message),
				CreatedAt: createdAt, CreatedAtFromModTime: true,
			}, nil
		})
}
