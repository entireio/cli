package agentimport

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// cursorWorkspace is what Cursor's chats store says about a session's
// workspace.
type cursorWorkspace int

const (
	// cursorWorkspaceUnknown: no usable record (an IDE session, a deleted
	// store, or a layout this code doesn't recognize).
	cursorWorkspaceUnknown cursorWorkspace = iota
	// cursorWorkspaceThisRepo: every record names this repository.
	cursorWorkspaceThisRepo
	// cursorWorkspaceOther: a record names another workspace.
	cursorWorkspaceOther
)

// cursorChatsWorkspaceLimit bounds the workspace directories read from the
// chats store; past it every session is left unknown.
const cursorChatsWorkspaceLimit = 2000

// cursorMetaMaxBytes bounds a meta.json read; real ones are ~200 bytes.
const cursorMetaMaxBytes = 64 << 10

var cursorWorkspaceKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// cursorSessionWorkspaces classifies sessionIDs by the workspace Cursor's CLI
// recorded for each. Cursor transcripts carry no cwd, but the CLI files every
// session under <chats>/<md5 of the workspace path>/<session id>/, and newer
// versions also write meta.json with the cwd. That is exact where the projects
// directory name is lossy, so it can tell repositories whose paths collide
// there apart.
//
// A meta.json cwd decides an entry (repoMatches, so a subdirectory counts);
// without one the directory name must be one of repoRoot's keys. An entry
// whose meta.json cwd doesn't hash to its directory means the layout isn't
// what this expects, so it proves nothing. A session filed under this repo
// and another is another's: importing it could leak the other's turns.
// Anything unreadable leaves the session unknown, never this repo's.
func cursorSessionWorkspaces(repoRoot string, sessionIDs []string) map[string]cursorWorkspace {
	out := make(map[string]cursorWorkspace, len(sessionIDs))
	base, err := cursor.ChatsBaseDir()
	if err != nil {
		return out
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) > cursorChatsWorkspaceLimit {
		return out
	}
	own := cursorRepoKeys(repoRoot)
	for _, id := range sessionIDs {
		if validation.ValidateSessionID(id) != nil {
			continue
		}
		verdict, unknown := cursorWorkspaceUnknown, false
		for _, e := range entries {
			if !e.IsDir() || !cursorWorkspaceKeyPattern.MatchString(e.Name()) {
				continue
			}
			dir := filepath.Join(base, e.Name(), id)
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				continue
			}
			switch cursorEntryWorkspace(repoRoot, e.Name(), dir, own) {
			case cursorWorkspaceOther:
				verdict = cursorWorkspaceOther
			case cursorWorkspaceThisRepo:
				if verdict == cursorWorkspaceUnknown {
					verdict = cursorWorkspaceThisRepo
				}
			case cursorWorkspaceUnknown:
				unknown = true
			}
		}
		if verdict == cursorWorkspaceThisRepo && unknown {
			verdict = cursorWorkspaceUnknown
		}
		out[id] = verdict
	}
	return out
}

// cursorEntryWorkspace classifies one <key>/<session id> directory.
func cursorEntryWorkspace(repoRoot, key, dir string, own map[string]bool) cursorWorkspace {
	cwd, ok := cursorMetaCwd(filepath.Join(dir, "meta.json"))
	if !ok {
		return cursorWorkspaceUnknown
	}
	if cwd != "" {
		switch {
		case cursor.ChatsWorkspaceKey(cwd) != key:
			return cursorWorkspaceUnknown
		case repoMatches(cwd, repoRoot):
			return cursorWorkspaceThisRepo
		default:
			return cursorWorkspaceOther
		}
	}
	if own[key] {
		return cursorWorkspaceThisRepo
	}
	return cursorWorkspaceOther
}

// cursorMetaCwd reads the cwd from a session's meta.json: "" when the file is
// absent (older CLI versions) or records none; ok is false when it exists but
// can't be read or parsed.
func cursorMetaCwd(path string) (cwd string, ok bool) {
	f, err := os.Open(path) //nolint:gosec // fixed name under a validated session dir in Cursor's store
	if os.IsNotExist(err) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, cursorMetaMaxBytes+1))
	if err != nil || len(data) > cursorMetaMaxBytes {
		return "", false
	}
	var meta struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return "", false
	}
	return meta.Cwd, true
}

// cursorRepoKeys are the chats keys repoRoot may be filed under: Cursor
// hashes the path as it saw it, which may be the symlink-resolved spelling.
// A spelling joins only if it is the same directory, so another path's key
// can never count as this repo's.
func cursorRepoKeys(repoRoot string) map[string]bool {
	keys := map[string]bool{}
	candidates := []string{repoRoot, filepath.Clean(repoRoot), normalizePath(repoRoot)}
	if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, c := range candidates {
		if c == repoRoot || samePath(c, repoRoot) {
			keys[cursor.ChatsWorkspaceKey(c)] = true
		}
	}
	return keys
}
