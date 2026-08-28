package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// SessionStore is one agent's own session directory, held as an *os.Root.
//
// Every agent keeps its transcripts somewhere Entire does not own —
// ~/.claude/projects/<hash>/, ~/.codex/sessions/, Cursor's SQLite store,
// ~/.pi/agent/sessions/<encoded-repo>/, a temp dir for OpenCode — and Entire
// reads and writes into all of them. The session ID that selects a file inside
// one comes from an agent hook payload, so this is where an untrusted name
// becomes a path, and a store gives each agent one handle to resolve it through.
//
// It deliberately does NOT try to contain the agent's own writes: the agent is a
// separate process writing its own store. What it contains is Entire's reads and
// writes, which is the half Entire is responsible for.
type SessionStore struct {
	agent SessionLocator
	dir   string
}

// SessionLocator is the slice of Agent a session store needs: where the agent
// keeps sessions for a repo, and how it names a session's file inside that
// directory. Narrow on purpose — a store has no business with transcripts,
// chunking, or hooks, and depending on three methods instead of thirty is what
// lets a test supply one without implementing the whole agent contract.
type SessionLocator interface {
	Name() types.AgentName
	GetSessionDir(repoPath string) (string, error)
	ResolveSessionFile(sessionDir, agentSessionID string) string
}

// ErrOutsideSessionStore reports a session file that does not lie inside the
// agent's session directory. Callers match it with errors.Is.
var ErrOutsideSessionStore = errors.New("path is outside the agent's session directory")

// OpenSessionStore returns ag's session store for repoPath.
//
// The directory need NOT exist yet: resolving a session file is path arithmetic
// plus a containment check, and callers legitimately ask where a transcript
// *would* live before anything has created the directory. Only the I/O methods
// need the directory, and they open the root at that point — memoized per
// directory via osroot.Shared, so every call site in one repo shares a handle.
func OpenSessionStore(ag SessionLocator, repoPath string) (*SessionStore, error) {
	if ag == nil {
		return nil, errors.New("agent is required to open a session store")
	}
	dir, err := ag.GetSessionDir(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s session directory: %w", ag.Name(), err)
	}
	return OpenSessionStoreAt(ag, dir)
}

// OpenSessionStoreAt is OpenSessionStore for a session directory the caller
// already resolved — the cross-project fallbacks that scan sibling directories,
// and agentimport, which is handed one.
func OpenSessionStoreAt(ag SessionLocator, dir string) (*SessionStore, error) {
	if dir == "" {
		return nil, errors.New("session directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	return &SessionStore{agent: ag, dir: abs}, nil
}

// Dir returns the store's absolute directory. It is for messages and for the
// paths Entire hands to other processes (an agent's own resume command, a
// transcript path recorded as a checkpoint's SessionRef); it is not an
// invitation to do I/O on the result.
func (s *SessionStore) Dir() string { return s.dir }

// Root returns the store's shared *os.Root, opening it on first use. Owned by
// the registry: do not close. A directory that does not exist is reported
// unwrapped so callers can classify it with os.IsNotExist.
func (s *SessionStore) Root() (*os.Root, error) {
	return osroot.Shared(s.dir) //nolint:wrapcheck // see doc comment
}

// rootForWrite is Root with the store directory created first. The directory is
// the root itself, so it cannot be created through it — this is the one place
// that reaches it from the outside.
func (s *SessionStore) rootForWrite() (*os.Root, error) {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	return s.Root()
}

// SessionFile resolves agentSessionID to a name inside the store, and to the
// absolute path of the same file.
//
// This is the check the whole type exists for. The agent decides its own file
// layout via ResolveSessionFile — some nest, some append an extension — and the
// ID reaching it came from a hook payload. Converting the result back into a
// name relative to the store rejects an ID that walked out of the directory,
// which a plain filepath.Join would have produced silently.
func (s *SessionStore) SessionFile(agentSessionID string) (name, absPath string, err error) {
	resolved := s.agent.ResolveSessionFile(s.dir, agentSessionID)
	name, err = s.Name(resolved)
	if err != nil {
		return "", "", err
	}
	return name, resolved, nil
}

// Name converts a path into a name inside the store, reporting
// ErrOutsideSessionStore for one that is not. A path already relative to the
// store is accepted as-is.
func (s *SessionStore) Name(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrOutsideSessionStore)
	}
	if !filepath.IsAbs(p) {
		cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
		if cleaned == "." || paths.IsRelativeTraversal(cleaned) {
			return "", fmt.Errorf("%w: %s", ErrOutsideSessionStore, p)
		}
		return cleaned, nil
	}
	rel, err := filepath.Rel(s.dir, p)
	if err != nil || rel == "." || paths.IsRelativeTraversal(rel) {
		return "", fmt.Errorf("%w: %s is not inside %s", ErrOutsideSessionStore, p, s.dir)
	}
	return filepath.ToSlash(rel), nil
}

// ReadFile reads name from the store.
func (s *SessionStore) ReadFile(name string) ([]byte, error) {
	root, err := s.Root()
	if err != nil {
		return nil, err
	}
	return osroot.ReadFile(root, name) //nolint:wrapcheck // preserved for os.IsNotExist at call sites
}

// WriteFile writes name in the store, creating parent directories. Session
// layouts nest (Gemini keys by project hash, Pi by encoded repo path), so the
// parents are made here rather than at each call site.
func (s *SessionStore) WriteFile(name string, data []byte, perm os.FileMode) error {
	root, err := s.rootForWrite()
	if err != nil {
		return err
	}
	if dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))); dir != "." {
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o750); err != nil {
			return fmt.Errorf("create session directory: %w", err)
		}
	}
	return osroot.WriteFile(root, name, data, perm) //nolint:wrapcheck // preserved for os.IsNotExist at call sites
}

// Exists reports whether name is present in the store. Lstat, not Stat: a
// dangling symlink is still a file that exists and must not be overwritten
// silently (see the rewind restore path, which distinguishes the two).
func (s *SessionStore) Exists(name string) bool {
	root, err := s.Root()
	if err != nil {
		return false
	}
	_, err = root.Lstat(name)
	return err == nil
}

// WriteSessionFile writes data to s.SessionRef through ag's own session store,
// creating parent directories.
//
// It exists because every agent's WriteSession was the same four lines —
// os.MkdirAll of the parent, then os.WriteFile of an absolute SessionRef — eight
// times over, each one taking the path on trust. Routing them through the store
// makes the containment one decision instead of eight, and the parent-directory
// create stops being something each agent has to remember.
//
// The store is anchored on s.RepoPath when it is set, which is the agent's real
// session directory for that repo. When it is not (callers that only carry a
// path, such as a restore driven from checkpoint metadata), the SessionRef's own
// directory anchors it: that contains nothing by itself, but it keeps the write
// on the same code path, and SessionRef there came from Entire's own resolution
// rather than from an agent payload.
func WriteSessionFile(ag SessionLocator, s *AgentSession, data []byte, perm os.FileMode) error {
	if s == nil {
		return errors.New("session is nil")
	}
	if s.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	store, err := sessionStoreForWrite(ag, s)
	if err != nil {
		return err
	}
	name, err := store.Name(s.SessionRef)
	if err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	if err := store.WriteFile(name, data, perm); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// sessionStoreForWrite resolves the store a session write lands in, preferring
// the agent's session directory for s.RepoPath and falling back to SessionRef's
// own directory. The fallback also covers the case where RepoPath is set but the
// SessionRef lies outside that store — some agents resolve a restored session to
// a sibling directory (see RestoredSessionPathResolver).
func sessionStoreForWrite(ag SessionLocator, s *AgentSession) (*SessionStore, error) {
	if s.RepoPath != "" {
		if store, err := OpenSessionStore(ag, s.RepoPath); err == nil {
			if _, nameErr := store.Name(s.SessionRef); nameErr == nil {
				return store, nil
			}
		}
	}
	return OpenSessionStoreAt(ag, filepath.Dir(s.SessionRef))
}
