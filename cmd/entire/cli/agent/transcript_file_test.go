package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

func TestReadTranscriptFile_RejectsLeafSymlinkInEntire(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	root, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	require.NoError(t, osroot.MkdirAllNoSymlink(root, "tmp", 0o750))
	target := filepath.Join(worktree, paths.EntireDir, "tmp", "target.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	link := filepath.Join(worktree, paths.EntireDir, "tmp", "transcript.jsonl")
	if err := os.Symlink("target.jsonl", link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err = agent.ReadTranscriptFile(link)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

func TestReadTranscriptFile_AllowsExternalAgentFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("external"), 0o600))
	got, err := agent.ReadTranscriptFile(path)
	require.NoError(t, err)
	require.Equal(t, "external", string(got))
}

// TestReadTranscriptFileUnderHome_RejectsSwappedSymlink is the attack
// session_adopt's validation alone cannot stop: validation ran once, at
// adoption time, over a path that was then a regular file. This simulates an
// attacker swapping that same path for a symlink afterward — before
// condensation, in a different process, gets around to reading it — and
// confirms the confined read refuses to follow it rather than quietly
// returning whatever the symlink now points at.
func TestReadTranscriptFileUnderHome_RejectsSwappedSymlink(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	projectsDir := filepath.Join(home, "projects", "adopt-test")
	require.NoError(t, os.MkdirAll(projectsDir, 0o750))
	transcriptPath := filepath.Join(projectsDir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("legitimate transcript"), 0o600))

	// Validation (elsewhere) passed against the regular file above. Now swap
	// it for a symlink pointing outside the home, as an attacker racing the
	// window between validation and this later read would.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_ed25519")
	require.NoError(t, os.WriteFile(secret, []byte("super secret key material"), 0o600))
	require.NoError(t, os.Remove(transcriptPath))
	if err := os.Symlink(secret, transcriptPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err := agent.ReadTranscriptFileUnderHome(transcriptPath, home)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

// TestReadTranscriptFileUnderHome_ReadsConfinedFile is the positive case:
// a transcript genuinely beneath the recorded home still reads normally.
func TestReadTranscriptFileUnderHome_ReadsConfinedFile(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	projectsDir := filepath.Join(home, "projects", "adopt-test")
	require.NoError(t, os.MkdirAll(projectsDir, 0o750))
	transcriptPath := filepath.Join(projectsDir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("legitimate transcript"), 0o600))

	got, err := agent.ReadTranscriptFileUnderHome(transcriptPath, home)
	require.NoError(t, err)
	require.Equal(t, "legitimate transcript", string(got))
}

// TestReadTranscriptFileUnderHome_EmptyHomeMatchesReadTranscriptFile pins
// behaviour identical to ReadTranscriptFile for every session recorded
// before AgentHome existed — the empty-home case must not regress.
func TestReadTranscriptFileUnderHome_EmptyHomeMatchesReadTranscriptFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("external"), 0o600))

	got, err := agent.ReadTranscriptFileUnderHome(path, "")
	require.NoError(t, err)
	require.Equal(t, "external", string(got))
}

func TestReadTranscriptFileUnderHome_RejectsPathOutsideHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("external"), 0o600))

	_, err := agent.ReadTranscriptFileUnderHome(path, home)
	require.ErrorIs(t, err, agent.ErrOutsideSessionStore)
}

func TestReadTranscriptFileUnderHome_CanonicalPathWithLinkedHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("transcript"), 0o600))
	linkedHome := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, linkedHome); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	data, err := agent.ReadTranscriptFileUnderHome(path, linkedHome)
	require.NoError(t, err)
	require.Equal(t, "transcript", string(data))
	require.NoError(t, os.Remove(path))
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("private content"), 0o600))
	require.NoError(t, os.Symlink(secret, path))
	_, err = agent.ReadTranscriptFileUnderHome(path, linkedHome)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

// TestTranscriptReadableUnderHome_RejectsLinkBelowHome covers a relocated
// directory inside the home (for example ~/.claude/projects pointing at
// another disk). Confined reads refuse that link, so the home must not be
// recorded for such a transcript.
func TestTranscriptReadableUnderHome_RejectsLinkBelowHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	elsewhere := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, "proj"), 0o750))
	transcript := filepath.Join(elsewhere, "proj", "session.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte("{}\n"), 0o600))
	if err := os.Symlink(elsewhere, filepath.Join(home, "projects")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	linked := filepath.Join(home, "projects", "proj", "session.jsonl")
	require.False(t, agent.TranscriptReadableUnderHome(linked, home))
	require.False(t, agent.TranscriptReadableUnderHome(filepath.Join(home, "projects", "proj", "missing.jsonl"), home),
		"a missing transcript behind a linked directory is still unreadable")
}

func TestTranscriptReadableUnderHome_AcceptsRegularAndMissing(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	dir := filepath.Join(home, "projects", "proj")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	transcript := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte("{}\n"), 0o600))

	require.True(t, agent.TranscriptReadableUnderHome(transcript, home))
	require.True(t, agent.TranscriptReadableUnderHome(filepath.Join(dir, "later.jsonl"), home))
	require.False(t, agent.TranscriptReadableUnderHome(filepath.Join(t.TempDir(), "s.jsonl"), home))
	require.False(t, agent.TranscriptReadableUnderHome("", home))
}
