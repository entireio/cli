package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
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
// need the directory, and they open a root at that point and close it again —
// see openRoot for why a store is not one of the memoized anchors.
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

// openRoot returns an *os.Root over the store directory. The CALLER closes it.
//
// Deliberately not osroot.Shared. That registry is for the handful of long-lived
// anchors a process holds — .entire, the git common dir, the worktree — and it
// never evicts, by design. A session store is not one of them: transcript
// fallback search (searchTranscriptInProjectDirs) opens a store for EVERY
// candidate directory it walks under ~/.claude/projects, and memoizing those
// retained one directory fd per candidate for the life of the process. Measured
// at exactly one fd per directory probed, against zero for the os.Stat this
// replaced; a few hundred project directories is enough to reach the usual 1024
// RLIMIT_NOFILE, at which point os.OpenRoot starts failing for every OTHER
// anchor too, since they all come from the same registry.
//
// The cost of not sharing is one openat per store operation, against reading a
// transcript that is routinely megabytes.
//
// A directory that does not exist is reported unwrapped so callers can classify
// it with os.IsNotExist.
func (s *SessionStore) openRoot() (*os.Root, error) {
	return os.OpenRoot(s.dir) //nolint:wrapcheck // see doc comment
}

// openRootForWrite is openRoot with the store directory created first. The
// directory is the root itself, so it cannot be created through it — this is the
// one place that reaches it from the outside. The caller closes the result.
func (s *SessionStore) openRootForWrite() (*os.Root, error) {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	return s.openRoot()
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
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return osroot.ReadFileNoFollow(root, name) //nolint:wrapcheck // preserved for os.IsNotExist at call sites
}

// WriteFile writes name in the store, creating parent directories. Session
// layouts nest (Gemini keys by project hash, Pi by encoded repo path), so the
// parents are made here rather than at each call site.
func (s *SessionStore) WriteFile(name string, data []byte, perm os.FileMode) error {
	root, err := s.openRootForWrite()
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))); dir != "." {
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o750); err != nil {
			return fmt.Errorf("create session directory: %w", err)
		}
	}
	if info, err := osroot.LstatNoSymlinks(root, name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("write session file: %w", osroot.ErrSymlinkedPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect session file: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, name, data, perm) //nolint:wrapcheck // preserved for os.IsNotExist at call sites
}

// Exists reports whether name is present in the store. Lstat, not Stat: a
// dangling symlink is still a file that exists and must not be overwritten
// silently (see the rewind restore path, which distinguishes the two).
func (s *SessionStore) Exists(name string) bool {
	root, err := s.openRoot()
	if err != nil {
		return false
	}
	defer root.Close()
	_, err = osroot.LstatNoSymlinks(root, name)
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

// sessionStoreForWrite resolves the store a session write lands in.
//
// With a RepoPath, that is the agent's session directory for it, and a
// SessionRef outside that directory is an ERROR rather than a reason to anchor
// somewhere else. It used to fall through to SessionRef's own parent, which made
// the containment check its own off switch: the one input that fails it was the
// one input that disabled it. The comment justifying that said some agents
// resolve a restored session to a sibling directory — they do not.
// RestoredSessionPathResolver's only implementation (Codex) returns
// <sessionDir>/YYYY/MM/DD/rollout-*.jsonl, which nests INSIDE the store, so the
// branch was unreachable as well as unsound.
//
// Without a RepoPath the SessionRef's own directory is the only candidate there
// is. That anchor contains nothing by itself, and it is kept because the
// alternative is refusing to write at all; what makes it acceptable is that
// SessionRef there came from Entire's own resolution rather than from an agent
// payload.
func sessionStoreForWrite(ag SessionLocator, s *AgentSession) (*SessionStore, error) {
	if s.RepoPath == "" {
		return OpenSessionStoreAt(ag, filepath.Dir(s.SessionRef))
	}

	store, err := OpenSessionStore(ag, s.RepoPath)
	if err != nil {
		return nil, err
	}
	if _, nameErr := store.Name(s.SessionRef); nameErr != nil {
		return nil, fmt.Errorf("%w: %s is not inside %s's session directory %s",
			ErrOutsideSessionStore, s.SessionRef, ag.Name(), store.Dir())
	}
	return store, nil
}
