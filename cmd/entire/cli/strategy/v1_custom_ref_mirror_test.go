package strategy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

type setReferenceErrorStorer struct {
	storage.Storer

	err error
}

func (s setReferenceErrorStorer) SetReference(*plumbing.Reference) error {
	return s.err
}

// setupV1CustomRefRepo creates an isolated repo with one commit, writes settings
// with the given checkpoints_version (empty string omits the option), chdirs
// in, and returns the open repo.
func setupV1CustomRefRepo(t *testing.T, version string) *git.Repository {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	body := `{"enabled": true}`
	if version != "" {
		body = `{"enabled": true, "strategy_options": {"checkpoints_version": ` + version + `}}`
	}
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(body), 0o644))

	t.Chdir(tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	return repo
}

// setV1MetadataBranch points the v1 metadata branch at HEAD and returns the hash.
func setV1MetadataBranch(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), head.Hash())))
	return head.Hash()
}

func v1CustomRefHash(t *testing.T, repo *git.Repository) (plumbing.Hash, bool) {
	t.Helper()
	ref, err := repo.Reference(plumbing.ReferenceName(paths.MetadataRefName), true)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return ref.Hash(), true
}

func localRefExists(t *testing.T, dir, refName string) bool {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	_, err = repo.Reference(plumbing.ReferenceName(refName), true)
	return err == nil
}

func v1MetadataBranchHash(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	return ref.Hash()
}

func enableV1CustomRefMirror(t *testing.T, dir string) {
	t.Helper()
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, paths.SettingsFileName),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_version": "1.1"}}`),
		0o644,
	))
}

func v1CustomRefsForTest() checkpoint.CommittedRefs {
	return checkpoint.CommittedRefs{
		Primary: plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		Read:    plumbing.ReferenceName(paths.MetadataRefName),
		Mirror:  plumbing.ReferenceName(paths.MetadataRefName),
	}
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_CreatesRef(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	v1Hash := setV1MetadataBranch(t, repo)

	require.NoError(t, MirrorCommittedMetadataRef(t.Context(), repo, v1CustomRefsForTest()))

	got, ok := v1CustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist", paths.MetadataRefName)
	assert.Equal(t, v1Hash, got)
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_DisabledNoOp(t *testing.T) {
	repo := setupV1CustomRefRepo(t, "") // v1 only
	setV1MetadataBranch(t, repo)

	require.NoError(t, MirrorCommittedMetadataRef(t.Context(), repo, checkpoint.CommittedRefs{
		Primary: plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		Read:    plumbing.NewBranchReferenceName(paths.MetadataBranchName),
	}))

	_, ok := v1CustomRefHash(t, repo)
	assert.False(t, ok, "v1 custom ref must not be created when not opted in")
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_AdvancesExistingRef(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	oldHash := setV1MetadataBranch(t, repo)
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), oldHash)))

	cwd, err := os.Getwd()
	require.NoError(t, err)
	testutil.WriteFile(t, cwd, "f2.txt", "more")
	testutil.GitAdd(t, cwd, "f2.txt")
	testutil.GitCommit(t, cwd, "second")
	newHash := setV1MetadataBranch(t, repo)
	require.NotEqual(t, oldHash, newHash)

	require.NoError(t, MirrorCommittedMetadataRef(t.Context(), repo, v1CustomRefsForTest()))

	got, ok := v1CustomRefHash(t, repo)
	require.True(t, ok)
	assert.Equal(t, newHash, got)
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_UsesProvidedRefs(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	head, err := repo.Head()
	require.NoError(t, err)

	primary := plumbing.NewBranchReferenceName("entire/checkpoints/custom")
	mirror := plumbing.ReferenceName("refs/entire/checkpoints/custom-read")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(primary, head.Hash())))

	require.NoError(t, MirrorCommittedMetadataRef(t.Context(), repo, checkpoint.CommittedRefs{
		Primary: primary,
		Read:    mirror,
		Mirror:  mirror,
	}))

	ref, err := repo.Reference(mirror, true)
	require.NoError(t, err)
	assert.Equal(t, head.Hash(), ref.Hash())
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_ReplacesLocallyAheadMirror(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	v1Hash := setV1MetadataBranch(t, repo)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	testutil.WriteFile(t, cwd, "f2.txt", "more")
	testutil.GitAdd(t, cwd, "f2.txt")
	testutil.GitCommit(t, cwd, "second")
	head, err := repo.Head()
	require.NoError(t, err)
	require.NotEqual(t, v1Hash, head.Hash())
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), head.Hash())))

	require.NoError(t, MirrorCommittedMetadataRef(t.Context(), repo, v1CustomRefsForTest()))

	got, ok := v1CustomRefHash(t, repo)
	require.True(t, ok)
	assert.Equal(t, v1Hash, got)
}

// Not parallel: uses t.Chdir().
func TestCondenseSession_MirrorsV1CustomRefWhenEnabled(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	enableV1CustomRefMirror(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-condense-v1-custom-ref"
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(t.Context(), sessionID)
	require.NoError(t, err)

	checkpointID := id.MustCheckpointID("aabbccdd1122")
	result, err := s.CondenseSession(t.Context(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	v1Hash := v1MetadataBranchHash(t, repo)
	got, ok := v1CustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist", paths.MetadataRefName)
	assert.Equal(t, v1Hash, got)
}

// Not parallel: uses t.Chdir().
func TestDeleteOrphanedCheckpoints_MirrorsV1CustomRefWhenEnabled(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	enableV1CustomRefMirror(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	setV1MetadataBranch(t, repo)
	v1HashBefore := v1MetadataBranchHash(t, repo)

	cpID := id.MustCheckpointID("aabbccdd1122")
	deleted, failed, err := DeleteOrphanedCheckpoints(t.Context(), []string{cpID.String()})
	require.NoError(t, err)
	require.Empty(t, failed)
	require.Equal(t, []string{cpID.String()}, deleted)

	v1HashAfter := v1MetadataBranchHash(t, repo)
	require.NotEqual(t, v1HashBefore, v1HashAfter, "cleanup must advance v1 with a new commit")

	mirrorHash, ok := v1CustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist", paths.MetadataRefName)
	assert.Equal(t, v1HashAfter, mirrorHash, "v1.1 mirror should track v1 after DeleteOrphanedCheckpoints")
}

// Not parallel: uses t.Chdir().
func TestFinalizeAllTurnCheckpoints_MirrorsV1CustomRefWhenEnabled(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	enableV1CustomRefMirror(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-finalize-v1-custom-ref"
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	state.TurnCheckpointIDs = nil
	require.NoError(t, s.saveSessionState(t.Context(), state))

	commitWithCheckpointTrailer(t, repo, dir, "bbccdd112233")
	require.NoError(t, s.PostCommit(t.Context()))

	state, err = s.loadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"bbccdd112233"}, state.TurnCheckpointIDs)

	beforeV1Hash := v1MetadataBranchHash(t, repo)
	beforeCustomHash, ok := v1CustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist after condensation", paths.MetadataRefName)
	require.Equal(t, beforeV1Hash, beforeCustomHash)

	fullTranscript := testTranscriptPromptResponse + `{"type":"human","message":{"content":"now test it"}}
{"type":"assistant","message":{"content":"tests pass"}}
`
	transcriptPath := filepath.Join(dir, ".entire", "metadata", sessionID, paths.TranscriptFileName)
	require.NoError(t, os.WriteFile(transcriptPath, []byte(fullTranscript), 0o644))
	state.TranscriptPath = transcriptPath

	require.NoError(t, s.HandleTurnEnd(t.Context(), state))

	afterV1Hash := v1MetadataBranchHash(t, repo)
	require.NotEqual(t, beforeV1Hash, afterV1Hash, "finalization should advance v1 metadata")
	afterCustomHash, ok := v1CustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist after finalization", paths.MetadataRefName)
	assert.Equal(t, afterV1Hash, afterCustomHash)
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_V1MissingErrors(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`) // no v1 metadata branch created

	err := MirrorCommittedMetadataRef(t.Context(), repo, v1CustomRefsForTest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), paths.MetadataBranchName)
	require.ErrorIs(t, err, ErrPrimaryMetadataMissing, "primary-missing must use the sentinel so callers can disambiguate from SetReference NotFound")

	_, ok := v1CustomRefHash(t, repo)
	assert.False(t, ok, "v1 custom ref must not be created when v1 metadata branch is absent")
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_SetReferenceNotFoundIsNotPrimaryMissing(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	setV1MetadataBranch(t, repo)
	repo.Storer = setReferenceErrorStorer{Storer: repo.Storer, err: plumbing.ErrReferenceNotFound}

	err := MirrorCommittedMetadataRef(t.Context(), repo, v1CustomRefsForTest())
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "SetReference NotFound should still unwrap to plumbing.ErrReferenceNotFound")
	require.NotErrorIs(t, err, ErrPrimaryMetadataMissing, "SetReference NotFound must not be mistaken for a missing primary")
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRef_SetReferenceErrorNamesTarget(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	v1Hash := setV1MetadataBranch(t, repo)
	storerErr := errors.New("set failed")
	repo.Storer = setReferenceErrorStorer{Storer: repo.Storer, err: storerErr}

	err := MirrorCommittedMetadataRef(t.Context(), repo, v1CustomRefsForTest())
	require.ErrorIs(t, err, storerErr)
	assert.Contains(t, err.Error(), paths.MetadataRefName)
	assert.Contains(t, err.Error(), v1Hash.String())
}

// Not parallel: uses t.Chdir().
func TestMirrorCommittedMetadataRefBestEffort_V1MissingNoOp(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`) // no v1 metadata branch created

	MirrorCommittedMetadataRefBestEffort(t.Context(), repo)

	_, ok := v1CustomRefHash(t, repo)
	assert.False(t, ok, "v1 custom ref must not be created when v1 metadata branch is absent")
}

// Not parallel: uses t.Chdir().
func TestUpdateCombinedAttribution_MirrorsV1CustomRefWhenEnabled(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	enableV1CustomRefMirror(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	// Two sessions overlapping the same commit make the checkpoint hold >1
	// session, which triggers combined-attribution persistence — a second v1
	// write that lands after per-session condensation has already mirrored.
	sessions := []struct{ id, file string }{
		{"combined-a", "a.txt"},
		{"combined-b", "b.txt"},
	}
	files := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		setupSessionWithCheckpointAndFile(t, s, dir, sess.id, sess.file)
		state, loadErr := s.loadSessionState(t.Context(), sess.id)
		require.NoError(t, loadErr)
		now := time.Now()
		state.Phase = session.PhaseEnded
		state.EndedAt = &now
		state.FilesTouched = []string{sess.file}
		require.NoError(t, s.saveSessionState(t.Context(), state))
		files = append(files, sess.file)
	}

	commitFilesWithTrailer(t, repo, dir, "ccddee112233", files...)
	require.NoError(t, s.PostCommit(t.Context()))

	v1Hash := v1MetadataBranchHash(t, repo)
	got, ok := v1CustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist", paths.MetadataRefName)
	assert.Equal(t, v1Hash, got, "custom ref must track v1 after combined-attribution write")
}

// TestPrePush_DoesNotPushV1CustomRef proves the phase-1 invariant: even with the
// mirror opted in and both refs present locally, pre-push pushes only the v1
// branch and never the v1 custom ref.
//
// Not parallel: uses t.Chdir().
func TestPrePush_DoesNotPushV1CustomRef(t *testing.T) {
	ctx := t.Context()
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	head := setV1MetadataBranch(t, repo)
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), head)))

	cwd, err := os.Getwd()
	require.NoError(t, err)
	bareDir := t.TempDir()
	runCheckpointRemoteGit(ctx, t, bareDir, "init", "--bare")
	runCheckpointRemoteGit(ctx, t, cwd, "remote", "add", "origin", bareDir)

	require.NoError(t, (&ManualCommitStrategy{}).PrePush(ctx, "origin"))

	assert.True(t, localRefExists(t, bareDir, "refs/heads/"+paths.MetadataBranchName),
		"v1 metadata branch should be pushed")
	assert.False(t, localRefExists(t, bareDir, paths.MetadataRefName),
		"v1 custom ref must never be pushed")
}
