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
	require.ErrorIs(t, err, agent.ErrOutsideSessionStore)
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
	t.Parallel()

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
