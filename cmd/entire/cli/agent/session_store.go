package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
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

// ErrUnsafeSessionName reports a name that is malformed as a filesystem
// component — a traversal segment, a control character, a trailing space or
// period, a Windows device name or volume separator.
//
// Separate from ErrOutsideSessionStore on purpose: that one says a path
// resolved OUT of the store, which is a containment failure, and reporting it
// for a merely malformed ID produced "path is outside the agent's session
// directory: invalid session ID \"session.\": ends with period", which names
// the wrong problem. Callers match either with errors.Is.
var ErrUnsafeSessionName = errors.New("unsafe session file name")

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
//
// The store's own location is not a boundary Entire enforces: it comes from the
// agent (GetSessionDir), not from checkpoint data or a hook payload, and it
// routinely lives under a symlinked ~/.claude or ~/.codex. What IS enforced is
// everything below it — see WriteFile, which creates nested directories with
// MkdirAllNoSymlink and refuses a symlinked leaf.
// 0700, not 0750: this directory holds session transcripts. It matches what
// the resume path used to create it with before that MkdirAll was removed as
// redundant, and a no-op when the agent already made the directory itself.
func (s *SessionStore) openRootForWrite() (*os.Root, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
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
	if err := validation.ValidateSessionID(agentSessionID); err != nil {
		return "", "", fmt.Errorf("resolve session file: %w: %w", ErrUnsafeSessionName, err)
	}
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
		// Malformed, not outside: nothing was compared against the store.
		return "", fmt.Errorf("%w: empty path", ErrUnsafeSessionName)
	}
	// VolumeName alongside IsAbs, the pairing validation.ValidateSessionID and
	// gitrepo's alternates checks already use. A Windows drive-relative path
	// like "C:foo" contains no separator and IsAbs reports false, so it would
	// otherwise be taken as a name inside the store — while filepath.Join drops
	// the base directory when the appended element carries a volume. Sending it
	// down the absolute branch makes filepath.Rel reject it across volumes,
	// which is the answer this containment check exists to give. No-op on Unix,
	// where VolumeName is always empty.
	if !filepath.IsAbs(p) && filepath.VolumeName(p) == "" {
		cleaned := cleanRelativeName(p)
		if relativeNameEscapes(cleaned) {
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

func cleanRelativeName(p string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
}

// relativeNameEscapes reports whether an already-cleaned relative name leaves
// its own base. One implementation, shared by Name and by the external-agent
// preflight, which has no store to resolve against and used to carry a copy.
func relativeNameEscapes(cleaned string) bool {
	return cleaned == "." || paths.IsRelativeTraversal(cleaned)
}

// SessionRefIsFilesystemPath reports whether ref is unambiguously a filesystem
// path rather than an agent-defined opaque key.
func SessionRefIsFilesystemPath(ref string) bool {
	return filepath.IsAbs(ref) || filepath.VolumeName(ref) != ""
}

// ValidateExternalSessionRef applies every rule on an external agent's
// session_ref that needs no session store, and reports whether ref is
// filesystem-shaped — that is, whether the caller must also run
// (*SessionStore).ValidateExternalWriteRef against the agent's store.
//
// The protocol leaves session_ref agent-defined, so a relative value may be an
// opaque key (a database row, a tenant-scoped identifier) and is forwarded as
// given. Two rules still apply to it: it must not be rooted, and it must not
// lexically escape its own base.
//
// Every rule here is decided without a session store, so every failure reports
// ErrUnsafeSessionName and never ErrOutsideSessionStore: `<store>/./s.jsonl`
// cleans to a file INSIDE the store, and a rooted reference was compared
// against nothing at all. Containment is ValidateExternalWriteRef's answer to
// give.
//
// Deliberately independent of RepoPath. The store-backed half needs a repo to
// resolve a session directory; these rules do not, and gating them on a field
// both current callers happen to set is a check that disappears for the next
// caller that does not.
func ValidateExternalSessionRef(ref string) (filesystemPath bool, err error) {
	if ref == "" {
		return false, nil
	}
	if SessionRefIsFilesystemPath(ref) {
		// A dot component survives no round trip through a plugin that joins or
		// normalizes it, so it is refused rather than cleaned away here.
		for _, component := range strings.Split(filepath.ToSlash(ref), "/") {
			if component == "." || component == ".." {
				return false, fmt.Errorf("%w: %s contains a dot path component", ErrUnsafeSessionName, ref)
			}
		}
		return true, nil
	}
	if os.IsPathSeparator(ref[0]) {
		return false, fmt.Errorf("%w: %s is rooted", ErrUnsafeSessionName, ref)
	}
	if relativeNameEscapes(cleanRelativeName(ref)) {
		return false, fmt.Errorf("%w: %s escapes its relative base", ErrUnsafeSessionName, ref)
	}
	return false, nil
}

// ValidateExternalWriteRef is the store-backed half of the preflight: it
// resolves ref as a name inside the store and checks that name. Callers pass a
// ref that ValidateExternalSessionRef reported as filesystem-shaped.
//
// The Name-then-check order is WriteFile's own prologue and stays here rather
// than in every caller.
func (s *SessionStore) ValidateExternalWriteRef(ref string) error {
	name, err := s.Name(ref)
	if err != nil {
		// Name is lexical, and the plugin is a separate program that need not
		// spell the store the way get-session-dir did. A dotfile-managed
		// ~/.agentx/sessions reported as the link and read back as its target —
		// or anything under macOS /var, which is a link to /private/var — is the
		// SAME directory, and refusing it would break exactly the setup the
		// built-in write path deliberately follows (see openRootForWrite). So a
		// containment failure is retried with both sides resolved before it is
		// believed.
		resolved, ok := s.nameAcrossSymlinks(ref)
		if !ok {
			return err
		}
		name = resolved
	}
	return s.validateWritePath(name)
}

// nameAcrossSymlinks retries Name with the store and the reference both
// resolved. It reports false when either cannot be resolved, so the caller
// keeps the lexical answer rather than trading a definite refusal for an
// unknown.
//
// Used by the external preflight only. The built-in write path
// (WriteSessionFile, sessionStoreForWrite) compares lexically and needs no
// retry because there is only one speller: its SessionRef is derived from the
// same GetSessionDir result the store was opened with. An external plugin is a
// separate program that reports its directory and returns its reference
// independently, which is what puts two spellings of one directory in play.
func (s *SessionStore) nameAcrossSymlinks(ref string) (string, bool) {
	realDir, ok := evalSymlinksAllowingMissingLeaf(s.dir)
	if !ok {
		return "", false
	}
	realRef, ok := evalSymlinksAllowingMissingLeaf(ref)
	if !ok {
		return "", false
	}
	rel, err := filepath.Rel(realDir, realRef)
	if err != nil || rel == "." || paths.IsRelativeTraversal(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// evalSymlinksAllowingMissingLeaf resolves the deepest existing prefix of p and
// re-appends the rest. filepath.EvalSymlinks requires the whole path to exist,
// and the reference being validated names a file that is about to be written —
// so the leaf, and sometimes its directory, legitimately does not exist yet.
func evalSymlinksAllowingMissingLeaf(p string) (string, bool) {
	rest := ""
	for cur := p; ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), true
		}
		if !os.IsNotExist(err) {
			return "", false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// validateWritePath rejects an unsafe component in name and a symlink at name
// itself. A missing store is allowed because the external agent may create it.
//
// This is a point-in-time preflight for a path handed to an external
// subprocess, not a containment boundary: the subprocess can race it, and the
// store's own location is the agent's to choose. Built-in writes go through
// WriteFile, which repeats the name check and then writes through an os.Root.
func (s *SessionStore) validateWritePath(name string) error {
	if err := validateWriteName(name); err != nil {
		return err
	}

	root, err := s.openRoot()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect session write path: %w", err)
	}
	defer root.Close()

	info, err := osroot.LstatNoSymlinks(root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect session write path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: %w", name, osroot.ErrSymlinkedPath)
	}
	return nil
}

// validateWriteName checks each component of name. Lexical only — it touches no
// filesystem, which is why it reports ErrUnsafeSessionName rather than the
// containment sentinel.
func validateWriteName(name string) error {
	for _, component := range strings.Split(filepath.ToSlash(name), "/") {
		if err := validation.ValidateFileNameComponent(component); err != nil {
			return fmt.Errorf("validate session file name: %w: %w", ErrUnsafeSessionName, err)
		}
	}
	return nil
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
// layouts nest (Copilot `<id>/events.jsonl`, Cursor `<id>/<id>.jsonl`, Codex
// `YYYY/MM/DD/`), so the parents are made here rather than at each call site.
//
// Not Pi, which this comment used to cite: it resolves to a flat name and puts
// its project component in GetSessionDir, i.e. in the store root — so it is an
// agent for which the MkdirAll below never fires.
func (s *SessionStore) WriteFile(name string, data []byte, perm os.FileMode) error {
	if err := validateWriteName(name); err != nil {
		return err
	}
	root, err := s.openRootForWrite()
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))); dir != "." {
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o700); err != nil {
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

// Lstat returns the FileInfo for name inside the store without following a
// symlink at any component. The leaf is returned as-is, so a caller can tell a
// symlink from a regular file and refuse it. A missing name is reported
// unwrapped for os.IsNotExist / errors.Is(err, fs.ErrNotExist).
func (s *SessionStore) Lstat(name string) (os.FileInfo, error) {
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return osroot.LstatNoSymlinks(root, name) //nolint:wrapcheck // preserved for os.IsNotExist at call sites
}

// CreateExclusive creates name as an empty file inside the store, creating
// parent directories, and fails when anything already exists there — the
// error wraps fs.ErrExist so callers can treat "the agent wrote it first" as
// success. A symlink in any component, including the leaf, is refused rather
// than followed. It exists for the late-transcript agents that must
// materialise a placeholder without ever replacing a file the agent has since
// written, which an atomic rename would do.
func (s *SessionStore) CreateExclusive(name string, perm os.FileMode) error {
	root, err := s.openRootForWrite()
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))); dir != "." {
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o700); err != nil {
			return fmt.Errorf("create session directory: %w", err)
		}
	}
	f, err := osroot.OpenFileNoFollow(root, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err //nolint:wrapcheck // preserved for errors.Is(err, fs.ErrExist) at call sites
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close session file: %w", err)
	}
	return nil
}

// Exists reports whether name is present in the store. Lstat, not Stat: a
// dangling symlink is still a file that exists and must not be overwritten
// silently (see the rewind restore path, which distinguishes the two).
func (s *SessionStore) Exists(name string) bool {
	_, err := s.Lstat(name)
	return err == nil
}

// IsDir reports whether name is a real directory in the store. Symlinks in the
// path are rejected by LstatNoSymlinks rather than followed.
func (s *SessionStore) IsDir(name string) bool {
	info, err := s.Lstat(name)
	return err == nil && info.IsDir()
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
