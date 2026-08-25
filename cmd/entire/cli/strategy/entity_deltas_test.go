package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests that install a producer stub, toggle the gate, or swap the spawn seam
// use t.Setenv / t.Chdir / package vars (process-global), so they cannot use
// t.Parallel(). The pure mapping tests at the bottom of the file can.

// producerDiffJSON is the REAL `entire-graph diff --json` output, captured from
// the producer binary — see testdata/entity_deltas_producer_diff.json for the
// commit it came from and why a hand-written fixture is not acceptable here.
func producerDiffJSON(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "entity_deltas_producer_diff.json"))
	require.NoError(t, err)
	return string(raw)
}

// producerFixtureFiles is every file the captured fixture reports a change in.
// Tests that are not about session scoping pass this so nothing is filtered.
var producerFixtureFiles = []string{
	"pkg/reloc/a.go",
	"pkg/reloc/b.go",
	"pkg/renamed/new.go",
	"pkg/renamed/old.go",
	"pkg/svc/handler.go",
}

// installEntityDeltasProducer puts a fake `entire-graph` at the front of $PATH.
// body is the shell body of the stub; it is what the CLI would exec for
// `entire graph diff …` (the plugin dispatcher strips the plugin name).
func installEntityDeltasProducer(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("producer stub is a POSIX shell script")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "entire-graph"), []byte("#!/bin/sh\n"+body), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// emitFixtureBody is a stub body that prints the captured producer output.
func emitFixtureBody(t *testing.T) string {
	t.Helper()
	return "cat <<'ENTIRE_EOF'\n" + producerDiffJSON(t) + "\nENTIRE_EOF\n"
}

// captureEntityDeltasJobs swaps the detached-spawn seam for a recorder and
// returns a pointer to the jobs the code under test scheduled. Production forks
// a real `entire __entity_deltas` child here; a test binary cannot.
func captureEntityDeltasJobs(t *testing.T) *[]entityDeltasJob {
	t.Helper()
	jobs := &[]entityDeltasJob{}
	original := entityDeltasSpawn
	entityDeltasSpawn = func(_, jobPath string) error {
		job, err := readEntityDeltasJob(jobPath)
		require.NoError(t, err, "scheduler must write a decodable job file")
		*jobs = append(*jobs, job)
		return nil
	}
	t.Cleanup(func() { entityDeltasSpawn = original })
	return jobs
}

// setupEntityDeltasRepo creates a repo with a base commit and a later HEAD
// commit, and returns the repo plus the base sha. The two commits must differ:
// entity deltas over an empty range are not computed.
func setupEntityDeltasRepo(t *testing.T) (dir string, repo *git.Repository, base string) {
	t.Helper()
	dir = setupGitRepo(t)
	t.Chdir(dir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	base = getHeadHash(t, repo)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("agent edit"), 0o600))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("agent change", &git.CommitOptions{})
	require.NoError(t, err)

	return dir, repo, base
}

// scheduleForTest runs the scheduler the way condensation does.
func scheduleForTest(t *testing.T, dir string, repo *git.Repository, base string, files []string) {
	t.Helper()
	scheduleEntityDeltas(context.Background(), context.Background(), repo,
		condenseOpts{repoDir: dir}, id.MustCheckpointID("e0e0e0e0e0e0"), "entity-deltas-session",
		base, files)
}

func TestScheduleEntityDeltas_DisabledIsNoOp(t *testing.T) {
	// A working producer is on PATH; the gate alone must keep it unused.
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	jobs := captureEntityDeltasJobs(t)

	scheduleForTest(t, dir, repo, base, []string{"test.txt"})
	assert.Empty(t, *jobs, "entity deltas must be off unless explicitly enabled")
}

// The env override is tri-state: an explicit 0 must beat an enabling setting.
func TestScheduleEntityDeltas_EnvDisableWins(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	writeEntityDeltasSetting(t, dir, true)
	jobs := captureEntityDeltasJobs(t)

	t.Setenv("ENTIRE_ENTITY_DELTAS", "0")
	scheduleForTest(t, dir, repo, base, []string{"test.txt"})
	assert.Empty(t, *jobs, "ENTIRE_ENTITY_DELTAS=0 must force the feature off")
}

func TestScheduleEntityDeltas_SchedulesJob(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	jobs := captureEntityDeltasJobs(t)

	scheduleForTest(t, dir, repo, base, []string{"test.txt", "other.go"})

	require.Len(t, *jobs, 1)
	job := (*jobs)[0]
	assert.Equal(t, id.MustCheckpointID("e0e0e0e0e0e0"), job.CheckpointID)
	assert.Equal(t, "entity-deltas-session", job.SessionID)
	assert.Equal(t, dir, job.RepoDir)
	assert.Equal(t, base, job.Base)
	assert.Equal(t, getHeadHash(t, repo), job.Head)
	assert.Equal(t, []string{"test.txt", "other.go"}, job.Files,
		"the session's own file set must travel with the job — it is what scopes the deltas")
}

// The job file is single-use: the child consumes it, so nothing accumulates in
// temp even across many condensations.
func TestScheduleEntityDeltas_JobFileIsConsumed(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")

	var jobPath string
	original := entityDeltasSpawn
	entityDeltasSpawn = func(_, p string) error { jobPath = p; return nil }
	t.Cleanup(func() { entityDeltasSpawn = original })

	scheduleForTest(t, dir, repo, base, []string{"test.txt"})
	require.NotEmpty(t, jobPath)
	require.FileExists(t, jobPath)

	_, err := readEntityDeltasJob(jobPath)
	require.NoError(t, err)
	assert.NoFileExists(t, jobPath, "reading the job must consume it")
}

func TestScheduleEntityDeltas_NoFilesSkips(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	jobs := captureEntityDeltasJobs(t)

	scheduleForTest(t, dir, repo, base, nil)
	assert.Empty(t, *jobs, "a session that touched no files cannot own an entity in the range")
}

func TestScheduleEntityDeltas_EmptyRangeSkips(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, _ := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	jobs := captureEntityDeltasJobs(t)

	scheduleForTest(t, dir, repo, getHeadHash(t, repo), []string{"test.txt"})
	assert.Empty(t, *jobs, "base == head has no range to diff")
}

// Defect this closes: every skip used to be Debug while the default level is
// Info, so "gate on, producer never installed" was silent forever.
func TestScheduleEntityDeltas_ProducerAbsentLogsOneInfoLine(t *testing.T) {
	dir, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	t.Setenv("PATH", t.TempDir()) // no entire-graph anywhere
	t.Setenv("ENTIRE_LOG_LEVEL", "info")
	jobs := captureEntityDeltasJobs(t)

	logger, err := logging.New(logging.Config{
		Dir:   filepath.Join(dir, logging.LogsDir),
		Level: slog.LevelInfo,
	})
	require.NoError(t, err)
	defer func() { _ = logger.Close() }()
	logCtx := logging.WithLogger(context.Background(), logger)

	scheduleEntityDeltas(context.Background(), logCtx, repo,
		condenseOpts{repoDir: dir}, id.MustCheckpointID("e0e0e0e0e0e0"), "entity-deltas-session",
		base, []string{"test.txt"})

	assert.Empty(t, *jobs, "a missing producer must not schedule work")

	raw, err := os.ReadFile(filepath.Join(dir, ".entire", "logs", "entire.log"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(raw), "entity deltas enabled but the producer is unavailable"),
		"exactly one Info line per condense, at the default level")
}

func TestComputeEntityDeltas_ProducerAbsentSkips(t *testing.T) {
	dir, _, base := setupEntityDeltasRepo(t)
	t.Setenv("PATH", t.TempDir())

	doc, err := computeEntityDeltas(context.Background(), dir, base, "head", nil)
	require.Error(t, err, "a missing producer must be an error the caller skips on")
	assert.Nil(t, doc)
}

func TestComputeEntityDeltas_ProducerErrorSkips(t *testing.T) {
	installEntityDeltasProducer(t, "echo 'boom' >&2\nexit 1\n")
	dir, _, base := setupEntityDeltasRepo(t)

	doc, err := computeEntityDeltas(context.Background(), dir, base, "head", nil)
	require.Error(t, err)
	assert.Nil(t, doc)
}

func TestComputeEntityDeltas_ProducerBadJSONSkips(t *testing.T) {
	installEntityDeltasProducer(t, "echo 'not json at all'\n")
	dir, _, base := setupEntityDeltasRepo(t)

	doc, err := computeEntityDeltas(context.Background(), dir, base, "head", nil)
	require.Error(t, err)
	assert.Nil(t, doc)
}

// The producer argv is a contract with an out-of-repo binary; assert it
// literally, so a reordering or a dropped flag fails here rather than in the
// field. The stub records what it was invoked with.
func TestComputeEntityDeltas_ProducerArgv(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	installEntityDeltasProducer(t, "printf '%s\\n' \"$@\" > "+argvFile+"\n"+emitFixtureBody(t))
	dir, _, base := setupEntityDeltasRepo(t)

	_, err := computeEntityDeltas(context.Background(), dir, base, "deadbeef", nil)
	require.NoError(t, err)

	raw, err := os.ReadFile(argvFile)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"diff", "--base", base, "--head", "deadbeef", "--repo", dir, "--json"},
		strings.Split(strings.TrimRight(string(raw), "\n"), "\n"))
}

// The backfill must never wait out a wedged producer: the subprocess is killed
// at the deadline. The assertion is tight against the deadline (200ms) rather
// than against the stub's sleep (10s) — a slack bound would pass even with the
// timeout removed entirely, which is exactly how this test used to be vacuous.
func TestComputeEntityDeltas_ProducerHangIsKilled(t *testing.T) {
	installEntityDeltasProducer(t, "sleep 10\n") // 50x the shortened deadline
	dir, _, base := setupEntityDeltasRepo(t)

	original := entityDeltasTimeout
	entityDeltasTimeout = 200 * time.Millisecond
	t.Cleanup(func() { entityDeltasTimeout = original })

	start := time.Now()
	_, err := computeEntityDeltas(context.Background(), dir, base, "head", nil)
	elapsed := time.Since(start)

	require.Error(t, err, "a hanging producer must be an error the caller skips on")
	assert.Contains(t, err.Error(), "timed out")
	assert.Less(t, elapsed, 2*time.Second,
		"the producer must be killed at the deadline, not waited out")
}

// Documents land on the checkpoint branch permanently, so an oversized producer
// run is dropped rather than stored.
func TestComputeEntityDeltas_OversizedOutputSkips(t *testing.T) {
	// Valid JSON, just too much of it: the cap must win before the decode.
	installEntityDeltasProducer(t, "printf '{\"files\":[]}'\nhead -c 4096 /dev/zero | tr '\\0' ' '\n")
	dir, _, base := setupEntityDeltasRepo(t)

	original := entityDeltasMaxOutputBytes
	entityDeltasMaxOutputBytes = 512
	t.Cleanup(func() { entityDeltasMaxOutputBytes = original })

	doc, err := computeEntityDeltas(context.Background(), dir, base, "head", nil)
	require.ErrorIs(t, err, errEntityDeltasOversized)
	assert.Contains(t, err.Error(), "more than 512 bytes")
	assert.Nil(t, doc)
}

func TestComputeEntityDeltas_UnderCapSucceeds(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, _, base := setupEntityDeltasRepo(t)

	original := entityDeltasMaxOutputBytes
	entityDeltasMaxOutputBytes = 1 << 20
	t.Cleanup(func() { entityDeltasMaxOutputBytes = original })

	doc, err := computeEntityDeltas(context.Background(), dir, base, "head",
		entityDeltasFileSet(producerFixtureFiles))
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Entities)
}

// condenseEntityDeltasSession runs a full condensation over base..HEAD and
// returns the resulting checkpoint tree.
func condenseEntityDeltasSession(t *testing.T, repo *git.Repository, base string, checkpointID id.CheckpointID) *object.Tree {
	t.Helper()

	state := &SessionState{
		SessionID:    "entity-deltas-session",
		AgentType:    "Claude Code",
		BaseCommit:   base,
		Phase:        session.PhaseActive,
		FilesTouched: []string{"test.txt"},
	}

	result, err := (&ManualCommitStrategy{}).CondenseSession(
		context.Background(), repo, checkpointID, state, map[string]struct{}{"test.txt": {}})
	require.NoError(t, err)
	require.False(t, result.Skipped, "session should have condensed")

	return metadataBranchTree(t, repo)
}

func metadataBranchTree(t *testing.T, repo *git.Repository) *object.Tree {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	return tree
}

func metadataBranchHash(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	return ref.Hash()
}

// writeEntityDeltasSetting turns the feature on (or off) through .entire/settings.json.
func writeEntityDeltasSetting(t *testing.T, dir string, enabled bool) {
	t.Helper()
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o750))
	raw, err := json.Marshal(map[string]any{"enabled": true, "entity_deltas": enabled})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, paths.SettingsFileName), raw, 0o600))
}

// The end-to-end contract: condensation schedules the work and returns; the
// detached runner attaches the deltas to the session's own directory inside the
// already-written checkpoint.
func TestCondenseAndBackfill_WritesEntityDeltasIntoCheckpointTree(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	_, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	jobs := captureEntityDeltasJobs(t)

	checkpointID := id.MustCheckpointID("e0e0e0e0e0e0")
	tree := condenseEntityDeltasSession(t, repo, base, checkpointID)

	// The hook path itself writes no deltas — it only schedules them.
	_, err := tree.File(checkpointID.Path() + "/0/" + paths.EntityDeltasFileName)
	require.Error(t, err, "the condense path must not write the deltas inline")
	require.Len(t, *jobs, 1)

	runEntityDeltasBackfill(context.Background(), (*jobs)[0])

	tree = metadataBranchTree(t, repo)
	file, err := tree.File(checkpointID.Path() + "/0/" + paths.EntityDeltasFileName)
	require.NoError(t, err, "entity_deltas.json must land in the session directory")

	contents, err := file.Contents()
	require.NoError(t, err)

	var doc entityDeltasDocument
	require.NoError(t, json.Unmarshal([]byte(contents), &doc))
	assert.Equal(t, "1.0", doc.SchemaVersion)
	assert.Equal(t, "entire-graph", doc.Producer)
	assert.Equal(t, "dev", doc.ProducerVersion, "the producer's real producer_version key must reach the document")
	assert.Equal(t, base, doc.Base)
	assert.Equal(t, getHeadHash(t, repo), doc.Head)
	assert.NotEmpty(t, doc.ComputedAt)

	// The session touched only test.txt, and the fixture's entities are all in
	// other files, so scoping leaves an empty (but present) list.
	assert.Empty(t, doc.Entities)

	// The session metadata is untouched by the backfill.
	_, err = tree.File(checkpointID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
}

// A backfill write that errors must leave the checkpoint byte-identical and
// must not surface as a failure. Structurally it cannot do otherwise: the
// backfill is a separate write onto an already-committed checkpoint.
func TestRunEntityDeltasBackfill_WriteErrorLeavesCheckpointIntact(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	jobs := captureEntityDeltasJobs(t)

	checkpointID := id.MustCheckpointID("e0e0e0e0e0e0")
	condenseEntityDeltasSession(t, repo, base, checkpointID)
	require.Len(t, *jobs, 1)
	before := metadataBranchHash(t, repo)

	// Retarget the job at a checkpoint that does not exist: the producer runs
	// and the document is built, then the store write fails.
	job := (*jobs)[0]
	job.CheckpointID = id.MustCheckpointID("ffffffffffff")

	runEntityDeltasBackfill(context.Background(), job)

	assert.Equal(t, before, metadataBranchHash(t, repo),
		"a failing backfill must not move the checkpoint branch")
	tree := metadataBranchTree(t, repo)
	_, err := tree.File(checkpointID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err, "the checkpoint itself must still be intact")

	// And the public entry point absorbs it all: it has no way to report a
	// failure, because nothing waits on this child's exit status.
	jobPath, err := writeEntityDeltasJob(job)
	require.NoError(t, err)
	RunEntityDeltasBackfill(context.Background(), jobPath)
	RunEntityDeltasBackfill(context.Background(), filepath.Join(dir, "no-such-job.json"))
	assert.Equal(t, before, metadataBranchHash(t, repo))
}

// With the gate on but no producer installed, the checkpoint is written exactly
// as it would have been — metadata present, no deltas file, no error.
func TestCondenseSession_ProducerAbsentLeavesCheckpointIntact(t *testing.T) {
	_, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	t.Setenv("PATH", t.TempDir())
	jobs := captureEntityDeltasJobs(t)

	checkpointID := id.MustCheckpointID("d0d0d0d0d0d0")
	tree := condenseEntityDeltasSession(t, repo, base, checkpointID)

	_, err := tree.File(checkpointID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err, "the checkpoint itself must still be written")

	_, err = tree.File(checkpointID.Path() + "/0/" + paths.EntityDeltasFileName)
	require.Error(t, err, "no deltas file when the producer is unavailable")
	assert.Empty(t, *jobs)
}

// ---------------------------------------------------------------------------
// Mapping: the frozen schema, asserted against the REAL producer's output.
// ---------------------------------------------------------------------------

func decodeProducerFixture(t *testing.T) graphDiffResult {
	t.Helper()
	var result graphDiffResult
	require.NoError(t, json.Unmarshal([]byte(producerDiffJSON(t)), &result))
	return result
}

func entityByName(t *testing.T, entities []entityDelta, name string) entityDelta {
	t.Helper()
	for _, e := range entities {
		if e.Name == name {
			return e
		}
	}
	require.FailNowf(t, "entity not found", "no entity named %q", name)
	return entityDelta{}
}

func TestEntityDeltasFromDiff_RealProducerOutput(t *testing.T) {
	t.Parallel()

	result := decodeProducerFixture(t)
	assert.Equal(t, "dev", result.ProducerVersion,
		"the producer's top-level key is producer_version, not version")

	entities := entityDeltasFromDiff(result, entityDeltasFileSet(producerFixtureFiles))
	require.Len(t, entities, 9)

	added := entityByName(t, entities, "NewHandler")
	assert.Equal(t, "added", added.Change)
	assert.Equal(t, "function", added.Kind)
	assert.Equal(t, "pkg/svc/handler.go", added.Path)
	assert.Equal(t, "func NewHandler() *Handler", added.Signature)
	assert.Equal(t, 5, added.StartLine)
	assert.Empty(t, added.OldName)
	assert.Empty(t, added.OldPath)

	// A removal carries only before_start_line; recording 0 loses the location.
	removed := entityByName(t, entities, "LegacyConfig")
	assert.Equal(t, "removed", removed.Change)
	assert.Equal(t, 5, removed.StartLine, "before_start_line must be used when after_start_line is absent")
	assert.Equal(t, "LegacyConfig struct { A int }", removed.OldSignature)

	bodyChanged := entityByName(t, entities, "Handler.Serve")
	assert.Equal(t, "modified", bodyChanged.Change, "body_changed folds into modified")
	assert.Equal(t, 9, bodyChanged.StartLine)

	sigChanged := entityByName(t, entities, "Parse")
	assert.Equal(t, "modified", sigChanged.Change, "signature_changed folds into modified")
	assert.Equal(t, "func Parse(s string)", sigChanged.OldSignature)
	assert.Equal(t, "func Parse(s string) error", sigChanged.Signature)
	assert.Equal(t, 14, sigChanged.StartLine, "after_start_line wins when present")

	renamed := entityByName(t, entities, "compute")
	assert.Equal(t, "renamed", renamed.Change)
	assert.Equal(t, "oldCompute", renamed.OldName)

	moved := entityByName(t, entities, "Relocated")
	assert.Equal(t, "moved", moved.Change)
	assert.Equal(t, "pkg/reloc/b.go", moved.Path, "new_path wins over the file path")
	assert.Equal(t, "pkg/reloc/a.go", moved.OldPath)
	assert.Empty(t, moved.OldName, "this one moved without being renamed")

	// One edit that both moved and renamed: old_name belongs to the change, not
	// to the change TYPE, so it must survive on a "moved".
	movedRenamed := entityByName(t, entities, "ShiftedRenamed")
	assert.Equal(t, "moved", movedRenamed.Change)
	assert.Equal(t, "Shifted", movedRenamed.OldName, "old_name must survive on a moved+renamed change")
	assert.Equal(t, "pkg/reloc/a.go", movedRenamed.OldPath)

	// The entity itself has no old_path, but its FILE was renamed — which
	// relocated it just the same.
	fileRenamed := entityByName(t, entities, "Inside")
	assert.Equal(t, "modified", fileRenamed.Change)
	assert.Equal(t, "pkg/renamed/new.go", fileRenamed.Path)
	assert.Equal(t, "pkg/renamed/old.go", fileRenamed.OldPath,
		"a renamed file's old_path must reach its entities")
}

// The producer diffs the whole base..head range, which in a multi-session (or
// human-edited) commit contains work this session did not do.
func TestEntityDeltasFromDiff_FiltersToSessionFiles(t *testing.T) {
	t.Parallel()

	result := decodeProducerFixture(t)

	only := entityDeltasFromDiff(result, entityDeltasFileSet([]string{"pkg/svc/handler.go"}))
	require.Len(t, only, 6, "only handler.go's entities belong to this session")
	for _, e := range only {
		assert.Equal(t, "pkg/svc/handler.go", e.Path)
	}

	none := entityDeltasFromDiff(result, entityDeltasFileSet([]string{"unrelated/file.go"}))
	assert.Empty(t, none)
	assert.NotNil(t, none, "entities must marshal as [] rather than null")

	// Either end of a move counts: the session that emptied a.go owns the
	// symbol that left it, even though it now lives in b.go.
	source := entityDeltasFromDiff(result, entityDeltasFileSet([]string{"pkg/reloc/a.go"}))
	require.Len(t, source, 2)
	for _, e := range source {
		assert.Equal(t, "pkg/reloc/a.go", e.OldPath)
	}
}

func TestEntityDeltasFromDiff_NeverNil(t *testing.T) {
	t.Parallel()

	entities := entityDeltasFromDiff(graphDiffResult{}, nil)
	require.NotNil(t, entities, "entities must marshal as [] rather than null")
	assert.Empty(t, entities)
}

func TestEntityDeltaFromChange_NameAndPathFallbacks(t *testing.T) {
	t.Parallel()

	got := entityDeltaFromChange(
		graphFileChange{Path: "pkg/a.go"},
		graphEntityChange{Type: "added", Kind: "function", Name: "Fn", AfterStartLine: 3})

	assert.Equal(t, "Fn", got.Name, "name is used when new_name is absent")
	assert.Equal(t, "pkg/a.go", got.Path, "the file path is used when new_path is absent")
	assert.Equal(t, 3, got.StartLine)
	assert.Empty(t, got.OldName)
	assert.Empty(t, got.OldPath)
}

// old_name and old_path answer "what/where was this before". Both are properties
// of the change carrying different previous values, NOT of the change type.
func TestEntityDeltaFromChange_OldFieldsFollowTheValues(t *testing.T) {
	t.Parallel()

	deltaFor := func(changeType string) entityDelta {
		return entityDeltaFromChange(graphFileChange{Path: "new/a.go"}, graphEntityChange{
			Type: changeType, Kind: "function", Name: "n",
			OldName: "was", OldPath: "old/a.go", NewPath: "new/a.go",
		})
	}

	for _, changeType := range []string{"renamed", "moved", "body_changed", "added", "removed"} {
		got := deltaFor(changeType)
		assert.Equal(t, "was", got.OldName, "old_name must survive change type %q", changeType)
		assert.Equal(t, "old/a.go", got.OldPath, "old_path must survive change type %q", changeType)
	}

	// A previous value equal to the current one carries no information.
	same := entityDeltaFromChange(graphFileChange{Path: "a.go", OldPath: "a.go"}, graphEntityChange{
		Type: "body_changed", Kind: "function", Name: "n", OldName: "n", OldPath: "a.go",
	})
	assert.Empty(t, same.OldName)
	assert.Empty(t, same.OldPath)
}

func TestEntityDeltaFromChange_StartLineFallsBackToBefore(t *testing.T) {
	t.Parallel()

	removed := entityDeltaFromChange(
		graphFileChange{Path: "pkg/a.go"},
		graphEntityChange{Type: "removed", Kind: "type", Name: "Gone", BeforeStartLine: 42})
	assert.Equal(t, 42, removed.StartLine, "a removal has only before_start_line")

	both := entityDeltaFromChange(
		graphFileChange{Path: "pkg/a.go"},
		graphEntityChange{Type: "body_changed", Kind: "function", Name: "F", BeforeStartLine: 7, AfterStartLine: 9})
	assert.Equal(t, 9, both.StartLine, "after_start_line wins when the entity still exists")
}

func TestEntityDeltaFromChange_FileOldPathReachesEntities(t *testing.T) {
	t.Parallel()

	got := entityDeltaFromChange(
		graphFileChange{Path: "pkg/new.go", OldPath: "pkg/old.go"},
		graphEntityChange{Type: "body_changed", Kind: "function", Name: "F", AfterStartLine: 3})
	assert.Equal(t, "pkg/old.go", got.OldPath, "renaming a file relocates every entity in it")

	// The entity's own old_path still wins when the producer emits one.
	got = entityDeltaFromChange(
		graphFileChange{Path: "pkg/new.go", OldPath: "pkg/old.go"},
		graphEntityChange{Type: "moved", Kind: "function", Name: "F", OldPath: "other/src.go"})
	assert.Equal(t, "other/src.go", got.OldPath)
}

func TestNormalizeEntityChange(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"added":             "added",
		"removed":           "removed",
		"renamed":           "renamed",
		"moved":             "moved",
		"body_changed":      "modified",
		"signature_changed": "modified",
		"changed":           "modified",
		"":                  "modified",
		"something_new":     "modified",
	}
	for in, want := range tests {
		assert.Equal(t, want, normalizeEntityChange(in), "normalizeEntityChange(%q)", in)
	}
}

func TestCappedBuffer(t *testing.T) {
	t.Parallel()

	b := &cappedBuffer{limit: 4}
	n, err := b.Write([]byte("ab"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.False(t, b.overflowed)

	n, err = b.Write([]byte("cd"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.False(t, b.overflowed, "filling the cap exactly is not an overflow")

	// A short write must still report the full length, so io.Copy does not
	// treat the cap as an error and abort the producer mid-run.
	n, err = b.Write([]byte("efgh"))
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.True(t, b.overflowed)
	assert.Equal(t, "abcd", string(b.Bytes()))
}

// ---------------------------------------------------------------------------
// Concurrency: the backfill child versus the pre-push OPF rewrite.
// ---------------------------------------------------------------------------

// probingOPF runs a probe in the middle of the rewrite — RedactBatch is called
// after the rewrite has read the v1 tip and before it compare-and-swaps, which
// is exactly the window a backfill must not land in.
type probingOPF struct {
	fakeOPFForRewrite

	probe func()
}

func (p *probingOPF) RedactBatch(ctx context.Context, inputs []string, cats []string) ([][]redact.Span, error) {
	if p.probe != nil {
		p.probe()
	}

	return p.fakeOPFForRewrite.RedactBatch(ctx, inputs, cats)
}

// shrinkEntityDeltasLockWaits makes both backfill-lock deadlines test-sized, so
// a test can observe a park without waiting minutes for it.
func shrinkEntityDeltasLockWaits(t *testing.T, wait time.Duration) {
	t.Helper()
	childWait, rewriteWait := entityDeltasLockWait, entityDeltasRewriteLockWait
	entityDeltasLockWait, entityDeltasRewriteLockWait = wait, wait
	t.Cleanup(func() {
		entityDeltasLockWait, entityDeltasRewriteLockWait = childWait, rewriteWait
	})
}

// The rewrite no longer holds the backfill lock for its whole duration — only
// for the CAS window at the end. A backfill may therefore land during the
// long OPF phase; the pre-CAS tip re-read turns that into V1RefMovedError so
// the user re-runs push rather than aborting mid-rewrite with a stale CAS.
func TestRewriteUnpushedV1WithOPF_BackfillDuringRewriteFailsCAS(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	_, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	shrinkEntityDeltasLockWaits(t, 200*time.Millisecond)
	jobs := captureEntityDeltasJobs(t)

	checkpointID := id.MustCheckpointID("e0e0e0e0e0e0")
	condenseEntityDeltasSession(t, repo, base, checkpointID)
	require.Len(t, *jobs, 1)
	// The child resolves the lock path from job.RepoDir and the rewrite from the
	// repo's worktree root; both normalize symlinks, so the two agree even when
	// (as on macOS temp dirs) those two strings differ.
	job := (*jobs)[0]
	require.NotEmpty(t, job.RepoDir)

	tipDuringRewrite := metadataBranchHash(t, repo)
	probed := false
	configureFakeOPF(t, &probingOPF{probe: func() {
		probed = true
		runEntityDeltasBackfill(context.Background(), job)
	}})

	_, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	require.Error(t, err)
	var moved *V1RefMovedError
	require.ErrorAs(t, err, &moved, "a backfill landing during OPF must surface at the CAS re-read")
	require.True(t, probed, "the probe must have run mid-rewrite")
	require.NotEqual(t, tipDuringRewrite, metadataBranchHash(t, repo),
		"the backfill must have advanced v1 during the unlocked OPF phase")

	tree := metadataBranchTree(t, repo)
	_, err = tree.File(checkpointID.Path() + "/0/" + paths.EntityDeltasFileName)
	require.NoError(t, err, "entity deltas from the mid-rewrite backfill must remain on v1")
}

// The lock is an optimization against a user-visible abort, not a correctness
// requirement — the compare-and-swap is. A rewrite that cannot take it must
// therefore behave exactly as it did before the lock existed.
func TestRewriteUnpushedV1WithOPF_HeldBackfillLockStillRewrites(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	dir, repo, originalTip := setupV1RepoInDir(t)
	shrinkEntityDeltasLockWaits(t, 100*time.Millisecond)

	lockPath, err := entityDeltasLockPath(context.Background(), dir)
	require.NoError(t, err)
	release, err := flock.Acquire(lockPath)
	require.NoError(t, err)
	t.Cleanup(release)

	newTip, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	require.NoError(t, err, "an unavailable lock must not fail the push")
	assert.NotEqual(t, originalTip, newTip, "the rewrite must still have run")
}

// ---------------------------------------------------------------------------
// Job file: malformed input and versioning.
// ---------------------------------------------------------------------------

// A job that cannot be decoded is still consumed: it is single-use either way,
// and leaving it behind would accumulate in temp forever.
func TestRunEntityDeltasBackfill_MalformedJobIsConsumedAndSkipped(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	_, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	captureEntityDeltasJobs(t)
	condenseEntityDeltasSession(t, repo, base, id.MustCheckpointID("e0e0e0e0e0e0"))
	before := metadataBranchHash(t, repo)

	jobPath := filepath.Join(t.TempDir(), "job.json")
	require.NoError(t, os.WriteFile(jobPath, []byte("{not json at all"), 0o600))

	RunEntityDeltasBackfill(context.Background(), jobPath)

	assert.NoFileExists(t, jobPath, "a malformed job must still be consumed")
	assert.Equal(t, before, metadataBranchHash(t, repo),
		"a malformed job must not touch the checkpoint branch")
}

// The writer and the reader are two different builds whenever an upgrade lands
// between the fork and the exec, so the child refuses a format it does not know
// rather than writing something wrong into a committed checkpoint. An absent
// version is the original unversioned job and still reads as v1.
func TestRunEntityDeltasBackfill_UnknownJobVersionIsRefused(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	_, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	jobs := captureEntityDeltasJobs(t)

	checkpointID := id.MustCheckpointID("e0e0e0e0e0e0")
	condenseEntityDeltasSession(t, repo, base, checkpointID)
	require.Len(t, *jobs, 1)
	assert.Equal(t, entityDeltasJobVersion, (*jobs)[0].V, "the scheduler must stamp the job version")

	before := metadataBranchHash(t, repo)
	future := (*jobs)[0]
	future.V = entityDeltasJobVersion + 1
	runEntityDeltasBackfill(context.Background(), future)
	assert.Equal(t, before, metadataBranchHash(t, repo),
		"a job from a newer format must not be acted on")

	legacy := (*jobs)[0]
	legacy.V = 0
	runEntityDeltasBackfill(context.Background(), legacy)
	tree := metadataBranchTree(t, repo)
	_, err := tree.File(checkpointID.Path() + "/0/" + paths.EntityDeltasFileName)
	require.NoError(t, err, "an unversioned job must still be read as v1")
}

// The job file belongs to the child. When there is no child, the scheduler owns
// the cleanup — the OS temp reaper is not a strategy for a file we already know
// nobody will read.
func TestScheduleEntityDeltas_SpawnFailureRemovesTheJobFile(t *testing.T) {
	installEntityDeltasProducer(t, emitFixtureBody(t))
	dir, repo, base := setupEntityDeltasRepo(t)
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")

	var jobPath string
	original := entityDeltasSpawn
	entityDeltasSpawn = func(_, p string) error {
		jobPath = p
		return errors.New("fork refused")
	}
	t.Cleanup(func() { entityDeltasSpawn = original })

	scheduleForTest(t, dir, repo, base, []string{"test.txt"})

	require.NotEmpty(t, jobPath, "the scheduler must have written a job file")
	assert.NoFileExists(t, jobPath, "a job whose child never started must not be left behind")
}
