package agentimport

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
)

func TestChatsWorkspaceKeyIsCursorsDirectoryName(t *testing.T) {
	t.Parallel()
	// Cursor files /home/dev/acme/foo-bar's sessions under this directory.
	if got := cursor.ChatsWorkspaceKey("/home/dev/acme/foo-bar"); got != "b1558eba625b9abfbeb1e145269c8897" {
		t.Fatalf("ChatsWorkspaceKey = %s", got)
	}
}

// cursorChatsFixture is a Cursor project directory shared by two colliding
// repositories, plus a chats store. Not parallel: it sets the Cursor dirs.
type cursorChatsFixture struct {
	repo, other, transcripts, chats string
}

func newCursorChatsFixture(t *testing.T, sessions ...string) cursorChatsFixture {
	t.Helper()
	repo, other := collidingRepos(t)
	transcripts := filepath.Join(t.TempDir(), "project", "agent-transcripts")
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range sessions {
		writeLines(t, filepath.Join(transcripts, id+".jsonl"), `{"role":"user","message":{"content":"hi"}}`)
	}
	chats := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", transcripts)
	t.Setenv("ENTIRE_TEST_CURSOR_CHATS_DIR", chats)
	return cursorChatsFixture{repo: repo, other: other, transcripts: transcripts, chats: chats}
}

// record files session id under workspace's key, with meta.json's cwd when
// cwd is non-empty.
func (f cursorChatsFixture) record(t *testing.T, key, id, cwd string) {
	t.Helper()
	dir := filepath.Join(f.chats, key, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if cwd != "" {
		writeLines(t, filepath.Join(dir, "meta.json"), jsonLine(t, map[string]any{"schemaVersion": 1, "cwd": cwd}))
	}
}

func sessionIDsOf(files []SessionFile) []string {
	ids := make([]string, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.SessionID)
	}
	slices.Sort(ids)
	return ids
}

// Sessions the Cursor CLI recorded settle a shared project directory: this
// repo's are imported, the colliding repo's are not, with or without meta.json.
func TestCursorDiscover_RecordedWorkspacesSettleASharedDir(t *testing.T) {
	f := newCursorChatsFixture(t, "mine-old", "mine-new", "theirs")
	f.record(t, cursor.ChatsWorkspaceKey(f.repo), "mine-old", "")
	f.record(t, cursor.ChatsWorkspaceKey(normalizePath(f.repo)), "mine-new", normalizePath(f.repo))
	f.record(t, cursor.ChatsWorkspaceKey(normalizePath(f.other)), "theirs", normalizePath(f.other))

	got, err := cursorImporter{}.Discover(f.repo, "", time.Now(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ids := sessionIDsOf(got); !slices.Equal(ids, []string{"mine-new", "mine-old"}) {
		t.Fatalf("imported %v, want only this repo's sessions", ids)
	}
}

// A session nothing recorded still needs the directory to be unshared.
func TestCursorDiscover_UnrecordedSessionInASharedDirIsRefused(t *testing.T) {
	f := newCursorChatsFixture(t, "mine", "ide")
	f.record(t, cursor.ChatsWorkspaceKey(f.repo), "mine", "")

	_, err := cursorImporter{}.Discover(f.repo, "", time.Now(), nil)
	if err == nil || !strings.Contains(err.Error(), "1 of its sessions record no workspace") {
		t.Fatalf("err = %v, want the shared-directory refusal counting the unrecorded session", err)
	}
}

// --path vouches only for sessions nothing recorded; another workspace's
// recorded session is still left out.
func TestCursorDiscover_PathStillDropsAnotherWorkspacesSession(t *testing.T) {
	f := newCursorChatsFixture(t, "ide", "theirs")
	f.record(t, cursor.ChatsWorkspaceKey(f.other), "theirs", "")

	got, err := cursorImporter{}.Discover(f.repo, f.transcripts, time.Now(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ids := sessionIDsOf(got); !slices.Equal(ids, []string{"ide"}) {
		t.Fatalf("imported %v, want only the unrecorded session", ids)
	}
}

func TestCursorSessionWorkspaces(t *testing.T) {
	f := newCursorChatsFixture(t)
	repoKey := cursor.ChatsWorkspaceKey(f.repo)
	sub := filepath.Join(f.repo, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.record(t, repoKey, "both", "")
	f.record(t, cursor.ChatsWorkspaceKey(f.other), "both", "")
	f.record(t, repoKey, "mismatched-meta", f.other)
	f.record(t, cursor.ChatsWorkspaceKey(sub), "subdir", sub)
	if err := os.MkdirAll(filepath.Join(f.chats, "not-a-key", "odd"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := cursorSessionWorkspaces(f.repo, []string{"both", "mismatched-meta", "subdir", "odd", "absent", "../escape"})
	want := map[string]cursorWorkspace{
		"both":            cursorWorkspaceOther,    // filed under another workspace too
		"mismatched-meta": cursorWorkspaceUnknown,  // meta.json cwd doesn't hash to its directory
		"subdir":          cursorWorkspaceThisRepo, // a recorded cwd inside the repo
		"odd":             cursorWorkspaceUnknown,  // only under a directory that isn't a key
		"absent":          cursorWorkspaceUnknown,
		"../escape":       cursorWorkspaceUnknown,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %v, want %v", id, got[id], w)
		}
	}
}
