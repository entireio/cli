package agent_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeStubAgent is the smallest Agent that SessionStore needs: a session
// directory and a file layout. resolve mirrors how real agents build a path from
// a session ID, including the escaping case the store exists to catch.
type storeStubAgent struct {
	dir     string
	resolve func(dir, id string) string
}

func (s *storeStubAgent) Name() types.AgentName                    { return "stub" }
func (s *storeStubAgent) Type() types.AgentType                    { return "stub" }
func (s *storeStubAgent) GetSessionDir(string) (string, error)     { return s.dir, nil }
func (s *storeStubAgent) ResolveSessionFile(dir, id string) string { return s.resolve(dir, id) }

// newStore builds a store over a fresh temp directory. Nothing to clean up: a
// store opens its root per operation and closes it again, so it holds no handle
// between calls.
func newStore(t *testing.T, resolve func(dir, id string) string) (*agent.SessionStore, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: dir, resolve: resolve}, dir)
	require.NoError(t, err)
	return store, dir
}

func joinResolve(dir, id string) string { return filepath.Join(dir, id+".jsonl") }

func TestSessionStore_SessionFileResolvesInsideTheStore(t *testing.T) {
	t.Parallel()

	store, dir := newStore(t, joinResolve)
	name, absPath, err := store.SessionFile("abc123")
	require.NoError(t, err)
	assert.Equal(t, "abc123.jsonl", name)
	assert.Equal(t, filepath.Join(dir, "abc123.jsonl"), absPath)
}

// The check the type exists for: an agent's own layout plus a session ID from a
// hook payload must not be able to name a file outside the agent's directory.
// filepath.Join would have produced this path silently.
func TestSessionStore_SessionFileRejectsEscapingSessionID(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, joinResolve)
	_, _, err := store.SessionFile("../../escaped")
	require.ErrorIs(t, err, agent.ErrUnsafeSessionName)
}

func TestSessionStore_SessionFileRejectsUnsafeIDBeforeResolver(t *testing.T) {
	t.Parallel()

	for _, sessionID := range []string{"session.", "CON"} {
		t.Run(sessionID, func(t *testing.T) {
			t.Parallel()

			resolverCalled := false
			store, _ := newStore(t, func(dir, id string) string {
				resolverCalled = true
				return joinResolve(dir, id)
			})
			_, _, err := store.SessionFile(sessionID)
			require.ErrorIs(t, err, agent.ErrUnsafeSessionName)
			assert.False(t, resolverCalled, "unsafe ID must not reach the agent resolver")
		})
	}
}

// An agent that resolves to a sibling directory is rejected too — the store's
// job is to report that, not to guess whether it was intended.
func TestSessionStore_SessionFileRejectsLayoutOutsideTheStore(t *testing.T) {
	t.Parallel()

	elsewhere := t.TempDir()
	store, _ := newStore(t, func(_, id string) string {
		return filepath.Join(elsewhere, id+".jsonl")
	})
	_, _, err := store.SessionFile("abc123")
	require.ErrorIs(t, err, agent.ErrOutsideSessionStore)
}

func TestSessionStore_WriteFileCreatesNestedParents(t *testing.T) {
	t.Parallel()

	store, dir := newStore(t, joinResolve)
	require.NoError(t, store.WriteFile("projects/hash/chats/a.json", []byte("{}"), 0o600))

	got, err := os.ReadFile(filepath.Join(dir, "projects", "hash", "chats", "a.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(got))
}

func TestSessionStore_WriteFileRejectsEscapingName(t *testing.T) {
	t.Parallel()

	store, dir := newStore(t, joinResolve)
	require.Error(t, store.WriteFile("../escaped.json", []byte("nope"), 0o600))

	_, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.json"))
	assert.True(t, os.IsNotExist(err), "an escaping write must not land outside the store")
}

// A symlinked store root is FOLLOWED, deliberately. The store's location comes
// from the agent (GetSessionDir), not from checkpoint metadata or a hook
// payload, and a ~/.claude or ~/.codex managed by a dotfile tool is an ordinary
// setup among exactly the people who run coding agents. Containment starts one
// level down — see TestSessionStore_WriteFileRejectsEscapingName and the
// symlinked-parent cases in the external agent's preflight.
func TestSessionStore_FollowsSymlinkedStoreRoot(t *testing.T) {
	t.Parallel()

	realStore := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "store")
	if err := os.Symlink(realStore, storeDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: storeDir, resolve: joinResolve}, storeDir)
	require.NoError(t, err)

	// ABSOLUTE refs on purpose. A relative one is returned by Name unchanged
	// without s.dir being consulted at all, so it answers nil whether the store
	// root is a symlink, a real directory, or absent — which is what let the
	// realpath case below ship broken.
	require.NoError(t, store.ValidateExternalWriteRef(filepath.Join(storeDir, "session.jsonl")))

	// The same file named through the link's TARGET. A plugin reports its store
	// through get-session-dir and may hand back a realpath'd reference; both
	// spellings are one directory and both must be accepted.
	require.NoError(t, store.ValidateExternalWriteRef(filepath.Join(realStore, "session.jsonl")))

	// Several missing components, so the resolve-the-deepest-existing-prefix walk
	// has to strip more than the leaf. A version of it that stripped only the
	// leaf passes every other assertion here.
	require.NoError(t, store.ValidateExternalWriteRef(filepath.Join(realStore, "a", "b", "session.jsonl")))

	// Still outside is still refused, resolved or not.
	outside := t.TempDir()
	require.Error(t, store.ValidateExternalWriteRef(filepath.Join(outside, "session.jsonl")))

	require.NoError(t, store.WriteFile("session.jsonl", []byte("hi\n"), 0o600))
	assert.FileExists(t, filepath.Join(realStore, "session.jsonl"))
}

// A store that does not exist yet is created even below a symlinked ancestor,
// for the same reason: that is where a dotfile-managed agent directory puts it.
func TestSessionStore_CreatesMissingStoreBelowSymlinkedAncestor(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	linkedParent := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(outside, linkedParent); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	storeDir := filepath.Join(linkedParent, "missing-store")
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: storeDir, resolve: joinResolve}, storeDir)
	require.NoError(t, err)

	require.NoError(t, store.WriteFile("session.jsonl", []byte("hi\n"), 0o600))
	assert.FileExists(t, filepath.Join(outside, "missing-store", "session.jsonl"))
}

// Lstat, not Stat: a dangling session log still exists, and both the rewind and
// resume paths must keep it rather than silently overwrite it.
func TestSessionStore_ExistsReportsDanglingSymlink(t *testing.T) {
	t.Parallel()

	store, dir := newStore(t, joinResolve)
	if err := os.Symlink(filepath.Join(dir, "absent-target"), filepath.Join(dir, "a.jsonl")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	assert.True(t, store.Exists("a.jsonl"))
}

func TestSessionStore_ValidateExternalWriteRefAllowsMissingStore(t *testing.T) {
	t.Parallel()

	storeDir := filepath.Join(t.TempDir(), "missing-store")
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: storeDir, resolve: joinResolve}, storeDir)
	require.NoError(t, err)

	require.NoError(t, store.ValidateExternalWriteRef("session.jsonl"))
	_, err = os.Stat(storeDir)
	assert.True(t, os.IsNotExist(err), "validation must not create the missing store")
}

func TestSessionStore_ValidateExternalWriteRefRejectsUnsafeNameWithMissingStore(t *testing.T) {
	t.Parallel()

	storeDir := filepath.Join(t.TempDir(), "missing-store")
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: storeDir, resolve: joinResolve}, storeDir)
	require.NoError(t, err)

	for _, name := range []string{
		filepath.Join(".. ", "session.jsonl"),
		filepath.Join("session.", "session.jsonl"),
		filepath.Join("bad\x00name", "session.jsonl"),
		filepath.Join("CON", "session.jsonl"),
		filepath.Join("nul.jsonl", "session.jsonl"),
	} {
		require.Error(t, store.ValidateExternalWriteRef(name), name)
	}
	_, err = os.Stat(storeDir)
	assert.True(t, os.IsNotExist(err), "validation must not create the missing store")
}

func TestWriteSessionFile_WritesThroughTheStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ag := &storeStubAgent{dir: dir, resolve: joinResolve}

	err := agent.WriteSessionFile(ag, &agent.AgentSession{
		SessionID:  "abc123",
		RepoPath:   t.TempDir(),
		SessionRef: filepath.Join(dir, "abc123.jsonl"),
		NativeData: []byte("line\n"),
	}, []byte("line\n"), 0o600)
	require.NoError(t, err)

	got, readErr := os.ReadFile(filepath.Join(dir, "abc123.jsonl"))
	require.NoError(t, readErr)
	assert.Equal(t, "line\n", string(got))
}

func TestWriteSessionFile_RequiresASessionRef(t *testing.T) {
	t.Parallel()

	ag := &storeStubAgent{dir: t.TempDir(), resolve: joinResolve}
	require.Error(t, agent.WriteSessionFile(ag, &agent.AgentSession{SessionID: "x"}, nil, 0o600))
	require.Error(t, agent.WriteSessionFile(ag, nil, nil, 0o600))
}

// TestSessionStore_ProbingManyDirectoriesRetainsNoDescriptors pins the reason
// openRoot does not go through osroot.Shared.
//
// searchTranscriptInProjectDirs opens a store for every candidate directory it
// walks under an agent's session base dir. When those roots were memoized, each
// probe retained a directory fd for the life of the process — one per candidate,
// measured — so a few hundred project directories reached RLIMIT_NOFILE, and
// because the registry is shared, os.OpenRoot then failed for .entire and the
// git common dir too.
func TestSessionStore_ProbingManyDirectoriesRetainsNoDescriptors(t *testing.T) {
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("no /proc/self/fd on this platform: %v", err)
		}
		return len(entries)
	}

	base := t.TempDir()
	const candidates = 128
	dirs := make([]string, candidates)
	for i := range candidates {
		dirs[i] = filepath.Join(base, fmt.Sprintf("project-%03d", i))
		require.NoError(t, os.MkdirAll(dirs[i], 0o750))
	}

	before := countFDs()
	for _, dir := range dirs {
		store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: dir, resolve: joinResolve}, dir)
		require.NoError(t, err)
		name, _, err := store.SessionFile("session-id")
		require.NoError(t, err)
		store.Exists(name) // absent in every candidate, as in a real miss
	}
	// Some slack for anything the runtime opens concurrently; the regression
	// this guards produced exactly `candidates` extra descriptors.
	require.Less(t, countFDs()-before, 16,
		"probing %d candidate directories must not retain a descriptor per directory", candidates)
}

// The store itself may not exist yet — an external plugin is allowed to create
// it — so the walk has to resolve a symlinked ANCESTOR while the store and the
// reference below it are both still missing.
func TestSessionStore_FollowsSymlinkedAncestorOfAMissingStore(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	realParent := filepath.Join(base, "real-parent")
	require.NoError(t, os.MkdirAll(realParent, 0o700))
	linkedParent := filepath.Join(base, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	storeDir := filepath.Join(linkedParent, "missing-store")
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: storeDir, resolve: joinResolve}, storeDir)
	require.NoError(t, err)

	// The plugin's own spelling, and the same location through the link's target.
	require.NoError(t, store.ValidateExternalWriteRef(filepath.Join(storeDir, "session.jsonl")))
	require.NoError(t, store.ValidateExternalWriteRef(filepath.Join(realParent, "missing-store", "session.jsonl")))

	// A sibling of the missing store is still outside it.
	require.Error(t, store.ValidateExternalWriteRef(filepath.Join(realParent, "other-store", "session.jsonl")))
}

// Nested directories inherit the store root's 0700 rather than 0750: they hold
// the same transcripts, and Copilot, Cursor and Codex all write into them.
func TestSessionStore_CreatesNestedDirectories0700(t *testing.T) {
	t.Parallel()

	storeDir := filepath.Join(t.TempDir(), "store")
	store, err := agent.OpenSessionStoreAt(&storeStubAgent{dir: storeDir, resolve: joinResolve}, storeDir)
	require.NoError(t, err)
	require.NoError(t, store.WriteFile("nested/deeper/session.jsonl", []byte("hi\n"), 0o600))

	for _, dir := range []string{storeDir, filepath.Join(storeDir, "nested"), filepath.Join(storeDir, "nested", "deeper")} {
		info, err := os.Stat(dir)
		require.NoError(t, err, dir)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), dir)
	}
}
