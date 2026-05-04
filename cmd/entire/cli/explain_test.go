package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestNewExplainCmd(t *testing.T) {
	cmd := newExplainCmd()

	if cmd.Name() != "explain" {
		t.Errorf("expected command name to be 'explain', got %s", cmd.Name())
	}
	if cmd.Use != "explain [checkpoint-id | commit-sha]" {
		t.Errorf("expected Use %q, got %q", "explain [checkpoint-id | commit-sha]", cmd.Use)
	}

	// Verify flags exist
	sessionFlag := cmd.Flags().Lookup("session")
	if sessionFlag == nil {
		t.Error("expected --session flag to exist")
	}

	commitFlag := cmd.Flags().Lookup("commit")
	if commitFlag == nil {
		t.Error("expected --commit flag to exist")
	}

	generateFlag := cmd.Flags().Lookup("generate")
	if generateFlag == nil {
		t.Error("expected --generate flag to exist")
	}

	forceFlag := cmd.Flags().Lookup("force")
	if forceFlag == nil {
		t.Error("expected --force flag to exist")
	}
}

func TestExplainCmd_SearchAllFlag(t *testing.T) {
	cmd := newExplainCmd()

	// Verify --search-all flag exists
	flag := cmd.Flags().Lookup("search-all")
	require.NotNil(t, flag, "expected --search-all flag to exist")

	if flag.DefValue != "false" {
		t.Errorf("expected default value 'false', got %q", flag.DefValue)
	}
}

// rowsHaveValue searches rows for a value substring (in either Label or Value).
// Used by formatCheckpointSummaryError tests to assert that envelope text or
// hint phrasing surfaces somewhere in the structured rows.
func rowsHaveValue(rows []explainRow, want string) bool {
	for _, r := range rows {
		if strings.Contains(r.Value, want) || strings.Contains(r.Label, want) {
			return true
		}
	}
	return false
}

func TestFormatCheckpointSummaryError_Auth(t *testing.T) {
	t.Parallel()
	label, rows, err := formatCheckpointSummaryError(&claudecode.ClaudeError{Kind: claudecode.ClaudeErrorAuth, Message: "Invalid API key"}, 0)
	if !strings.Contains(strings.ToLower(label), "authentication failed") {
		t.Errorf("missing 'authentication failed' in label %q", label)
	}
	if !rowsHaveValue(rows, "Invalid API key") {
		t.Errorf("missing envelope message in rows: %+v", rows)
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
}

func TestFormatCheckpointSummaryError_RateLimit(t *testing.T) {
	t.Parallel()
	label, _, err := formatCheckpointSummaryError(&claudecode.ClaudeError{Kind: claudecode.ClaudeErrorRateLimit, Message: "429"}, 0)
	if !strings.Contains(label, "rate limit") {
		t.Errorf("missing rate-limit phrasing in label: %q", label)
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
}

func TestFormatCheckpointSummaryError_Config(t *testing.T) {
	t.Parallel()
	_, rows, err := formatCheckpointSummaryError(&claudecode.ClaudeError{Kind: claudecode.ClaudeErrorConfig, Message: "model not found"}, 0)
	if !rowsHaveValue(rows, "model not found") {
		t.Errorf("envelope message not surfaced in rows: %+v", rows)
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
}

func TestFormatCheckpointSummaryError_CLIMissing(t *testing.T) {
	t.Parallel()
	label, _, err := formatCheckpointSummaryError(&claudecode.ClaudeError{Kind: claudecode.ClaudeErrorCLIMissing}, 0)
	if !strings.Contains(label, "not installed") {
		t.Errorf("missing cli-missing phrasing in label: %q", label)
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
}

// TestFormatCheckpointSummaryError_TypedBranchesHandleEmptyMessage guards against
// the null-result-envelope regression: Claude can emit is_error:true with a real
// HTTP status (401/429/4xx) but result:null, producing a ClaudeError with Message="".
// The Auth/RateLimit/Config branches must not render a bare colon in label or rows.
func TestFormatCheckpointSummaryError_TypedBranchesHandleEmptyMessage(t *testing.T) {
	t.Parallel()
	kinds := []claudecode.ClaudeErrorKind{
		claudecode.ClaudeErrorAuth,
		claudecode.ClaudeErrorRateLimit,
		claudecode.ClaudeErrorConfig,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			label, rows, err := formatCheckpointSummaryError(&claudecode.ClaudeError{Kind: kind}, 0)
			if err == nil {
				t.Fatal("expected structured error")
			}
			// Label must not end with a bare colon (the classic regression of
			// rendering "...: " with nothing after it).
			if strings.HasSuffix(strings.TrimSpace(label), ":") {
				t.Errorf("label ends with bare colon: %q", label)
			}
			for _, r := range rows {
				if strings.HasSuffix(strings.TrimSpace(r.Value), ":") {
					t.Errorf("row value ends with bare colon: %q (full: %+v)", r.Value, rows)
				}
			}
		})
	}
}

func TestFormatCheckpointSummaryError_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	label, rows, err := formatCheckpointSummaryError(fmt.Errorf("wrapped: %w", context.DeadlineExceeded), 5*time.Minute)
	if !strings.Contains(label, "timed out") {
		t.Errorf("expected 'timed out' in label, got %q", label)
	}
	if !strings.Contains(label, "5m") {
		t.Errorf("expected '5m' in label, got %q", label)
	}
	if len(rows) == 0 {
		t.Fatal("expected rows for causes/try")
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
	if !strings.Contains(err.Error(), "safety deadline") {
		t.Errorf("expected 'safety deadline' in structured error, got %q", err)
	}
	// Negative guards against regressions:
	//   - Hardcoded "Claude" / "sonnet" / "Anthropic" would misdirect users of
	//     alternate summary providers (codex, gemini).
	combined := label + "\n" + err.Error()
	var combinedSb194 strings.Builder
	for _, r := range rows {
		combinedSb194.WriteString("\n" + r.Label + " " + r.Value)
	}
	combined += combinedSb194.String()
	for _, unwanted := range []string{"summary_timeout_seconds", "Claude", "sonnet", "Anthropic", "anthropic.com"} {
		if strings.Contains(combined, unwanted) {
			t.Errorf("unexpected %q in provider-neutral timeout message: %q", unwanted, combined)
		}
	}
}

func TestFormatCheckpointSummaryError_Canceled(t *testing.T) {
	t.Parallel()
	label, _, err := formatCheckpointSummaryError(fmt.Errorf("wrapped: %w", context.Canceled), 0)
	if !strings.Contains(label, "canceled") {
		t.Errorf("missing canceled in label: %q", label)
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
}

func TestFormatCheckpointSummaryError_Passthrough(t *testing.T) {
	t.Parallel()
	_, rows, err := formatCheckpointSummaryError(errors.New("something else"), 0)
	if err == nil {
		t.Fatal("expected structured error")
	}
	combined := err.Error()
	var combinedSb219 strings.Builder
	for _, r := range rows {
		combinedSb219.WriteString(" " + r.Value)
	}
	combined += combinedSb219.String()
	if !strings.Contains(combined, "something else") {
		t.Errorf("original error not preserved in structured error or rows: %q rows=%+v", err, rows)
	}
}

// TestFormatCheckpointSummaryError_Unknown covers the three branches of the
// default-case suffix builder. Guards against users seeing
// "Claude failed to generate the summary:" with nothing after the colon
// (the null-result and no-stderr-OOM scenarios).
func TestFormatCheckpointSummaryError_Unknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  *claudecode.ClaudeError
		want string // substring that must appear in the label or rows
	}{
		{"APIStatus when Message empty", &claudecode.ClaudeError{Kind: claudecode.ClaudeErrorUnknown, APIStatus: 500}, "500"},
		{"ExitCode when Message empty", &claudecode.ClaudeError{Kind: claudecode.ClaudeErrorUnknown, ExitCode: 137}, "137"},
		{"Negative ExitCode renders as abnormal, not -1", &claudecode.ClaudeError{Kind: claudecode.ClaudeErrorUnknown, ExitCode: -1}, "abnormal"},
		{"All-zero fields render a diagnostic sentinel, not empty", &claudecode.ClaudeError{Kind: claudecode.ClaudeErrorUnknown}, "no diagnostic detail"},
		{"Message takes precedence", &claudecode.ClaudeError{Kind: claudecode.ClaudeErrorUnknown, Message: "something weird"}, "something weird"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			label, rows, err := formatCheckpointSummaryError(tc.err, 0)
			if err == nil {
				t.Fatal("expected structured error")
			}
			if strings.HasSuffix(strings.TrimSpace(label), ":") {
				t.Errorf("label ends with bare colon: %q", label)
			}
			combined := label
			var combinedSb260 strings.Builder
			for _, r := range rows {
				combinedSb260.WriteString(" " + r.Value)
			}
			combined += combinedSb260.String()
			if !strings.Contains(combined, tc.want) {
				t.Errorf("missing %q in %q", tc.want, combined)
			}
		})
	}
}

// TestExplainCmd_PositionalArgConflictsWithFlags verifies that combining a
// positional target with --checkpoint, --commit, or --session is rejected.
// The bare-positional happy path (auto-resolution to a checkpoint ID or commit
// ref) is covered by the TestRunExplainAuto_* tests in this file.
func TestExplainCmd_PositionalArgConflictsWithFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"positional arg with --checkpoint", []string{"abc123", "--checkpoint", "def456"}},
		{"positional arg with -c", []string{"abc123", "-c", "def456"}},
		{"positional arg with --commit", []string{"abc123", "--commit", "HEAD"}},
		{"positional arg with --session", []string{"abc123", "--session", "sess-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newExplainCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error when combining positional arg with flags, got nil")
			}
			if !strings.Contains(err.Error(), "cannot combine positional argument") {
				t.Errorf("expected 'cannot combine positional argument' error, got: %v", err)
			}
		})
	}
}

// runExplainAutoTestRepo seeds a git repo and returns the initial commit's hash.
func runExplainAutoTestRepo(t *testing.T) (repo *git.Repository, initialCommit plumbing.Hash) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "seed.txt", "seed")
	testutil.GitAdd(t, tmpDir, "seed.txt")
	testutil.GitCommit(t, tmpDir, "seed commit")

	opened, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	head, err := opened.Head()
	require.NoError(t, err)
	return opened, head.Hash()
}

// TestRunExplainAuto_NoMatchReturnsCompositeError verifies that a target
// that's neither a checkpoint ID/prefix nor a resolvable git ref returns
// the composite "no checkpoint or commit found" error — proving the
// checkpoint-first → commit-fallback routing chains correctly all the way
// to the final error.
func TestRunExplainAuto_NoMatchReturnsCompositeError(t *testing.T) {
	runExplainAutoTestRepo(t)

	var out, errOut bytes.Buffer
	err := runExplainAuto(context.Background(), &out, &errOut, "abababababab", false, false, false, false, false, false, false)

	require.Error(t, err)
	require.ErrorContains(t, err, `no checkpoint or commit found matching "abababababab"`)
}

// TestRunExplainAuto_CommitRefWithCheckpointTrailer verifies that a commit
// SHA passed positionally falls through to commit resolution and delegates
// to the checkpoint path with the ID from the Entire-Checkpoint trailer.
func TestRunExplainAuto_CommitRefWithCheckpointTrailer(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()

	cpID := id.MustCheckpointID("deadbeefcafe")
	require.NoError(t, checkpoint.NewGitStore(repo).WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-auto",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	wt, err := repo.Worktree()
	require.NoError(t, err)
	tmpDir := wt.Filesystem.Root()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "feature.txt"), []byte("feature"), 0o644))
	_, err = wt.Add("feature.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit(trailers.AppendCheckpointTrailer("Implement feature", cpID.String()), &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	var out, errOut bytes.Buffer
	err = runExplainAuto(ctx, &out, &errOut, commitHash.String(), true, false, false, false, false, false, false)
	require.NoError(t, err)
	require.Contains(t, out.String(), cpID.String(), "expected checkpoint header resolved via trailer")
}

// TestRunExplainAuto_CommitWithoutTrailer covers the trailer-less commit
// dispatch: read-only modes print a friendly message and exit 0, while
// --generate / --raw-transcript must error so scripts can distinguish
// "done" from "didn't happen" (Cursor Bugbot finding on PR #990).
func TestRunExplainAuto_CommitWithoutTrailer(t *testing.T) {
	_, initial := runExplainAutoTestRepo(t)
	shortSHA := initial.String()[:7]

	tests := []struct {
		name        string
		rawTrans    bool
		generate    bool
		wantErr     bool
		wantContain string // substring required in err (if wantErr) or out (if !wantErr)
	}{
		{"read-only prints friendly message", false, false, false, "✗ No associated Entire checkpoint"},
		{"--generate errors", false, true, true, "cannot generate summary"},
		{"--raw-transcript errors", true, false, true, "cannot show raw transcript"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runExplainAuto(context.Background(), &out, &errOut, initial.String(), true, false, false, tc.rawTrans, tc.generate, false, false)
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantContain)
				require.ErrorContains(t, err, shortSHA)
			} else {
				require.NoError(t, err)
				require.Contains(t, out.String(), tc.wantContain)
				require.Contains(t, out.String(), shortSHA)
			}
		})
	}
}

// TestRunExplainCheckpoint_NotFoundSentinels verifies the typed-error
// contract runExplainAuto depends on: non-matching targets return an error
// wrapping checkpoint.ErrCheckpointNotFound (for errors.Is detection),
// regardless of --generate. The old code returned the temp-checkpoint
// sentinel speculatively for --generate, breaking fallback routing.
func TestRunExplainCheckpoint_NotFoundSentinels(t *testing.T) {
	runExplainAutoTestRepo(t)

	for _, generate := range []bool{false, true} {
		t.Run(fmt.Sprintf("generate=%v", generate), func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runExplainCheckpoint(context.Background(), &out, &errOut, "abababababab", false, false, false, false, generate, false, false)

			require.Error(t, err)
			require.ErrorIs(t, err, checkpoint.ErrCheckpointNotFound)
			require.NotErrorIs(t, err, errCannotGenerateTemporaryCheckpoint,
				"sentinel must not fire unless a real temp checkpoint was matched")
		})
	}
}

func writeTemporaryCheckpointForExplainTest(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	testFile := filepath.Join(tmpDir, "temp.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("initial content"), 0o644))
	_, err = wt.Add("temp.txt")
	require.NoError(t, err)
	initialCommit, err := wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	sessionID := "2026-01-27-temp-session"
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
	require.NoError(t, os.MkdirAll(metadataDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, paths.PromptFileName), []byte("temporary checkpoint prompt"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"type":"user","message":{"content":[{"type":"text","text":"temporary checkpoint"}]}}`+"\n"), 0o644))

	require.NoError(t, os.WriteFile(testFile, []byte("updated content"), 0o644))

	result, err := checkpoint.NewGitStore(repo).WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        initialCommit.String()[:7],
		ModifiedFiles:     []string{"temp.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "temporary checkpoint with code changes",
		AuthorName:        "Test",
		AuthorEmail:       "test@example.com",
		IsFirstCheckpoint: false,
	})
	require.NoError(t, err)
	require.False(t, result.Skipped)

	return result.CommitHash.String()
}

func TestRunExplainAuto_GenerateTemporaryCheckpointDoesNotFallBackToCommit(t *testing.T) {
	tempCheckpointSHA := writeTemporaryCheckpointForExplainTest(t)

	var out, errOut bytes.Buffer
	err := runExplainAuto(context.Background(), &out, &errOut, tempCheckpointSHA, true, false, false, false, true, false, false)

	require.Error(t, err)
	require.ErrorIs(t, err, errCannotGenerateTemporaryCheckpoint)
	require.NotErrorIs(t, err, checkpoint.ErrCheckpointNotFound)
	require.NotContains(t, err.Error(), "no Entire-Checkpoint trailer")
}

// TestRunExplainAuto_TemporaryCheckpointRendersIdentityBullet verifies the
// brand identity-bullet shape is used for temporary checkpoints, with the
// "after commit" affordance text in the summary block.
func TestRunExplainAuto_TemporaryCheckpointRendersIdentityBullet(t *testing.T) {
	tempCheckpointSHA := writeTemporaryCheckpointForExplainTest(t)
	shortID := tempCheckpointSHA[:7]

	var out, errOut bytes.Buffer
	// noPager=true to suppress the pager's terminal-only path so output lands
	// in the buffer; generate=false so we read (and don't try to summarize).
	err := runExplainAuto(context.Background(), &out, &errOut, tempCheckpointSHA, true, false, false, false, false, false, false)
	require.NoError(t, err)

	output := out.String()
	if !strings.Contains(output, fmt.Sprintf("● Checkpoint %s [temporary]", shortID)) {
		t.Errorf("expected '● Checkpoint %s [temporary]' identity bullet, got:\n%s", shortID, output)
	}
	if !strings.Contains(output, "## Summary") {
		t.Errorf("expected '## Summary' heading in temporary output, got:\n%s", output)
	}
	if !strings.Contains(output, "Temporary checkpoints can be summarized after commit") {
		t.Errorf("expected 'after commit' affordance in temporary output, got:\n%s", output)
	}
}

// collidingShaPrefix creates commits until two share a 2-char SHA prefix
// and returns that prefix. 2 chars is the smallest even-byte boundary
// HashesWithPrefix uses, so a collision at this length reliably exercises
// the ambiguity detection path without SHA mining.
func collidingShaPrefix(t *testing.T, repo *git.Repository, tmpDir string) string {
	t.Helper()
	wt, err := repo.Worktree()
	require.NoError(t, err)

	seen := make(map[string]int)
	for i := range 300 {
		testutil.WriteFile(t, tmpDir, "f.txt", fmt.Sprintf("content-%d", i))
		_, err = wt.Add("f.txt")
		require.NoError(t, err)
		h, err := wt.Commit(fmt.Sprintf("commit %d", i), &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "t@e.com", When: time.Now().Add(time.Duration(i) * time.Second)},
		})
		require.NoError(t, err)
		p := h.String()[:2]
		seen[p]++
		if seen[p] >= 2 {
			return p
		}
	}
	t.Skip("could not produce colliding 2-char SHA prefix in 300 iterations")
	return ""
}

// TestResolveCommitUnambiguous_MultipleCommitMatches verifies the reviewer-
// flagged bug: go-git v6's ResolveRevision silently returns the first
// candidate when a hex prefix matches multiple commits. With the helper
// wrapping it, ambiguity must surface as errAmbiguousCommitPrefix.
func TestResolveCommitUnambiguous_MultipleCommitMatches(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	prefix := collidingShaPrefix(t, repo, tmpDir)

	_, matches, err := resolveCommitUnambiguous(repo, prefix)
	require.Error(t, err)
	require.ErrorIs(t, err, errAmbiguousCommitPrefix)
	require.GreaterOrEqual(t, len(matches), 2, "expected ambiguous matches slice")
}

// TestRunExplainCommit_AmbiguousPrintsToErrWAndReturnsSilent verifies the
// ambiguous-prefix path: the styled failure block lands on errW, the
// returned error is a *SilentError (so main.go does not double-print),
// and stdout stays empty.
func TestRunExplainCommit_AmbiguousPrintsToErrWAndReturnsSilent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	prefix := collidingShaPrefix(t, repo, tmpDir)

	var out, errOut bytes.Buffer
	err = runExplainCommit(context.Background(), &out, &errOut, prefix, true, false, false, false, false, false, false)

	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("expected *SilentError, got %T: %v", err, err)
	}
	if !strings.Contains(errOut.String(), "✗ Ambiguous checkpoint prefix") {
		t.Errorf("missing styled failure on errW:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "matches") {
		t.Errorf("expected 'matches' row in errW:\n%s", errOut.String())
	}
	if out.String() != "" {
		t.Errorf("did not expect anything on stdout:\n%s", out.String())
	}
}

// TestRunExplainCheckpoint_AmbiguousCommittedPrefixPrintsToErrWAndReturnsSilent
// verifies that an ambiguous prefix matching multiple committed checkpoints
// renders the styled failure block to errW (not stdout) and returns a
// *SilentError so main.go does not double-print. Mirrors the commit-side
// ambiguity test.
func TestRunExplainCheckpoint_AmbiguousCommittedPrefixPrintsToErrWAndReturnsSilent(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()

	// Seed two committed checkpoints sharing a hex prefix.
	store := checkpoint.NewGitStore(repo)
	transcriptBytes := redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"))
	for _, cpID := range []id.CheckpointID{
		id.MustCheckpointID("e7aaaaaaaaaa"),
		id.MustCheckpointID("e7bbbbbbbbbb"),
	} {
		require.NoError(t, store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    "session-" + cpID.String(),
			Strategy:     "manual-commit",
			Transcript:   transcriptBytes,
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
		}))
	}

	var out, errOut bytes.Buffer
	err := runExplainCheckpoint(ctx, &out, &errOut, "e7", true, false, false, false, false, false, false)

	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("expected *SilentError, got %T: %v", err, err)
	}
	if !strings.Contains(errOut.String(), "✗ Ambiguous checkpoint prefix") {
		t.Errorf("missing styled failure on errW:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "matches") {
		t.Errorf("expected 'matches' row in errW:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "committed checkpoints") {
		t.Errorf("expected 'committed checkpoints' kind in errW:\n%s", errOut.String())
	}
	if out.String() != "" {
		t.Errorf("did not expect anything on stdout:\n%s", out.String())
	}
}

// TestResolveCommitUnambiguous_UniquePrefixSucceeds verifies a full SHA
// resolves to the expected hash without triggering ambiguity detection.
func TestResolveCommitUnambiguous_UniquePrefixSucceeds(t *testing.T) {
	_, initial := runExplainAutoTestRepo(t)
	repo, err := git.PlainOpen(".")
	require.NoError(t, err)

	got, matches, err := resolveCommitUnambiguous(repo, initial.String())
	require.NoError(t, err)
	require.Nil(t, matches, "no ambiguous matches expected")
	require.Equal(t, initial, got)
}

// TestAbbreviateCommitHash_GrowsOnCollision verifies the helper grows past
// the default 7 chars when necessary — matching git's --abbrev auto-growth.
// The same 2-char SHA collision we construct for resolution is enough to
// force abbreviation beyond 2 chars (though in practice 7 still tends to
// be unique for ~300 commits).
func TestAbbreviateCommitHash_GrowsOnCollision(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	prefix := collidingShaPrefix(t, repo, tmpDir)

	// Find a hash whose SHA starts with the colliding prefix.
	hashes := commitHashesWithPrefix(repo, prefix)
	require.GreaterOrEqual(t, len(hashes), 2)

	abbrev := abbreviateCommitHash(repo, hashes[0])
	require.True(t, strings.HasPrefix(hashes[0].String(), abbrev), "abbreviation must be a prefix of the full hash")
	require.GreaterOrEqual(t, len(abbrev), 7, "abbreviation must be at least git's default of 7 chars")
	require.LessOrEqual(t, len(abbrev), 40, "abbreviation cannot exceed full hash length")
}

// TestAbbreviateCommitHash_UsesSevenByDefault verifies the helper returns
// the 7-char default when there's no collision, matching git's behavior.
func TestAbbreviateCommitHash_UsesSevenByDefault(t *testing.T) {
	_, initial := runExplainAutoTestRepo(t)
	repo, err := git.PlainOpen(".")
	require.NoError(t, err)

	abbrev := abbreviateCommitHash(repo, initial)
	require.Equal(t, initial.String()[:7], abbrev)
}

// TestRunExplainAuto_GenerateAmbiguousPrefixRefused guards the Codex finding
// that a short positional arg matching both a committed-checkpoint prefix
// and a git revision must not silently write a summary to the wrong
// checkpoint. SHA mining isn't practical, so we construct the collision by
// picking a checkpoint ID that starts with the seed commit's abbreviation.
func TestRunExplainAuto_GenerateAmbiguousPrefixRefused(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)
	commitPrefix := head.Hash().String()[:7]
	collisionID := id.MustCheckpointID(commitPrefix + "aaaaa")

	require.NoError(t, checkpoint.NewGitStore(repo).WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: collisionID,
		SessionID:    "session-collision",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	var out, errOut bytes.Buffer
	err = runExplainAuto(ctx, &out, &errOut, commitPrefix, true, false, false, false, true, false, false)

	require.Error(t, err)
	require.ErrorContains(t, err, "ambiguous target")
	require.ErrorContains(t, err, "--commit")
	require.ErrorContains(t, err, "--checkpoint")
}

// TestExplainCmd_CommitFlagWithGenerateValidates verifies --commit +
// --generate passes flag validation (previously hasCheckpointTarget
// excluded commitFlag, so the explicit form couldn't invoke generate).
func TestExplainCmd_CommitFlagWithGenerateValidates(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "x")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "seed")

	cmd := newExplainCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--commit", "HEAD", "--generate"})

	// Command will fail downstream (no trailer on seed commit), but must
	// not fail at flag validation.
	if err := cmd.Execute(); err != nil {
		require.NotContains(t, err.Error(), "--generate requires")
	}
}

func TestGenerateCheckpointAISummary_AddsDefaultTimeoutWithoutParentDeadline(t *testing.T) {
	tmpTimeout := checkpointSummaryTimeout
	tmpGenerator := generateTranscriptSummary
	t.Cleanup(func() {
		checkpointSummaryTimeout = tmpTimeout
		generateTranscriptSummary = tmpGenerator
	})

	checkpointSummaryTimeout = 50 * time.Millisecond

	var gotDeadline time.Time
	generateTranscriptSummary = func(
		ctx context.Context,
		_ redact.RedactedBytes,
		_ []string,
		_ types.AgentType,
		_ summarize.Generator,
	) (*checkpoint.Summary, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("expected deadline on summary context")
		}
		gotDeadline = deadline
		return &checkpoint.Summary{Intent: "intent", Outcome: "outcome"}, nil
	}

	start := time.Now()
	summary, _, err := generateCheckpointAISummary(context.Background(), []byte("transcript"), nil, agent.AgentTypeClaudeCode, nil)
	if err != nil {
		t.Fatalf("generateCheckpointAISummary() error = %v", err)
	}
	if summary == nil {
		t.Fatal("expected summary")
	}
	if gotDeadline.IsZero() {
		t.Fatal("expected deadline to be set")
	}
	if remaining := gotDeadline.Sub(start); remaining < 30*time.Millisecond || remaining > 200*time.Millisecond {
		t.Fatalf("deadline offset = %s, want around %s", remaining, checkpointSummaryTimeout)
	}
}

func TestMaybeCompactExternalTranscriptForSummary_RedactsExternalOutput(t *testing.T) {
	// Cannot use t.Parallel() because external agent discovery mutates the
	// package-level agent registry and this test changes cwd/PATH.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"external_agents":true}`),
		0o644,
	))

	const (
		name   = "summary-redact"
		kind   = types.AgentType("Summary Redact Agent")
		secret = "q9Xv2Lm8Rt1Yp4Kd7Wz0Hs6Nc3Bf5Jg"
	)
	externalDir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  info)
    echo '{"protocol_version":1,"name":"` + name + `","type":"` + string(kind) + `","description":"External redaction test agent","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{"hooks":false,"transcript_analyzer":false,"transcript_preparer":false,"token_calculator":false,"compact_transcript":true,"text_generator":false,"hook_response_writer":false,"subagent_aware_extractor":false}}'
    ;;
  compact-transcript)
    echo '{"transcript":"eyJ2IjoxLCJhZ2VudCI6InN1bW1hcnktcmVkYWN0IiwiY2xpX3ZlcnNpb24iOiJ0ZXN0IiwidHlwZSI6InVzZXIiLCJ0cyI6IjIwMjYtMDEtMDFUMDA6MDA6MDBaIiwiY29udGVudCI6W3sidGV4dCI6ImtleT1xOVh2MkxtOFJ0MVlwNEtkN1d6MEhzNk5jM0JmNUpnIn1dfQo="}'
    ;;
  *)
    echo '{}'
    ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(externalDir, "entire-agent-"+name), []byte(script), 0o755))
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := maybeCompactExternalTranscriptForSummary(ctx, []byte("not-json"), kind)
	if strings.Contains(string(got), secret) {
		t.Fatalf("external compact transcript was not redacted: %s", got)
	}
	if !strings.Contains(string(got), redact.RedactedPlaceholder) {
		t.Fatalf("expected redacted compact transcript, got: %s", got)
	}
}

func TestGenerateCheckpointAISummary_UsesParentDeadlineAndWrapsSentinel(t *testing.T) {
	tmpTimeout := checkpointSummaryTimeout
	tmpGenerator := generateTranscriptSummary
	t.Cleanup(func() {
		checkpointSummaryTimeout = tmpTimeout
		generateTranscriptSummary = tmpGenerator
	})

	checkpointSummaryTimeout = 30 * time.Second

	parentCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	parentDeadline, _ := parentCtx.Deadline()

	var gotDeadline time.Time
	generateTranscriptSummary = func(
		ctx context.Context,
		_ redact.RedactedBytes,
		_ []string,
		_ types.AgentType,
		_ summarize.Generator,
	) (*checkpoint.Summary, error) {
		gotDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		return nil, ctx.Err()
	}

	_, appliedDeadline, err := generateCheckpointAISummary(parentCtx, []byte("transcript"), nil, agent.AgentTypeClaudeCode, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if gotDeadline.IsZero() {
		t.Fatal("expected deadline to be captured")
	}
	// The applied deadline must reflect the shorter parent-ctx deadline,
	// not the package-default checkpointSummaryTimeout. Otherwise
	// formatCheckpointSummaryError would report the wrong timeout to users.
	if appliedDeadline >= checkpointSummaryTimeout {
		t.Fatalf("appliedDeadline = %s; want shorter than %s (parent had tighter deadline)",
			appliedDeadline, checkpointSummaryTimeout)
	}
	if delta := gotDeadline.Sub(parentDeadline); delta < -5*time.Millisecond || delta > 5*time.Millisecond {
		t.Fatalf("deadline delta = %s, want near 0", delta)
	}
	if strings.Contains(err.Error(), "30s") {
		t.Fatalf("timeout error should not report default timeout when parent deadline fired: %v", err)
	}
}

// TestGenerateCheckpointAISummary_PreservesClaudeErrorWhenCtxIsDone guards
// against the race where the underlying summarizer returns a typed
// *ClaudeError AND the context happens to be done. Prior code checked
// timeoutCtx.Err() and unconditionally wrapped with %w context.DeadlineExceeded,
// which discarded the typed error and routed the user to the wrong
// "safety deadline" guidance instead of the auth/rate-limit message.
func TestGenerateCheckpointAISummary_PreservesClaudeErrorWhenCtxIsDone(t *testing.T) {
	tmpTimeout := checkpointSummaryTimeout
	tmpGenerator := generateTranscriptSummary
	t.Cleanup(func() {
		checkpointSummaryTimeout = tmpTimeout
		generateTranscriptSummary = tmpGenerator
	})

	checkpointSummaryTimeout = 30 * time.Second

	// Cancel the parent before we even call — ctx.Err() will be non-nil.
	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()

	claudeErr := &claudecode.ClaudeError{Kind: claudecode.ClaudeErrorAuth, Message: "Invalid API key"}
	generateTranscriptSummary = func(
		context.Context,
		redact.RedactedBytes,
		[]string,
		types.AgentType,
		summarize.Generator,
	) (*checkpoint.Summary, error) {
		return nil, claudeErr
	}

	_, _, err := generateCheckpointAISummary(parentCtx, []byte("transcript"), nil, agent.AgentTypeClaudeCode, nil)
	var ce *claudecode.ClaudeError
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As did not recover *ClaudeError; got %v", err)
	}
	if ce.Kind != claudecode.ClaudeErrorAuth {
		t.Errorf("Kind = %v; want auth", ce.Kind)
	}
}

func TestGenerateCheckpointAISummary_ClampsLongParentDeadlineToDefaultTimeout(t *testing.T) {
	tmpTimeout := checkpointSummaryTimeout
	tmpGenerator := generateTranscriptSummary
	t.Cleanup(func() {
		checkpointSummaryTimeout = tmpTimeout
		generateTranscriptSummary = tmpGenerator
	})

	checkpointSummaryTimeout = 50 * time.Millisecond

	parentCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var gotDeadline time.Time
	generateTranscriptSummary = func(
		ctx context.Context,
		_ redact.RedactedBytes,
		_ []string,
		_ types.AgentType,
		_ summarize.Generator,
	) (*checkpoint.Summary, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("expected deadline on summary context")
		}
		gotDeadline = deadline
		return &checkpoint.Summary{Intent: "intent", Outcome: "outcome"}, nil
	}

	start := time.Now()
	summary, _, err := generateCheckpointAISummary(parentCtx, []byte("transcript"), nil, agent.AgentTypeClaudeCode, nil)
	if err != nil {
		t.Fatalf("generateCheckpointAISummary() error = %v", err)
	}
	if summary == nil {
		t.Fatal("expected summary")
	}
	if gotDeadline.IsZero() {
		t.Fatal("expected deadline to be set")
	}
	if remaining := gotDeadline.Sub(start); remaining < 30*time.Millisecond || remaining > 200*time.Millisecond {
		t.Fatalf("deadline offset = %s, want around %s", remaining, checkpointSummaryTimeout)
	}
}

func TestGenerateCheckpointAISummary_UsesCancellationSentinel(t *testing.T) {
	tmpTimeout := checkpointSummaryTimeout
	tmpGenerator := generateTranscriptSummary
	t.Cleanup(func() {
		checkpointSummaryTimeout = tmpTimeout
		generateTranscriptSummary = tmpGenerator
	})

	parentCtx, cancel := context.WithCancel(context.Background())

	generateTranscriptSummary = func(
		ctx context.Context,
		_ redact.RedactedBytes,
		_ []string,
		_ types.AgentType,
		_ summarize.Generator,
	) (*checkpoint.Summary, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}

	_, _, err := generateCheckpointAISummary(parentCtx, []byte("transcript"), nil, agent.AgentTypeClaudeCode, nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Canceled, got %v", err)
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected cancellation message, got %v", err)
	}
}

func TestExplainCommit_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)

	var stdout bytes.Buffer
	err := runExplainCommit(context.Background(), &stdout, &stdout, "nonexistent", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error for nonexistent commit, got nil")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "resolve") {
		t.Errorf("expected 'not found' or 'resolve' in error, got: %v", err)
	}
}

func TestExplainCommit_NoEntireData(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create a commit without Entire metadata
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("regular commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainCommit(context.Background(), &stdout, &stdout, commitHash.String(), false, false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("runExplainCommit() should not error for non-Entire commits, got: %v", err)
	}

	output := stdout.String()

	// Should show message indicating no Entire checkpoint (new failure-block shape)
	if !strings.Contains(output, "✗ No associated Entire checkpoint") {
		t.Errorf("expected styled failure block on output, got: %s", output)
	}
	if !strings.Contains(output, "  reason") {
		t.Errorf("expected reason row, got: %s", output)
	}
	// Should mention the commit hash
	if !strings.Contains(output, commitHash.String()[:7]) {
		t.Errorf("expected output to contain short commit hash, got: %s", output)
	}
}

func TestExplainCommit_WithMetadataTrailerButNoCheckpoint(t *testing.T) {
	// Test that commits with Entire-Metadata trailer (but no Entire-Checkpoint)
	// now show "no checkpoint" message (new behavior)
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create session metadata directory first
	sessionID := "2025-12-09-test-session-xyz789"
	sessionDir := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	// Create prompt file
	promptContent := "Add new feature"
	if err := os.WriteFile(filepath.Join(sessionDir, paths.PromptFileName), []byte(promptContent), 0o644); err != nil {
		t.Fatalf("failed to create prompt file: %v", err)
	}

	// Create a commit with Entire-Metadata trailer (but NO Entire-Checkpoint)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("feature content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}

	// Commit with Entire-Metadata trailer (no Entire-Checkpoint)
	metadataDir := ".entire/metadata/" + sessionID
	commitMessage := trailers.FormatMetadata("Add new feature", metadataDir)
	commitHash, err := w.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainCommit(context.Background(), &stdout, &stdout, commitHash.String(), false, false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("runExplainCommit() error = %v", err)
	}

	output := stdout.String()

	// New behavior: should show "no checkpoint" failure block since there's no Entire-Checkpoint trailer
	if !strings.Contains(output, "✗ No associated Entire checkpoint") {
		t.Errorf("expected styled failure block, got: %s", output)
	}
	if !strings.Contains(output, "  reason") {
		t.Errorf("expected reason row, got: %s", output)
	}
	// Should mention the commit hash
	if !strings.Contains(output, commitHash.String()[:7]) {
		t.Errorf("expected output to contain short commit hash, got: %s", output)
	}
}

func TestExplainDefault_ShowsBranchView(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Create initial commit so HEAD exists (required for branch view)
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainDefault(context.Background(), &stdout, true) // noPager=true for test

	// Should NOT error - should show branch view
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	output := stdout.String()
	// Should show branch header (new metadata-row shape: "branch  <name>")
	if !strings.Contains(output, "branch  ") {
		t.Errorf("expected 'branch' row in output, got: %s", output)
	}
	// Should show checkpoints count (likely 0)
	if !strings.Contains(output, "checkpoints") {
		t.Errorf("expected 'checkpoints' row in output, got: %s", output)
	}
}

func TestExplainDefault_NoCheckpoints_ShowsHelpfulMessage(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Create initial commit so HEAD exists (required for branch view)
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory but no checkpoints
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainDefault(context.Background(), &stdout, true) // noPager=true for test

	// Should NOT error
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	output := stdout.String()
	// Should show checkpoints count as 0 (new metadata-row shape)
	if !strings.Contains(output, "checkpoints  0") {
		t.Errorf("expected 'checkpoints  0' in output, got: %s", output)
	}
	// Should show helpful message about checkpoints appearing after saves
	if !strings.Contains(output, "Checkpoints will appear") || !strings.Contains(output, "agent session") {
		t.Errorf("expected helpful message about checkpoints, got: %s", output)
	}
}

func TestExplainBothFlagsError(t *testing.T) {
	// Test that providing both --session and --commit returns an error
	var stdout, stderr bytes.Buffer
	err := runExplain(context.Background(), &stdout, &stderr, "session-id", "commit-sha", "", "", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error when both flags provided, got nil")
	}
	// Case-insensitive check for "cannot specify multiple"
	errLower := strings.ToLower(err.Error())
	if !strings.Contains(errLower, "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' in error, got: %v", err)
	}
}

func TestFormatSessionInfo(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-test-session-abc",
		Description: "Test description",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{
			{
				CheckpointID: "abc1234567890",
				Message:      "First checkpoint",
				Timestamp:    now.Add(-time.Hour),
			},
			{
				CheckpointID: "def0987654321",
				Message:      "Second checkpoint",
				Timestamp:    now,
			},
		},
	}

	// Create checkpoint details matching the session checkpoints
	checkpointDetails := []checkpointDetail{
		{
			Index:     1,
			ShortID:   "abc1234",
			Timestamp: now.Add(-time.Hour),
			Message:   "First checkpoint",
			Interactions: []interaction{{
				Prompt:    "Fix the bug",
				Responses: []string{"Fixed the bug in auth module"},
				Files:     []string{"auth.go"},
			}},
			Files: []string{"auth.go"},
		},
		{
			Index:     2,
			ShortID:   "def0987",
			Timestamp: now,
			Message:   "Second checkpoint",
			Interactions: []interaction{{
				Prompt:    "Add tests",
				Responses: []string{"Added unit tests"},
				Files:     []string{"auth_test.go"},
			}},
			Files: []string{"auth_test.go"},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Verify output contains expected sections
	if !strings.Contains(output, "Session:") {
		t.Error("expected output to contain 'Session:'")
	}
	if !strings.Contains(output, session.ID) {
		t.Error("expected output to contain session ID")
	}
	if !strings.Contains(output, "Strategy:") {
		t.Error("expected output to contain 'Strategy:'")
	}
	if !strings.Contains(output, "manual-commit") {
		t.Error("expected output to contain strategy name")
	}
	if !strings.Contains(output, "Checkpoints: 2") {
		t.Error("expected output to contain 'Checkpoints: 2'")
	}
	// Check checkpoint details
	if !strings.Contains(output, "Checkpoint 1") {
		t.Error("expected output to contain 'Checkpoint 1'")
	}
	if !strings.Contains(output, "## Prompt") {
		t.Error("expected output to contain '## Prompt'")
	}
	if !strings.Contains(output, "## Responses") {
		t.Error("expected output to contain '## Responses'")
	}
	if !strings.Contains(output, "Files Modified") {
		t.Error("expected output to contain 'Files Modified'")
	}
}

func TestFormatSessionInfo_WithSourceRef(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-test-session-abc",
		Description: "Test description",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{
			{
				CheckpointID: "abc1234567890",
				Message:      "First checkpoint",
				Timestamp:    now,
			},
		},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:     1,
			ShortID:   "abc1234",
			Timestamp: now,
			Message:   "First checkpoint",
		},
	}

	// Test with source ref provided
	sourceRef := "entire/metadata@abc123def456"
	output := formatSessionInfo(session, sourceRef, checkpointDetails)

	// Verify source ref is displayed
	if !strings.Contains(output, "Source Ref:") {
		t.Error("expected output to contain 'Source Ref:'")
	}
	if !strings.Contains(output, sourceRef) {
		t.Errorf("expected output to contain source ref %q, got:\n%s", sourceRef, output)
	}
}

// TestManualCommitStrategyCallable verifies that the strategy's methods are callable
func TestManualCommitStrategyCallable(t *testing.T) {
	s := strategy.NewManualCommitStrategy()

	// GetAdditionalSessions should exist and be callable
	_, err := s.GetAdditionalSessions(context.Background())
	if err != nil {
		t.Logf("GetAdditionalSessions returned error: %v", err)
	}
}

func TestFormatSessionInfo_CheckpointNumberingReversed(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-test-session",
		Strategy:    "manual-commit",
		StartTime:   now.Add(-2 * time.Hour),
		Checkpoints: []strategy.Checkpoint{}, // Not used for format test
	}

	// Simulate checkpoints coming in newest-first order from ListSessions
	// but numbered with oldest=1, newest=N
	checkpointDetails := []checkpointDetail{
		{
			Index:     3, // Newest checkpoint should have highest number
			ShortID:   "ccc3333",
			Timestamp: now,
			Message:   "Third (newest) checkpoint",
			Interactions: []interaction{{
				Prompt:    "Latest change",
				Responses: []string{},
			}},
		},
		{
			Index:     2,
			ShortID:   "bbb2222",
			Timestamp: now.Add(-time.Hour),
			Message:   "Second checkpoint",
			Interactions: []interaction{{
				Prompt:    "Middle change",
				Responses: []string{},
			}},
		},
		{
			Index:     1, // Oldest checkpoint should be #1
			ShortID:   "aaa1111",
			Timestamp: now.Add(-2 * time.Hour),
			Message:   "First (oldest) checkpoint",
			Interactions: []interaction{{
				Prompt:    "Initial change",
				Responses: []string{},
			}},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Verify checkpoint ordering in output
	// Checkpoint 3 should appear before Checkpoint 2 which should appear before Checkpoint 1
	idx3 := strings.Index(output, "Checkpoint 3")
	idx2 := strings.Index(output, "Checkpoint 2")
	idx1 := strings.Index(output, "Checkpoint 1")

	if idx3 == -1 || idx2 == -1 || idx1 == -1 {
		t.Fatalf("expected all checkpoints to be in output, got:\n%s", output)
	}

	// In the output, they should appear in the order they're in the slice (newest first)
	if idx3 > idx2 || idx2 > idx1 {
		t.Errorf("expected checkpoints to appear in order 3, 2, 1 in output (newest first), got positions: 3=%d, 2=%d, 1=%d", idx3, idx2, idx1)
	}

	// Verify the dates appear correctly
	if !strings.Contains(output, "Latest change") {
		t.Error("expected output to contain 'Latest change' prompt")
	}
	if !strings.Contains(output, "Initial change") {
		t.Error("expected output to contain 'Initial change' prompt")
	}
}

func TestFormatSessionInfo_EmptyCheckpoints(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-empty-session",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	output := formatSessionInfo(session, "", nil)

	if !strings.Contains(output, "Checkpoints: 0") {
		t.Errorf("expected output to contain 'Checkpoints: 0', got:\n%s", output)
	}
}

func TestFormatSessionInfo_CheckpointWithTaskMarker(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-task-session",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "abc1234",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Task checkpoint",
			Interactions: []interaction{{
				Prompt:    "Run tests",
				Responses: []string{},
			}},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	if !strings.Contains(output, "[Task]") {
		t.Errorf("expected output to contain '[Task]' marker, got:\n%s", output)
	}
}

func TestFormatSessionInfo_CheckpointWithDate(t *testing.T) {
	// Test that checkpoint headers include the full date
	timestamp := time.Date(2025, 12, 10, 14, 35, 0, 0, time.UTC)
	session := &strategy.Session{
		ID:          "2025-12-10-dated-session",
		Strategy:    "manual-commit",
		StartTime:   timestamp,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:     1,
			ShortID:   "abc1234",
			Timestamp: timestamp,
			Message:   "Test checkpoint",
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should contain "2025-12-10 14:35" in the checkpoint header
	if !strings.Contains(output, "2025-12-10 14:35") {
		t.Errorf("expected output to contain date '2025-12-10 14:35', got:\n%s", output)
	}
}

func TestFormatSessionInfo_ShowsMessageWhenNoInteractions(t *testing.T) {
	// Test that checkpoints without transcript content show the commit message
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-12-incremental-session",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	// Checkpoint with message but no interactions (like incremental checkpoints)
	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "abc1234",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Starting 'dev' agent: Implement feature X (toolu_01ABC)",
			Interactions:     []interaction{}, // Empty - no transcript available
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should show the commit message when there are no interactions
	if !strings.Contains(output, "Starting 'dev' agent: Implement feature X (toolu_01ABC)") {
		t.Errorf("expected output to contain commit message when no interactions, got:\n%s", output)
	}

	// Should NOT show "## Prompt" or "## Responses" sections since there are no interactions
	if strings.Contains(output, "## Prompt") {
		t.Errorf("expected output to NOT contain '## Prompt' when no interactions, got:\n%s", output)
	}
	if strings.Contains(output, "## Responses") {
		t.Errorf("expected output to NOT contain '## Responses' when no interactions, got:\n%s", output)
	}
}

func TestFormatSessionInfo_ShowsMessageAndFilesWhenNoInteractions(t *testing.T) {
	// Test that checkpoints without transcript but with files show both message and files
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-12-incremental-with-files",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "def5678",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Running tests for API endpoint (toolu_02DEF)",
			Interactions:     []interaction{}, // Empty - no transcript
			Files:            []string{"api/endpoint.go", "api/endpoint_test.go"},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should show the commit message
	if !strings.Contains(output, "Running tests for API endpoint (toolu_02DEF)") {
		t.Errorf("expected output to contain commit message, got:\n%s", output)
	}

	// Should also show the files
	if !strings.Contains(output, "Files Modified") {
		t.Errorf("expected output to contain 'Files Modified', got:\n%s", output)
	}
	if !strings.Contains(output, "api/endpoint.go") {
		t.Errorf("expected output to contain modified file, got:\n%s", output)
	}
}

func TestFormatSessionInfo_DoesNotShowMessageWhenHasInteractions(t *testing.T) {
	// Test that checkpoints WITH interactions don't show the message separately
	// (the interactions already contain the content)
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-12-full-checkpoint",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "ghi9012",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Completed 'dev' agent: Implement feature (toolu_03GHI)",
			Interactions: []interaction{
				{
					Prompt:    "Implement the feature",
					Responses: []string{"I've implemented the feature by..."},
					Files:     []string{"feature.go"},
				},
			},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should show the interaction content
	if !strings.Contains(output, "Implement the feature") {
		t.Errorf("expected output to contain prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "I've implemented the feature by") {
		t.Errorf("expected output to contain response, got:\n%s", output)
	}

	// The message should NOT appear as a separate line (it's redundant when we have interactions)
	// The output should contain ## Prompt and ## Responses for the interaction
	if !strings.Contains(output, "## Prompt") {
		t.Errorf("expected output to contain '## Prompt' when has interactions, got:\n%s", output)
	}
}

func TestExplainCmd_HasCheckpointFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("checkpoint")
	if flag == nil {
		t.Error("expected --checkpoint flag to exist")
	}
}

func TestExplainCmd_HasShortFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("short")
	if flag == nil {
		t.Fatal("expected --short flag to exist")
		return // unreachable but satisfies staticcheck
	}

	// Should have -s shorthand
	if flag.Shorthand != "s" {
		t.Errorf("expected -s shorthand, got %q", flag.Shorthand)
	}
}

func TestExplainCmd_HasFullFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("full")
	if flag == nil {
		t.Error("expected --full flag to exist")
	}
}

func TestExplainCmd_HasRawTranscriptFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("raw-transcript")
	if flag == nil {
		t.Error("expected --raw-transcript flag to exist")
	}
}

func TestRunExplain_MutualExclusivityError(t *testing.T) {
	var buf, errBuf bytes.Buffer

	// Providing both --session and --checkpoint should error
	err := runExplain(context.Background(), &buf, &errBuf, "session-id", "", "checkpoint-id", "", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error when multiple flags provided")
	}
	if !strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %v", err)
	}
}

func TestRunExplainCheckpoint_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with an initial commit (required for checkpoint lookup)
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(context.Background(), &buf, &errBuf, "nonexistent123", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error for nonexistent checkpoint")
	}
	if !strings.Contains(err.Error(), "checkpoint not found") {
		t.Errorf("expected 'checkpoint not found' error, got: %v", err)
	}
}

func TestRunExplainCheckpoint_V2OnlyCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := wt.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`), 0o644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("777777777777")
	ctx := context.Background()

	if err := v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello from v2"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("failed to write v2 checkpoint: %v", err)
	}

	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(context.Background(), &buf, &errBuf, "777777", false, false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("expected success for v2-only checkpoint, got error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "● Checkpoint 777777777777") {
		t.Fatalf("expected checkpoint header in output, got: %s", output)
	}
	if !strings.Contains(output, "session-v2") {
		t.Fatalf("expected v2 session ID in output, got: %s", output)
	}
}

func TestRunExplainCheckpoint_V2OnlyRawTranscript(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := wt.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`), 0o644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("888888888888")
	ctx := context.Background()

	if err := v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"raw from v2"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("failed to write v2 checkpoint: %v", err)
	}

	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(context.Background(), &buf, &errBuf, "888888", false, false, false, true, false, false, false)
	if err != nil {
		t.Fatalf("expected success for v2-only raw transcript, got error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "raw from v2") {
		t.Fatalf("expected v2 raw transcript in output, got: %s", output)
	}
}

func TestRunExplainCheckpoint_V2CheckpointRemoteFallbackResolvesRawTranscript(t *testing.T) {
	ctx := context.Background()

	emptyConfig := filepath.Join(t.TempDir(), "empty-git-config")
	require.NoError(t, os.WriteFile(emptyConfig, []byte(""), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", emptyConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", emptyConfig)

	checkpointDir := t.TempDir()
	testutil.InitRepo(t, checkpointDir)
	testutil.WriteFile(t, checkpointDir, "checkpoint.txt", "checkpoint")
	testutil.GitAdd(t, checkpointDir, "checkpoint.txt")
	testutil.GitCommit(t, checkpointDir, "checkpoint init")

	checkpointRepo, err := git.PlainOpen(checkpointDir)
	require.NoError(t, err)

	cpID := id.MustCheckpointID("121212121212")
	rawTranscript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"raw from checkpoint_remote"}]}}` + "\n")
	checkpointStore := checkpoint.NewV2GitStore(checkpointRepo, "origin")
	require.NoError(t, checkpointStore.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-checkpoint-remote",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(rawTranscript),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	localDir := t.TempDir()
	t.Chdir(localDir)

	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "local.txt", "local")
	testutil.GitAdd(t, localDir, "local.txt")
	testutil.GitCommit(t, localDir, "local init")

	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "git@github.com:user/source.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	sshScript := filepath.Join(t.TempDir(), "fake-ssh")
	require.NoError(t, os.WriteFile(sshScript, []byte(`#!/bin/bash
set -euo pipefail
cmd="${@: -1}"
case "$cmd" in
  *"user/source.git"*)
    echo "origin intentionally unavailable" >&2
    exit 1
    ;;
  *"org/checkpoints.git"*) repo="$CHECKPOINT_REPO" ;;
  *)
    echo "unexpected ssh command: $cmd" >&2
    exit 1
    ;;
esac
exec git-upload-pack "$repo"
`), 0o755))
	t.Setenv("GIT_SSH", sshScript)
	t.Setenv("CHECKPOINT_REPO", checkpointDir)

	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true, "checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(ctx, &buf, &errBuf, "121212", false, false, false, true, false, false, false)
	require.NoError(t, err)
	require.Contains(t, buf.String(), "raw from checkpoint_remote")
}

func TestRunExplainCheckpoint_V2UsesCompactTranscriptForIntent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := wt.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`), 0o644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("999999999999")
	ctx := context.Background()

	compactTranscript := []byte(
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"compact prompt text"}]}` + "\n" +
			`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"m1","content":[{"type":"text","text":"assistant reply"}]}` + "\n",
	)

	if err := v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-v2",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"raw prompt text"}]}}` + "\n")),
		CompactTranscript:         compactTranscript,
		AuthorName:                "Test",
		AuthorEmail:               "test@example.com",
		CheckpointTranscriptStart: 0,
	}); err != nil {
		t.Fatalf("failed to write v2 checkpoint: %v", err)
	}

	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(context.Background(), &buf, &errBuf, "999999", false, false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("expected success for v2 checkpoint, got error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "## Intent") {
		t.Fatalf("expected '## Intent' heading in no-color output, got: %s", output)
	}
	if !strings.Contains(output, "compact prompt text") {
		t.Fatalf("expected compact transcript to drive intent extraction, got: %s", output)
	}
}

func TestRunExplainCheckpoint_V2PreferredGenerateWritesBothStores(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("test"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o644,
	))

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("aabbccddeeff")
	ctx := context.Background()

	transcript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"generate test"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":"done"}}` + "\n")

	// Dual-write: checkpoint exists in both v1 and v2.
	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	// generate=true, force=true — should succeed by writing to both v1 and v2 stores.
	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(ctx, &buf, &errBuf, "aabbcc", false, false, false, false, true, true, false)
	// Generation requires an AI summarizer which isn't available in unit tests,
	// but the important thing is we don't get the old "only v1 checkpoints supported" error.
	if err != nil && strings.Contains(err.Error(), "summary updates are currently supported only for v1 checkpoints") {
		t.Fatalf("should not reject v2-resolved checkpoints for generation when v1 has the data: %v", err)
	}
}

func TestRunExplainCheckpoint_V2OnlyGenerateSucceedsViaV2Store(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("test"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o644,
	))

	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("f1f2f3f4f5f6")
	ctx := context.Background()

	transcript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"v2-only generate"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":"done"}}` + "\n")

	// Write to v2 only — no v1 checkpoint exists.
	require.NoError(t, v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2-only",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	// generate=true, force=true — should not fail with "failed to save summary"
	// because v2 store can persist even when v1 doesn't have the checkpoint.
	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(ctx, &buf, &errBuf, "f1f2f3", false, false, false, false, true, true, false)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "claude") || strings.Contains(errMsg, "executable file not found") {
			t.Skipf("skipping: summarizer unavailable in CI: %v", err)
		}
		require.NotContains(t, errMsg, "failed to save summary",
			"v2-only checkpoint should persist summary via v2 store")
	}
}

func TestRunExplainCheckpoint_V2FallsBackToFullWhenCompactMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("test"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o644,
	))

	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("e1e2e3e4e5e6")
	ctx := context.Background()

	rawTranscript := []byte(
		`{"type":"user","message":{"content":[{"type":"text","text":"raw fallback prompt"}]}}` + "\n" +
			`{"type":"assistant","message":{"content":"raw reply"}}` + "\n",
	)

	// Write checkpoint with raw transcript but NO compact transcript.
	require.NoError(t, v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-no-compact",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(rawTranscript),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	// Default explain (not --full) should fall back to /full/current transcript
	// when compact transcript is missing on /main.
	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(ctx, &buf, &errBuf, "e1e2e3", false, false, false, false, false, false, false)
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, "raw fallback prompt",
		"should use raw transcript from /full/current when compact is missing")
}

func TestRunExplainCheckpoint_V2CompactTranscriptNotUsedForGenerate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("test"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o644,
	))

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	cpID := id.MustCheckpointID("c0c1c2c3c4c5")
	ctx := context.Background()

	rawTranscript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"raw prompt for summarizer"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":"raw reply"}}` + "\n")
	compactTranscript := []byte(`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","content":[{"text":"compact prompt"}]}` + "\n")

	// Dual-write with compact transcript.
	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-compact",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(rawTranscript),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:      cpID,
		SessionID:         "session-compact",
		Strategy:          "manual-commit",
		Transcript:        redact.AlreadyRedacted(rawTranscript),
		CompactTranscript: compactTranscript,
		AuthorName:        "Test",
		AuthorEmail:       "test@example.com",
	}))

	// generate=true — should NOT fail with "no transcript content" which would
	// indicate the compact transcript was incorrectly fed to the summarizer.
	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(ctx, &buf, &errBuf, "c0c1c2", false, false, false, false, true, true, false)
	if err != nil && strings.Contains(err.Error(), "no transcript content for this checkpoint") {
		t.Fatalf("compact transcript should not be used for --generate; raw transcript should be used instead: %v", err)
	}
}

func TestListCommittedForExplain_MergesV1AndV2(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "f.txt"), []byte("x"), 0o644))
	_, err = wt.Add("f.txt")
	require.NoError(t, err)
	_, err = wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t.com", When: time.Now()},
	})
	require.NoError(t, err)

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	ctx := context.Background()

	transcript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")

	// Write a v1-only checkpoint (pre-v2 era).
	v1OnlyID := id.MustCheckpointID("aaa111222333")
	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: v1OnlyID,
		SessionID:    "session-v1-only",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "T",
		AuthorEmail:  "t@t.com",
	}))

	// Write a dual-write checkpoint (exists in both v1 and v2).
	dualID := id.MustCheckpointID("bbb444555666")
	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: dualID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "T",
		AuthorEmail:  "t@t.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: dualID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "T",
		AuthorEmail:  "t@t.com",
	}))

	// With v2 preferred: should return both the dual-write AND the v1-only checkpoint.
	results, err := listCommittedForExplain(ctx, v1Store, v2Store, true)
	require.NoError(t, err)

	foundIDs := make(map[id.CheckpointID]bool)
	for _, r := range results {
		foundIDs[r.CheckpointID] = true
	}
	require.True(t, foundIDs[v1OnlyID], "v1-only checkpoint should be visible when v2 is preferred")
	require.True(t, foundIDs[dualID], "dual-write checkpoint should be visible")

	// No duplicates: dual checkpoint should appear exactly once.
	dualCount := 0
	for _, r := range results {
		if r.CheckpointID == dualID {
			dualCount++
		}
	}
	require.Equal(t, 1, dualCount, "dual-write checkpoint should not be duplicated")
}

func TestListCommittedForExplain_V2Disabled_ReturnsV1Only(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "f.txt"), []byte("x"), 0o644))
	_, err = wt.Add("f.txt")
	require.NoError(t, err)
	_, err = wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t.com", When: time.Now()},
	})
	require.NoError(t, err)

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	ctx := context.Background()

	transcript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")

	v1ID := id.MustCheckpointID("ccc777888999")
	require.NoError(t, v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: v1ID,
		SessionID:    "session-v1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "T",
		AuthorEmail:  "t@t.com",
	}))

	// v2 also has a checkpoint, but v2 is disabled — should only see v1.
	v2ID := id.MustCheckpointID("ddd000111222")
	require.NoError(t, v2Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: v2ID,
		SessionID:    "session-v2",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		AuthorName:   "T",
		AuthorEmail:  "t@t.com",
	}))

	results, err := listCommittedForExplain(ctx, v1Store, v2Store, false)
	require.NoError(t, err)

	foundIDs := make(map[id.CheckpointID]bool)
	for _, r := range results {
		foundIDs[r.CheckpointID] = true
	}
	require.True(t, foundIDs[v1ID], "v1 checkpoint should be returned")
	require.False(t, foundIDs[v2ID], "v2-only checkpoint should NOT appear when v2 is disabled")
}

func TestFormatCheckpointOutput_Short(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 3,
		FilesTouched:     []string{"main.go", "util.go"},
		TokenUsage: &agent.TokenUsage{
			InputTokens:  10000,
			OutputTokens: 5000,
		},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go", "util.go"},
			CheckpointsCount: 3,
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts: "Add a new feature",
	}

	// Default mode: empty commit message (not shown anyway in default mode)
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, false, &bytes.Buffer{})

	// Should show checkpoint ID
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Should show session ID
	if !strings.Contains(output, "2026-01-21-test-session") {
		t.Error("expected session ID in output")
	}
	// Should show timestamp
	if !strings.Contains(output, "2026-01-21") {
		t.Error("expected timestamp in output")
	}
	// Should show token usage (10000 + 5000 = 15000), formatted compactly.
	if !strings.Contains(output, "  tokens   15k") {
		t.Error("expected token count in output")
	}
	// Should show Intent heading (markdown body)
	if !strings.Contains(output, "## Intent") {
		t.Errorf("expected '## Intent' heading in no-color output, got:\n%s", output)
	}
	// Should show Summary heading with --generate hint affordance
	if !strings.Contains(output, "## Summary") {
		t.Errorf("expected '## Summary' heading in no-color output, got:\n%s", output)
	}
	if !strings.Contains(output, "entire explain --generate") {
		t.Errorf("expected --generate hint in summary affordance, got:\n%s", output)
	}
	// Should NOT show full file list in default mode
	if strings.Contains(output, "main.go") {
		t.Error("default output should not show file list (use --full)")
	}
}

func TestFormatCheckpointOutput_Verbose(t *testing.T) {
	// Transcript with user prompts that match what we expect to see
	transcriptContent := []byte(`{"type":"user","uuid":"u1","message":{"content":"Add a new feature"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"I'll add the feature"}]}}
{"type":"user","uuid":"u2","message":{"content":"Fix the bug"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Fixed it"}]}}
{"type":"user","uuid":"u3","message":{"content":"Refactor the code"}}
`)

	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 3,
		FilesTouched:     []string{"main.go", "util.go", "config.yaml"},
		TokenUsage: &agent.TokenUsage{
			InputTokens:  10000,
			OutputTokens: 5000,
		},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-21-test-session",
			CreatedAt:                 time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go", "util.go", "config.yaml"},
			CheckpointsCount:          3,
			CheckpointTranscriptStart: 0, // All content is this checkpoint's
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts:    "Add a new feature\nFix the bug\nRefactor the code",
		Transcript: transcriptContent,
	}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false, &bytes.Buffer{})

	// Should show checkpoint ID (like default)
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Should show session ID (like default)
	if !strings.Contains(output, "2026-01-21-test-session") {
		t.Error("expected session ID in output")
	}
	// Verbose should show files (with backticks in markdown list items)
	if !strings.Contains(output, "`main.go`") {
		t.Error("verbose output should show files")
	}
	if !strings.Contains(output, "`util.go`") {
		t.Error("verbose output should show all files")
	}
	if !strings.Contains(output, "`config.yaml`") {
		t.Error("verbose output should show all files")
	}
	// Should show "## Files (N)" markdown heading
	if !strings.Contains(output, "## Files (3)") {
		t.Errorf("verbose output should have '## Files (3)' heading, got:\n%s", output)
	}
	// Verbose should show scoped transcript section
	if !strings.Contains(output, "Transcript (checkpoint scope)") {
		t.Error("verbose output should have Transcript (checkpoint scope) section")
	}
	if !strings.Contains(output, "Add a new feature") {
		t.Error("verbose output should show prompts")
	}
}

func TestFormatCheckpointOutput_Verbose_NoCommitMessage(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 1,
		FilesTouched:     []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go"},
			CheckpointsCount: 1,
		},
		Prompts: "Add a feature",
	}

	// When commit message is empty, should not show Commit section
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false, &bytes.Buffer{})

	if strings.Contains(output, "  commits") {
		t.Error("verbose output should not show Commits section when nil (not searched)")
	}
}

func TestFormatCheckpointOutput_Full(t *testing.T) {
	// Use proper transcript format that matches actual Claude transcripts
	transcriptData := `{"type":"user","message":{"content":"Add a new feature"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll add that feature for you."}]}}`

	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 3,
		FilesTouched:     []string{"main.go", "util.go"},
		TokenUsage: &agent.TokenUsage{
			InputTokens:  10000,
			OutputTokens: 5000,
		},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go", "util.go"},
			CheckpointsCount: 3,
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts:    "Add a new feature",
		Transcript: []byte(transcriptData),
	}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, true, &bytes.Buffer{})

	// Should show checkpoint ID (like default)
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Full should also include verbose sections (## Files heading)
	if !strings.Contains(output, "## Files (2)") {
		t.Errorf("full output should include '## Files (2)' heading, got:\n%s", output)
	}
	// Full shows full session transcript (not scoped)
	if !strings.Contains(output, "Transcript (full session)") {
		t.Error("full output should have Transcript (full session) section")
	}
	// Should contain actual transcript content (parsed format)
	if !strings.Contains(output, "Add a new feature") {
		t.Error("full output should show transcript content")
	}
	if !strings.Contains(output, "[Assistant]") {
		t.Error("full output should show assistant messages in parsed transcript")
	}
}

func TestFormatCheckpointOutput_WithSummary(t *testing.T) {
	cpID := id.MustCheckpointID("abc123456789")
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: cpID,
		FilesTouched: []string{"file1.go", "file2.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID: cpID,
			SessionID:    "2026-01-22-test-session",
			CreatedAt:    time.Date(2026, 1, 22, 10, 30, 0, 0, time.UTC),
			FilesTouched: []string{"file1.go", "file2.go"},
			Summary: &checkpoint.Summary{
				Intent:  "Implement user authentication",
				Outcome: "Added login and logout functionality",
				Learnings: checkpoint.LearningsSummary{
					Repo:     []string{"Uses JWT for auth tokens"},
					Code:     []checkpoint.CodeLearning{{Path: "auth.go", Line: 42, Finding: "Token validation happens here"}},
					Workflow: []string{"Always run tests after auth changes"},
				},
				Friction:  []string{"Had to refactor session handling"},
				OpenItems: []string{"Add password reset flow"},
			},
		},
		Prompts: "Add user authentication",
	}

	// Test default output (non-verbose) with summary
	output := formatCheckpointOutput(summary, content, cpID, nil, checkpoint.Author{}, false, false, &bytes.Buffer{})

	// Should show AI-generated intent and outcome as markdown.
	if !strings.Contains(output, "## Intent\n\nImplement user authentication") {
		t.Errorf("expected AI intent in output, got:\n%s", output)
	}
	if !strings.Contains(output, "## Outcome\n\nAdded login and logout functionality") {
		t.Errorf("expected AI outcome in output, got:\n%s", output)
	}
	// Summary markdown includes all generated summary sections.
	if !strings.Contains(output, "## Learnings") {
		t.Errorf("summary output should show learnings, got:\n%s", output)
	}

	// Test verbose output with summary
	verboseOutput := formatCheckpointOutput(summary, content, cpID, nil, checkpoint.Author{}, true, false, &bytes.Buffer{})

	// Verbose should show learnings sections
	if !strings.Contains(verboseOutput, "## Learnings") {
		t.Errorf("verbose output should show learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "### Repository") {
		t.Errorf("verbose output should show repository learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Uses JWT for auth tokens") {
		t.Errorf("verbose output should show repo learning content, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "### Code") {
		t.Errorf("verbose output should show code learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "`auth.go:42`") {
		t.Errorf("verbose output should show code learning with line number, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "### Workflow") {
		t.Errorf("verbose output should show workflow learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "## Friction") {
		t.Errorf("verbose output should show friction, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "## Open Items") {
		t.Errorf("verbose output should show open items, got:\n%s", verboseOutput)
	}
}

func TestFormatCheckpointOutput_SummaryStartsAfterTightHeaderRule(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123456789")
	summary := &checkpoint.CheckpointSummary{CheckpointID: cpID}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID: cpID,
			SessionID:    "2026-01-22-test-session",
			CreatedAt:    time.Date(2026, 1, 22, 10, 30, 0, 0, time.UTC),
			Summary: &checkpoint.Summary{
				Intent:  "Implement user authentication",
				Outcome: "Added login and logout functionality",
			},
		},
	}

	output := formatCheckpointOutput(summary, content, cpID, nil, checkpoint.Author{}, false, false, &bytes.Buffer{})
	rule := strings.Repeat("─", 60)
	want := "  created  2026-01-22 10:30:00\n" + rule + "\n## Intent"

	if !strings.Contains(output, want) {
		t.Fatalf("expected summary to start immediately after header rule, got:\n%s", output)
	}
}

func TestBuildSummaryMarkdown_FullSummary(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.Summary{
		Intent:  "Rotate session tokens on logout",
		Outcome: "Logout now mints a new token",
		Learnings: checkpoint.LearningsSummary{
			Repo: []string{"Auth lives behind the auth_v2 gate"},
			Code: []checkpoint.CodeLearning{
				{Path: "auth/session.go", Line: 42, Finding: "Rotate before cookie clear"},
			},
			Workflow: []string{"Manual curl confirmed the path"},
		},
		Friction:  []string{"go-git v5 reset deleted .entire"},
		OpenItems: []string{"Backfill rotation for legacy cookies"},
	}

	got := buildSummaryMarkdown(summary)

	want := "## Intent\n\n" +
		"Rotate session tokens on logout\n\n" +
		"## Outcome\n\n" +
		"Logout now mints a new token\n\n" +
		"## Learnings\n\n" +
		"### Repository\n\n" +
		"- Auth lives behind the auth_v2 gate\n\n" +
		"### Code\n\n" +
		"- `auth/session.go:42` — Rotate before cookie clear\n\n" +
		"### Workflow\n\n" +
		"- Manual curl confirmed the path\n\n" +
		"## Friction\n\n" +
		"- go-git v5 reset deleted .entire\n\n" +
		"## Open Items\n\n" +
		"- Backfill rotation for legacy cookies\n"

	if got != want {
		t.Errorf("buildSummaryMarkdown mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildSummaryMarkdown_NoLearnings(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.Summary{
		Intent:  "Trivial fix",
		Outcome: "Fixed",
	}

	got := buildSummaryMarkdown(summary)

	if strings.Contains(got, "## Learnings") {
		t.Errorf("expected no Learnings heading when all subsections empty, got:\n%s", got)
	}
	if !strings.Contains(got, "## Intent\n\nTrivial fix\n\n") {
		t.Errorf("expected Intent block, got:\n%s", got)
	}
	if !strings.Contains(got, "## Outcome\n\nFixed\n") {
		t.Errorf("expected Outcome block, got:\n%s", got)
	}
}

func TestBuildSummaryMarkdown_PartialLearnings(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.Summary{
		Intent:  "i",
		Outcome: "o",
		Learnings: checkpoint.LearningsSummary{
			Code: []checkpoint.CodeLearning{
				{Path: "a.go", Finding: "x"},
			},
		},
	}

	got := buildSummaryMarkdown(summary)

	if !strings.Contains(got, "## Learnings") {
		t.Errorf("expected Learnings heading when Code populated, got:\n%s", got)
	}
	if !strings.Contains(got, "### Code") {
		t.Errorf("expected Code subsection, got:\n%s", got)
	}
	if strings.Contains(got, "### Repository") {
		t.Errorf("did not expect Repository subsection, got:\n%s", got)
	}
	if strings.Contains(got, "### Workflow") {
		t.Errorf("did not expect Workflow subsection, got:\n%s", got)
	}
}

func TestBuildSummaryMarkdown_CodeLineVariants(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.Summary{
		Intent:  "i",
		Outcome: "o",
		Learnings: checkpoint.LearningsSummary{
			Code: []checkpoint.CodeLearning{
				{Path: "a.go", Line: 10, EndLine: 20, Finding: "range"},
				{Path: "b.go", Line: 5, Finding: "single"},
				{Path: "c.go", Finding: "no-line"},
			},
		},
	}

	got := buildSummaryMarkdown(summary)

	wantLines := []string{
		"- `a.go:10-20` — range",
		"- `b.go:5` — single",
		"- `c.go` — no-line",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("expected line %q in output, got:\n%s", line, got)
		}
	}
}

func TestBuildSummaryMarkdown_EmptyFrictionAndOpenItems(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.Summary{
		Intent:  "i",
		Outcome: "o",
	}

	got := buildSummaryMarkdown(summary)

	if strings.Contains(got, "## Friction") {
		t.Errorf("did not expect Friction heading, got:\n%s", got)
	}
	if strings.Contains(got, "## Open Items") {
		t.Errorf("did not expect Open Items heading, got:\n%s", got)
	}
}

func TestBuildSummaryMarkdown_BacktickEscape(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.Summary{
		Intent:  "Use the `foo` command",
		Outcome: "Wrapped in `bar`",
	}

	got := buildSummaryMarkdown(summary)

	if strings.Contains(got, "`foo`") {
		t.Errorf("expected backticks to be neutralized in Intent, got:\n%s", got)
	}
	if strings.Contains(got, "`bar`") {
		t.Errorf("expected backticks to be neutralized in Outcome, got:\n%s", got)
	}
	if !strings.Contains(got, "Use the ‘foo‘ command") {
		t.Errorf("expected U+2018 substitution in Intent, got:\n%s", got)
	}
}

func TestBuildSummaryMarkdown_NilSummary(t *testing.T) {
	t.Parallel()

	if got := buildSummaryMarkdown(nil); got != "" {
		t.Errorf("expected empty string for nil summary, got %q", got)
	}
}

func TestBuildFilesMarkdown_RendersPathsAsInlineCode(t *testing.T) {
	t.Parallel()

	got := buildFilesMarkdown([]string{
		"normal.go",
		"- tricky [path].go",
		"dir/`quoted`.go",
	})

	wantLines := []string{
		"- `normal.go`",
		"- `- tricky [path].go`",
		"- `dir/‘quoted‘.go`",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("expected escaped file line %q in output, got:\n%s", line, got)
		}
	}
}

func TestFormatCheckpointHeader_FullMetadataPlain(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	summary := &checkpoint.CheckpointSummary{
		TokenUsage: &agent.TokenUsage{InputTokens: 18432},
	}
	meta := checkpoint.CommittedMetadata{
		SessionID: "2026-04-29-7f3c1a",
		CreatedAt: time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
	}
	commits := []associatedCommit{{
		ShortSHA: "9f2c11a",
		Message:  "feat(auth): rotate session tokens on logout",
		Date:     time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC),
	}}
	author := checkpoint.Author{Name: "Peyton Montei", Email: "peyton@entire.io"}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(summary, meta, cpID, commits, author, styles)

	wantLines := []string{
		"● Checkpoint a3b2c4d5e6f7",
		"  session  2026-04-29-7f3c1a",
		"  created  2026-04-29 14:22:08",
		"  author   Peyton Montei <peyton@entire.io>",
		"  tokens   18.4k",
		"  commits  9f2c11a feat(auth): rotate session tokens on logout",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("expected line %q in header, got:\n%s", line, got)
		}
	}
}

func TestFormatCheckpointHeader_NoAuthor(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID: "s",
		CreatedAt: time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
	}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(nil, meta, cpID, nil, checkpoint.Author{}, styles)

	if strings.Contains(got, "  author") {
		t.Errorf("did not expect author row when Name empty, got:\n%s", got)
	}
}

func TestFormatCheckpointHeader_NoCommits(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID: "s",
		CreatedAt: time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
	}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(nil, meta, cpID, nil, checkpoint.Author{}, styles)

	if strings.Contains(got, "  commits") {
		t.Errorf("did not expect commits row when commits is nil, got:\n%s", got)
	}
}

func TestFormatCheckpointHeader_MultipleCommits(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID: "s",
		CreatedAt: time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
	}
	commits := []associatedCommit{
		{ShortSHA: "aaa1111", Message: "first", Date: time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC)},
		{ShortSHA: "bbb2222", Message: "second", Date: time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC)},
	}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(nil, meta, cpID, commits, checkpoint.Author{}, styles)

	if !strings.Contains(got, "  commits  (2)") {
		t.Errorf("expected commits row with count (2), got:\n%s", got)
	}
	if !strings.Contains(got, "           aaa1111 2026-04-29 first") {
		t.Errorf("expected first commit line aligned under value column, got:\n%s", got)
	}
	if !strings.Contains(got, "           bbb2222 2026-04-29 second") {
		t.Errorf("expected second commit line aligned under value column, got:\n%s", got)
	}
}

func TestFormatCheckpointHeader_EmptyCommitsSlice(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID: "s",
		CreatedAt: time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
	}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(nil, meta, cpID, []associatedCommit{}, checkpoint.Author{}, styles)

	if !strings.Contains(got, "  commits  (none on this branch)") {
		t.Errorf("expected explicit none row when commits slice is empty, got:\n%s", got)
	}
}

func TestFormatCheckpointHeader_NoTokenUsage(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID: "s",
		CreatedAt: time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
	}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(nil, meta, cpID, nil, checkpoint.Author{}, styles)

	if strings.Contains(got, "  tokens") {
		t.Errorf("did not expect tokens row when both meta and summary are nil, got:\n%s", got)
	}
}

func TestFormatCheckpointHeader_TokensFromSummaryFallback(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID:  "s",
		CreatedAt:  time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
		TokenUsage: nil,
	}
	summary := &checkpoint.CheckpointSummary{
		TokenUsage: &agent.TokenUsage{InputTokens: 1234},
	}
	styles := statusStyles{colorEnabled: false, width: 60}

	got := formatCheckpointHeader(summary, meta, cpID, nil, checkpoint.Author{}, styles)

	if !strings.Contains(got, "  tokens   1.2k") {
		t.Errorf("expected tokens row from summary fallback, got:\n%s", got)
	}
}

func TestFormatCheckpointHeader_ColorEnabledRenders(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	meta := checkpoint.CommittedMetadata{
		SessionID:  "s",
		CreatedAt:  time.Date(2026, 4, 29, 14, 22, 8, 0, time.UTC),
		TokenUsage: &agent.TokenUsage{InputTokens: 1234},
	}
	plainStyles := statusStyles{colorEnabled: false, width: 60}
	colorStyles := statusStyles{
		colorEnabled: true,
		width:        60,
		bold:         lipgloss.NewStyle().Bold(true),
		dim:          lipgloss.NewStyle().Faint(true),
		yellow:       lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	}

	plain := formatCheckpointHeader(nil, meta, cpID, nil, checkpoint.Author{}, plainStyles)
	styled := formatCheckpointHeader(nil, meta, cpID, nil, checkpoint.Author{}, colorStyles)

	if !strings.Contains(plain, "●") {
		t.Errorf("expected ● glyph in plain output, got:\n%s", plain)
	}
	if !strings.Contains(styled, "●") {
		t.Errorf("expected ● glyph in styled output, got:\n%s", styled)
	}
	if len(styled) <= len(plain) {
		t.Errorf("expected styled length (%d) > plain length (%d)", len(styled), len(plain))
	}
}

func TestBuildPagerCmd_LessRInjectedWhenEnvUnset(t *testing.T) {
	oldEnv := pagerLookupEnv
	t.Cleanup(func() { pagerLookupEnv = oldEnv })

	pagerLookupEnv = func(key string) string {
		if key == pagerEnvVar || key == lessEnvVar {
			return ""
		}
		return os.Getenv(key)
	}

	cmd, pager := buildPagerCmd(context.Background())

	if runtime.GOOS == windowsGOOS {
		t.Skip("LESS injection only applies to less on Unix")
	}
	if pager != lessPagerName {
		t.Fatalf("expected resolved pager 'less' on non-Windows, got %q", pager)
	}

	found := false
	for _, e := range cmd.Env {
		if e == lessRawControlEnv {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected LESS=-R in cmd.Env")
	}
}

func TestBuildPagerCmd_ReplacesEmptyLessEnv(t *testing.T) {
	t.Setenv(lessEnvVar, "")

	oldEnv := pagerLookupEnv
	t.Cleanup(func() { pagerLookupEnv = oldEnv })

	pagerLookupEnv = func(key string) string {
		if key == pagerEnvVar || key == lessEnvVar {
			return ""
		}
		return os.Getenv(key)
	}

	cmd, pager := buildPagerCmd(context.Background())

	if runtime.GOOS == windowsGOOS {
		t.Skip("LESS injection only applies to less on Unix")
	}
	if pager != lessPagerName {
		t.Fatalf("expected resolved pager 'less' on non-Windows, got %q", pager)
	}

	lessEntries := 0
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, lessEnvVar+"=") {
			lessEntries++
			if e != lessRawControlEnv {
				t.Errorf("expected %s, got %q", lessRawControlEnv, e)
			}
		}
	}
	if lessEntries != 1 {
		t.Errorf("expected exactly one LESS entry, got %d", lessEntries)
	}
}

func TestBuildPagerCmd_LessRSkippedWhenLessEnvSet(t *testing.T) {
	oldEnv := pagerLookupEnv
	t.Cleanup(func() { pagerLookupEnv = oldEnv })

	pagerLookupEnv = func(key string) string {
		switch key {
		case pagerEnvVar:
			return ""
		case lessEnvVar:
			return "-FRX"
		default:
			return os.Getenv(key)
		}
	}

	cmd, _ := buildPagerCmd(context.Background())

	for _, e := range cmd.Env {
		if e == lessRawControlEnv {
			t.Error("did not expect LESS=-R when user set LESS=-FRX")
		}
	}
}

func TestBuildPagerCmd_HonorsCustomPager(t *testing.T) {
	oldEnv := pagerLookupEnv
	t.Cleanup(func() { pagerLookupEnv = oldEnv })

	pagerLookupEnv = func(key string) string {
		if key == pagerEnvVar {
			return "bat"
		}
		return os.Getenv(key)
	}

	cmd, pager := buildPagerCmd(context.Background())

	if pager != "bat" {
		t.Errorf("expected resolved pager 'bat', got %q", pager)
	}
	for _, e := range cmd.Env {
		if e == lessRawControlEnv {
			t.Error("did not expect LESS=-R when user picked a custom pager")
		}
	}
}

func TestFormatBranchCheckpoints_BasicOutput(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Add feature X",
			Date:          now,
			CheckpointID:  "chk123456789",
			SessionID:     "2026-01-22-session-1",
			SessionPrompt: "Implement feature X",
		},
		{
			ID:            "def456ghi789",
			Message:       "Fix bug in Y",
			Date:          now.Add(-time.Hour),
			CheckpointID:  "chk987654321",
			SessionID:     "2026-01-22-session-2",
			SessionPrompt: "Fix the bug",
		},
	}

	output := formatBranchCheckpoints(io.Discard, "feature/my-branch", points, "")

	// Should show branch name
	if !strings.Contains(output, "feature/my-branch") {
		t.Errorf("expected branch name in output, got:\n%s", output)
	}

	// Should show checkpoint count (new metadata-row shape)
	if !strings.Contains(output, "checkpoints  2") {
		t.Errorf("expected 'checkpoints  2' in output, got:\n%s", output)
	}

	// Should show checkpoint messages
	if !strings.Contains(output, "Add feature X") {
		t.Errorf("expected first checkpoint message in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Fix bug in Y") {
		t.Errorf("expected second checkpoint message in output, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_GroupedByCheckpointID(t *testing.T) {
	// Create checkpoints spanning multiple days
	today := time.Date(2026, 1, 22, 10, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 1, 21, 14, 0, 0, 0, time.UTC)

	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Today checkpoint 1",
			Date:          today,
			CheckpointID:  "chk111111111",
			SessionID:     "2026-01-22-session-1",
			SessionPrompt: "First task today",
		},
		{
			ID:            "def456ghi789",
			Message:       "Today checkpoint 2",
			Date:          today.Add(-30 * time.Minute),
			CheckpointID:  "chk222222222",
			SessionID:     "2026-01-22-session-1",
			SessionPrompt: "First task today",
		},
		{
			ID:            "ghi789jkl012",
			Message:       "Yesterday checkpoint",
			Date:          yesterday,
			CheckpointID:  "chk333333333",
			SessionID:     "2026-01-21-session-2",
			SessionPrompt: "Task from yesterday",
		},
	}

	output := formatBranchCheckpoints(io.Discard, "main", points, "")

	// Should group by checkpoint ID - check for checkpoint headers (identity bullet)
	if !strings.Contains(output, "● chk111111111") {
		t.Errorf("expected checkpoint ID header in output, got:\n%s", output)
	}
	if !strings.Contains(output, "● chk333333333") {
		t.Errorf("expected checkpoint ID header in output, got:\n%s", output)
	}

	// Dates should appear inline with commits (format MM-DD)
	if !strings.Contains(output, "01-22") {
		t.Errorf("expected today's date inline with commits, got:\n%s", output)
	}
	if !strings.Contains(output, "01-21") {
		t.Errorf("expected yesterday's date inline with commits, got:\n%s", output)
	}

	// Today's checkpoints should appear before yesterday's (sorted by latest timestamp)
	todayIdx := strings.Index(output, "chk111111111")
	yesterdayIdx := strings.Index(output, "chk333333333")
	if todayIdx == -1 || yesterdayIdx == -1 || todayIdx > yesterdayIdx {
		t.Errorf("expected today's checkpoints before yesterday's, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_NoCheckpoints(t *testing.T) {
	output := formatBranchCheckpoints(io.Discard, "feature/empty-branch", nil, "")

	// Should show branch name
	if !strings.Contains(output, "feature/empty-branch") {
		t.Errorf("expected branch name in output, got:\n%s", output)
	}

	// Should indicate no checkpoints (new metadata-row shape: "checkpoints  0")
	if !strings.Contains(output, "checkpoints  0") && !strings.Contains(output, "No checkpoints") {
		t.Errorf("expected indication of no checkpoints, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_ShowsSessionInfo(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Test checkpoint",
			Date:          now,
			CheckpointID:  "chk123456789",
			SessionID:     "2026-01-22-test-session",
			SessionPrompt: "This is my test prompt",
		},
	}

	output := formatBranchCheckpoints(io.Discard, "main", points, "")

	// Should show session prompt
	if !strings.Contains(output, "This is my test prompt") {
		t.Errorf("expected session prompt in output, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_ShowsTemporaryIndicator(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:           "abc123def456",
			Message:      "Committed checkpoint",
			Date:         now,
			CheckpointID: "chk123456789",
			IsLogsOnly:   true, // Committed = logs only, no indicator shown
			SessionID:    "2026-01-22-session-1",
		},
		{
			ID:           "def456ghi789",
			Message:      "Active checkpoint",
			Date:         now.Add(-time.Hour),
			CheckpointID: "chk987654321",
			IsLogsOnly:   false, // Temporary = can be rewound, shows [temporary]
			SessionID:    "2026-01-22-session-1",
		},
	}

	output := formatBranchCheckpoints(io.Discard, "main", points, "")

	// Should indicate temporary (non-committed) checkpoints with [temporary]
	if !strings.Contains(output, "[temporary]") {
		t.Errorf("expected [temporary] indicator for non-committed checkpoint, got:\n%s", output)
	}

	// Committed checkpoints should NOT have [temporary] indicator
	// Find the line with the committed checkpoint and verify it doesn't have [temporary]
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "chk123456789") && strings.Contains(line, "[temporary]") {
			t.Errorf("committed checkpoint should not have [temporary] indicator, got:\n%s", output)
		}
	}
}

func TestFormatBranchCheckpoints_ShowsTaskCheckpoints(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:               "abc123def456",
			Message:          "Running tests (toolu_01ABC)",
			Date:             now,
			CheckpointID:     "chk123456789",
			IsTaskCheckpoint: true,
			ToolUseID:        "toolu_01ABC",
			SessionID:        "2026-01-22-session-1",
		},
	}

	output := formatBranchCheckpoints(io.Discard, "main", points, "")

	// Should indicate task checkpoint
	if !strings.Contains(output, "[Task]") && !strings.Contains(output, "task") {
		t.Errorf("expected task checkpoint indicator, got:\n%s", output)
	}
}

// TestFormatCheckpointGroup_NoPromptNoCommitShowsPlaceholder verifies the
// (no prompt recorded) placeholder appears only when neither a session prompt
// nor a commit message is available.
func TestFormatCheckpointGroup_NoPromptNoCommitShowsPlaceholder(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	styles := newStatusStyles(io.Discard)
	formatCheckpointGroup(&sb, checkpointGroup{
		checkpointID: "temporary",
		prompt:       "",
		isTemporary:  true,
		commits:      []commitEntry{{date: time.Now(), gitSHA: "deadbee", message: ""}},
	}, styles)
	out := sb.String()
	if !strings.Contains(out, "(no prompt recorded)") {
		t.Errorf("expected '(no prompt recorded)' placeholder:\n%s", out)
	}
}

// TestFormatCheckpointGroup_FallsBackToCommitMessage verifies the cascade:
// when SessionPrompt is empty but a commit message is present, the headline
// renders the commit message bare (not the placeholder).
func TestFormatCheckpointGroup_FallsBackToCommitMessage(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	styles := newStatusStyles(io.Discard)
	formatCheckpointGroup(&sb, checkpointGroup{
		checkpointID: "abc123def456",
		prompt:       "",
		commits:      []commitEntry{{date: time.Now(), gitSHA: "deadbee", message: "feat(cli): wire up paging"}},
	}, styles)
	out := sb.String()
	if !strings.Contains(out, "● abc123def456") {
		t.Errorf("expected identity bullet headline:\n%s", out)
	}
	if !strings.Contains(out, "feat(cli): wire up paging") {
		t.Errorf("expected commit-message fallback in headline:\n%s", out)
	}
	if strings.Contains(out, "(no prompt recorded)") {
		t.Errorf("did not expect dimmed placeholder when commit message available:\n%s", out)
	}
}

func TestFormatBranchCheckpoints_TruncatesLongMessages(t *testing.T) {
	now := time.Now()
	longMessage := strings.Repeat("a", 200) // 200 character message
	points := []strategy.RewindPoint{
		{
			ID:           "abc123def456",
			Message:      longMessage,
			Date:         now,
			CheckpointID: "chk123456789",
			SessionID:    "2026-01-22-session-1",
		},
	}

	output := formatBranchCheckpoints(io.Discard, "main", points, "")

	// Output should not contain the full 200 character message
	if strings.Contains(output, longMessage) {
		t.Errorf("expected long message to be truncated, got full message in output")
	}

	// Should contain truncation indicator (usually "...")
	if !strings.Contains(output, "...") {
		t.Errorf("expected truncation indicator '...' for long message, got:\n%s", output)
	}
}

func TestGetBranchCheckpoints_ReadsPromptFromShadowBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with an initial commit
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit initial file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Create metadata directory with prompt.txt
	sessionID := "2026-01-27-test-session"
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	expectedPrompt := "This is my test prompt for the checkpoint"
	if err := os.WriteFile(filepath.Join(metadataDir, paths.PromptFileName), []byte(expectedPrompt), 0o644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create first checkpoint (baseline copy) - this one gets filtered out
	store := checkpoint.NewGitStore(repo)
	baseCommit := initialCommit.String()[:7]
	_, err = store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint (baseline)",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() first checkpoint error = %v", err)
	}

	// Modify test file again for a second checkpoint with actual code changes
	if err := os.WriteFile(testFile, []byte("second modification"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// Create second checkpoint (has code changes, won't be filtered)
	_, err = store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "Second checkpoint with code changes",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: false, // Not first, has parent
	})
	if err != nil {
		t.Fatalf("WriteTemporary() second checkpoint error = %v", err)
	}

	// Now call getBranchCheckpoints and verify the prompt is read
	points, err := getBranchCheckpoints(context.Background(), repo, 10)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// Should have at least one temporary checkpoint (the second one with code changes)
	var foundTempCheckpoint bool
	for _, point := range points {
		if !point.IsLogsOnly && point.SessionID == sessionID {
			foundTempCheckpoint = true
			// Verify the prompt was read correctly from the shadow branch tree
			if point.SessionPrompt != expectedPrompt {
				t.Errorf("expected prompt %q, got %q", expectedPrompt, point.SessionPrompt)
			}
			break
		}
	}

	if !foundTempCheckpoint {
		t.Errorf("expected to find temporary checkpoint with session ID %s, got points: %+v", sessionID, points)
	}
}

func TestGetCurrentWorktreeHash_MainWorktree(t *testing.T) {
	// In a temp dir with a real .git directory (main worktree), getCurrentWorktreeHash
	// should return the hash of empty string (main worktree ID is "").
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)

	hash := getCurrentWorktreeHash(context.Background())
	expected := checkpoint.HashWorktreeID("") // Main worktree has empty ID
	if hash != expected {
		t.Errorf("getCurrentWorktreeHash(context.Background()) = %q, want %q (hash of empty worktree ID)", hash, expected)
	}
}

func TestGetReachableTemporaryCheckpoints_FiltersByWorktree(t *testing.T) {
	// Shadow branches are namespaced by worktree hash (entire/<commit>-<worktreeHash>).
	// Only shadow branches matching the current worktree should be included.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Setup metadata for both sessions
	sessionIDLocal := "2026-02-10-local-session"
	sessionIDOther := "2026-02-10-other-session"
	for _, sid := range []string{sessionIDLocal, sessionIDOther} {
		metaDir := filepath.Join(tmpDir, ".entire", "metadata", sid)
		if err := os.MkdirAll(metaDir, 0o755); err != nil {
			t.Fatalf("failed to create metadata dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(metaDir, paths.PromptFileName), []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write prompt: %v", err)
		}
		if err := os.WriteFile(filepath.Join(metaDir, "full.jsonl"), []byte(`{"test":true}`), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}
	}

	store := checkpoint.NewGitStore(repo)
	baseCommit := initialCommit.String()[:7]

	writeCheckpoints := func(sessionID, worktreeID string) {
		t.Helper()
		metaDirAbs := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
		// Baseline
		if _, err := store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
			SessionID: sessionID, BaseCommit: baseCommit, WorktreeID: worktreeID,
			ModifiedFiles: []string{"test.txt"}, MetadataDir: ".entire/metadata/" + sessionID,
			MetadataDirAbs: metaDirAbs, CommitMessage: "baseline", AuthorName: "Test",
			AuthorEmail: "test@test.com", IsFirstCheckpoint: true,
		}); err != nil {
			t.Fatalf("WriteTemporary baseline error: %v", err)
		}
		// Code change checkpoint
		if err := os.WriteFile(testFile, []byte(sessionID+" changes"), 0o644); err != nil {
			t.Fatalf("failed to modify test file: %v", err)
		}
		if _, err := store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
			SessionID: sessionID, BaseCommit: baseCommit, WorktreeID: worktreeID,
			ModifiedFiles: []string{"test.txt"}, MetadataDir: ".entire/metadata/" + sessionID,
			MetadataDirAbs: metaDirAbs, CommitMessage: "code changes", AuthorName: "Test",
			AuthorEmail: "test@test.com", IsFirstCheckpoint: false,
		}); err != nil {
			t.Fatalf("WriteTemporary code changes error: %v", err)
		}
	}

	writeCheckpoints(sessionIDLocal, "")               // Main worktree (matches test env)
	writeCheckpoints(sessionIDOther, "other-worktree") // Different worktree

	// getBranchCheckpoints should only include local worktree's checkpoints
	points, err := getBranchCheckpoints(context.Background(), repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints error: %v", err)
	}

	for _, p := range points {
		if p.SessionID == sessionIDOther {
			t.Errorf("found checkpoint from other worktree (session %s) - should be filtered out", sessionIDOther)
		}
	}
	var foundLocal bool
	for _, p := range points {
		if p.SessionID == sessionIDLocal {
			foundLocal = true
		}
	}
	if !foundLocal {
		t.Errorf("expected local worktree checkpoint (session %s), got: %+v", sessionIDLocal, points)
	}
}

// TestRunExplainBranchDefault_ShowsBranchCheckpoints is covered by TestExplainDefault_ShowsBranchView
// since runExplainDefault now calls runExplainBranchDefault directly.

func TestRunExplainBranchDefault_DetachedHead(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with a commit
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Checkout to detached HEAD state
	if err := w.Checkout(&git.CheckoutOptions{Hash: commitHash}); err != nil {
		t.Fatalf("failed to checkout detached HEAD: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainBranchDefault(context.Background(), &stdout, true)

	// Should NOT error
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	output := stdout.String()

	// Should indicate detached HEAD state in branch name
	if !strings.Contains(output, "HEAD") && !strings.Contains(output, "detached") {
		t.Errorf("expected output to indicate detached HEAD state, got: %s", output)
	}
}

func TestIsAncestorOf(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("v1"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commit1, err := w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit
	if err := os.WriteFile(testFile, []byte("v2"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commit2, err := w.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	t.Run("commit is ancestor of later commit", func(t *testing.T) {
		// commit1 should be an ancestor of commit2
		if !strategy.IsAncestorOf(context.Background(), repo, commit1, commit2) {
			t.Error("expected commit1 to be ancestor of commit2")
		}
	})

	t.Run("commit is not ancestor of earlier commit", func(t *testing.T) {
		// commit2 should NOT be an ancestor of commit1
		if strategy.IsAncestorOf(context.Background(), repo, commit2, commit1) {
			t.Error("expected commit2 to NOT be ancestor of commit1")
		}
	})

	t.Run("commit is ancestor of itself", func(t *testing.T) {
		// A commit should be considered an ancestor of itself
		if !strategy.IsAncestorOf(context.Background(), repo, commit1, commit1) {
			t.Error("expected commit to be ancestor of itself")
		}
	})
}

func TestGetBranchCheckpoints_OnFeatureBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit on main
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Get checkpoints (should be empty, but shouldn't error)
	points, err := getBranchCheckpoints(context.Background(), repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// Should return empty list (no checkpoints yet)
	if len(points) != 0 {
		t.Errorf("expected 0 checkpoints, got %d", len(points))
	}
}

func TestHasCodeChanges_FirstCommitReturnsTrue(t *testing.T) {
	// First commit on a shadow branch (no parent) should return true
	// since it captures the working copy state - real uncommitted work
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit (has no parent)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// First commit (no parent) captures working copy state - should return true
	if !hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return true for first commit (captures working copy)")
	}
}

func TestHasCodeChanges_OnlyMetadataChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with only .entire/ metadata changes
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", "session-123")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write metadata file: %v", err)
	}
	if _, err := w.Add(".entire"); err != nil {
		t.Fatalf("failed to add .entire: %v", err)
	}
	commitHash, err := w.Commit("metadata only commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// Only .entire/ changes should return false
	if hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return false when only .entire/ files changed")
	}
}

func TestHasCodeChanges_WithCodeChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with code changes
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add modified file: %v", err)
	}
	commitHash, err := w.Commit("code change commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// Code changes should return true
	if !hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return true when code files changed")
	}
}

func TestHasCodeChanges_MixedChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with BOTH code and metadata changes
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", "session-123")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write metadata file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	if _, err := w.Add(".entire"); err != nil {
		t.Fatalf("failed to add .entire: %v", err)
	}
	commitHash, err := w.Commit("mixed changes commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// Mixed changes should return true (code changes present)
	if !hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return true when commit has both code and metadata changes")
	}
}

func TestGetBranchCheckpoints_FiltersMainCommits(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit on master (go-git default)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	mainCommit, err := w.Commit("main commit with Entire-Checkpoint: abc123def456", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Create feature branch
	featureBranch := "feature/test"
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   mainCommit,
		Branch: plumbing.NewBranchReferenceName(featureBranch),
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}

	// Create commit on feature branch
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("feature commit with Entire-Checkpoint: def456ghi789", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Get checkpoints - should only include feature branch commits, not main
	// Note: Without actual checkpoint data in entire/checkpoints/v1, this returns empty
	// but the important thing is it doesn't error and the filtering logic runs
	points, err := getBranchCheckpoints(context.Background(), repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// Without checkpoint data (no entire/checkpoints/v1 branch), should return 0 checkpoints
	// This validates the filtering code path runs without error
	if len(points) != 0 {
		t.Errorf("expected 0 checkpoints without checkpoint data, got %d", len(points))
	}
}

func TestScopeTranscriptForCheckpoint_SlicesTranscript(t *testing.T) {
	// Transcript with 5 lines - prompts 1, 2, 3 with their responses
	fullTranscript := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"response 1"}]}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"response 2"}]}}
{"type":"user","uuid":"u3","message":{"content":"prompt 3"}}
`)

	// Checkpoint starts at line 2 (after prompt 1 and response 1)
	// Should only include lines 2-4 (prompt 2, response 2, prompt 3)
	scoped := scopeTranscriptForCheckpoint(fullTranscript, 2, agent.AgentTypeClaudeCode)

	// Parse the scoped transcript to verify content
	lines, err := transcript.ParseFromBytes(scoped)
	if err != nil {
		t.Fatalf("failed to parse scoped transcript: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines in scoped transcript, got %d", len(lines))
	}

	// First line should be prompt 2 (u2), not prompt 1
	if lines[0].UUID != "u2" {
		t.Errorf("expected first line to be u2 (prompt 2), got %s", lines[0].UUID)
	}

	// Last line should be prompt 3 (u3)
	if lines[2].UUID != "u3" {
		t.Errorf("expected last line to be u3 (prompt 3), got %s", lines[2].UUID)
	}
}

func TestScopeTranscriptForCheckpoint_ZeroLinesReturnsAll(t *testing.T) {
	transcriptData := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}
`)

	// With linesAtStart=0, should return full transcript
	scoped := scopeTranscriptForCheckpoint(transcriptData, 0, agent.AgentTypeClaudeCode)

	lines, err := transcript.ParseFromBytes(scoped)
	if err != nil {
		t.Fatalf("failed to parse scoped transcript: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines with linesAtStart=0, got %d", len(lines))
	}
}

func TestScopeTranscriptForCheckpoint_CodexUsesStoredLineOffsets(t *testing.T) {
	t.Parallel()

	fullTranscript := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"developer instructions"}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md\ninstructions"}]}}
{"timestamp":"t4","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}
{"timestamp":"t5","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"response to first"}]}}
{"timestamp":"t6","type":"event_msg","payload":{"type":"token_count","input_tokens":10,"output_tokens":1}}
{"timestamp":"t7","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second prompt"}]}}
{"timestamp":"t8","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"response to second"}]}}
`)

	scoped := scopeTranscriptForCheckpoint(fullTranscript, 6, agent.AgentTypeCodex)
	entries, err := summarize.BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(scoped), agent.AgentTypeCodex)
	if err != nil {
		t.Fatalf("failed to build condensed transcript: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 scoped entries, got %d", len(entries))
	}

	if entries[0].Type != summarize.EntryTypeUser || entries[0].Content != "second prompt" {
		t.Fatalf("expected first entry to be second prompt, got %#v", entries[0])
	}

	if entries[1].Type != summarize.EntryTypeAssistant || entries[1].Content != "response to second" {
		t.Fatalf("expected second entry to be second response, got %#v", entries[1])
	}
}

func TestExtractPromptsFromScopedTranscript(t *testing.T) {
	// Transcript with 4 lines - 2 user prompts, 2 assistant responses
	transcript := []byte(`{"type":"user","uuid":"u1","message":{"content":"First prompt"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"First response"}]}}
{"type":"user","uuid":"u2","message":{"content":"Second prompt"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Second response"}]}}
`)

	prompts := extractPromptsFromTranscript(transcript, "")

	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}

	if prompts[0] != "First prompt" {
		t.Errorf("expected first prompt 'First prompt', got %q", prompts[0])
	}

	if prompts[1] != "Second prompt" {
		t.Errorf("expected second prompt 'Second prompt', got %q", prompts[1])
	}
}

func TestFormatCheckpointOutput_UsesScopedPrompts(t *testing.T) {
	// Full transcript with 4 lines (2 prompts + 2 responses)
	// Checkpoint starts at line 2 (should only show second prompt)
	fullTranscript := []byte(`{"type":"user","uuid":"u1","message":{"content":"First prompt - should NOT appear"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"First response"}]}}
{"type":"user","uuid":"u2","message":{"content":"Second prompt - SHOULD appear"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Second response"}]}}
`)

	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 2, // Checkpoint starts at line 2
		},
		Prompts:    "First prompt - should NOT appear\nSecond prompt - SHOULD appear", // Full prompts (not scoped yet)
		Transcript: fullTranscript,
	}

	// Verbose output should use scoped prompts
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false, &bytes.Buffer{})

	// Should show ONLY the second prompt (scoped)
	if !strings.Contains(output, "Second prompt - SHOULD appear") {
		t.Errorf("expected scoped prompt in output, got:\n%s", output)
	}

	// Should NOT show the first prompt (it's before this checkpoint's scope)
	if strings.Contains(output, "First prompt - should NOT appear") {
		t.Errorf("expected first prompt to be excluded from scoped output, got:\n%s", output)
	}
}

func TestFormatCheckpointOutput_FallsBackToStoredPrompts(t *testing.T) {
	// Test backwards compatibility: when no transcript exists, use stored prompts
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Stored prompt from older checkpoint",
		Transcript: nil, // No transcript available
	}

	// Verbose output should fall back to stored prompts
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false, &bytes.Buffer{})

	// Intent should use stored prompt
	if !strings.Contains(output, "Stored prompt from older checkpoint") {
		t.Errorf("expected fallback to stored prompts, got:\n%s", output)
	}
}

func TestFormatCheckpointOutput_FullShowsEntireTranscript(t *testing.T) {
	// Test that --full mode shows the entire transcript, not scoped
	fullTranscript := []byte(`{"type":"user","uuid":"u1","message":{"content":"First prompt"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"First response"}]}}
{"type":"user","uuid":"u2","message":{"content":"Second prompt"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Second response"}]}}
`)

	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 2, // Checkpoint starts at line 2
		},
		Transcript: fullTranscript,
	}

	// Full mode should show the ENTIRE transcript (not scoped)
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, true, &bytes.Buffer{})

	// Should show the full transcript including first prompt (even though scoped prompts exclude it)
	if !strings.Contains(output, "First prompt") {
		t.Errorf("expected --full to show entire transcript including first prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "Second prompt") {
		t.Errorf("expected --full to show entire transcript including second prompt, got:\n%s", output)
	}
}

func TestRunExplainCommit_NoCheckpointTrailer(t *testing.T) {
	// Create test repo with a commit that has no Entire-Checkpoint trailer
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Create a commit without checkpoint trailer
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	hash, err := w.Commit("Regular commit without trailer", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var buf bytes.Buffer
	err = runExplainCommit(context.Background(), &buf, &buf, hash.String()[:7], false, false, false, false, false, false, false)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "✗ No associated Entire checkpoint") {
		t.Errorf("expected styled failure block, got: %s", output)
	}
	if !strings.Contains(output, "  reason") {
		t.Errorf("expected reason row, got: %s", output)
	}
}

func TestRunExplainCommit_WithCheckpointTrailer(t *testing.T) {
	// Create test repo with a commit that has an Entire-Checkpoint trailer
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	// Create a commit with checkpoint trailer
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}

	// Create commit with checkpoint trailer
	checkpointID := "abc123def456"
	commitMsg := "Feature commit\n\nEntire-Checkpoint: " + checkpointID + "\n"
	hash, err := w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var buf bytes.Buffer
	// This should try to look up the checkpoint and fail (checkpoint doesn't exist in store)
	// but it should still attempt the lookup rather than showing commit details
	err = runExplainCommit(context.Background(), &buf, &buf, hash.String()[:7], false, false, false, false, false, false, false)

	// Should error because the checkpoint doesn't exist in the store
	if err == nil {
		t.Fatalf("expected error for missing checkpoint in store, got nil")
	}

	// Error should mention checkpoint not found
	if !strings.Contains(err.Error(), "checkpoint not found") && !strings.Contains(err.Error(), "abc123def456") {
		t.Errorf("expected error about checkpoint not found, got: %v", err)
	}
}

func TestFormatBranchCheckpoints_SessionFilter(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Checkpoint from session 1",
			Date:          now,
			CheckpointID:  "chk111111111",
			SessionID:     "2026-01-22-session-alpha",
			SessionPrompt: "Task for session alpha",
		},
		{
			ID:            "def456ghi789",
			Message:       "Checkpoint from session 2",
			Date:          now.Add(-time.Hour),
			CheckpointID:  "chk222222222",
			SessionID:     "2026-01-22-session-beta",
			SessionPrompt: "Task for session beta",
		},
		{
			ID:            "ghi789jkl012",
			Message:       "Another checkpoint from session 1",
			Date:          now.Add(-2 * time.Hour),
			CheckpointID:  "chk333333333",
			SessionID:     "2026-01-22-session-alpha",
			SessionPrompt: "Another task for session alpha",
		},
	}

	t.Run("no filter shows all checkpoints", func(t *testing.T) {
		output := formatBranchCheckpoints(io.Discard, "main", points, "")

		// Should show all checkpoints (new metadata-row shape)
		if !strings.Contains(output, "checkpoints  3") {
			t.Errorf("expected 'checkpoints  3' in output, got:\n%s", output)
		}
		// Should show prompts from both sessions
		if !strings.Contains(output, "Task for session alpha") {
			t.Errorf("expected alpha session prompt in output, got:\n%s", output)
		}
		if !strings.Contains(output, "Task for session beta") {
			t.Errorf("expected beta session prompt in output, got:\n%s", output)
		}
	})

	t.Run("filter by exact session ID", func(t *testing.T) {
		output := formatBranchCheckpoints(io.Discard, "main", points, "2026-01-22-session-alpha")

		// Should show only alpha checkpoints (2 of them)
		if !strings.Contains(output, "checkpoints  2") {
			t.Errorf("expected 'checkpoints  2' in output, got:\n%s", output)
		}
		if !strings.Contains(output, "Task for session alpha") {
			t.Errorf("expected alpha session prompt in output, got:\n%s", output)
		}
		// Should NOT contain beta session prompt
		if strings.Contains(output, "Task for session beta") {
			t.Errorf("expected output to NOT contain beta session prompt, got:\n%s", output)
		}
		// Should show filter info as a metadata row (label aligned to widest "checkpoints")
		if !strings.Contains(output, "session      2026-01-22-session-alpha") {
			t.Errorf("expected 'session ... 2026-01-22-session-alpha' in output, got:\n%s", output)
		}
	})

	t.Run("filter by session ID prefix", func(t *testing.T) {
		output := formatBranchCheckpoints(io.Discard, "main", points, "2026-01-22-session-b")

		// Should show only beta checkpoint (1)
		if !strings.Contains(output, "checkpoints  1") {
			t.Errorf("expected 'checkpoints  1' in output, got:\n%s", output)
		}
		if !strings.Contains(output, "Task for session beta") {
			t.Errorf("expected beta session prompt in output, got:\n%s", output)
		}
	})

	t.Run("filter with no matches", func(t *testing.T) {
		output := formatBranchCheckpoints(io.Discard, "main", points, "nonexistent-session")

		// Should show 0 checkpoints
		if !strings.Contains(output, "checkpoints  0") {
			t.Errorf("expected 'checkpoints  0' in output, got:\n%s", output)
		}
		// Should show filter info even with no matches (label aligned to widest "checkpoints")
		if !strings.Contains(output, "session      nonexistent-session") {
			t.Errorf("expected 'session ... nonexistent-session' in output, got:\n%s", output)
		}
	})
}

func TestRunExplain_SessionFlagFiltersListView(t *testing.T) {
	// Test that --session alone (without --checkpoint or --commit) filters the list view.
	// This is a unit test for the routing logic.
	// Use a fresh git repo so we don't walk the real repo's shadow branches (which is slow).
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test User"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = tmp
		cmd.Env = testutil.GitIsolatedEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(tmp)

	var buf, errBuf bytes.Buffer

	// When session is specified alone, it should NOT error for mutual exclusivity
	// It should route to the list view with a filter (which may fail for other reasons
	// like not being in a git repo, but not for mutual exclusivity)
	err := runExplain(context.Background(), &buf, &errBuf, "some-session", "", "", "", false, false, false, false, false, false, false)

	// Should NOT be a mutual exclusivity error
	if err != nil && strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("--session alone should not trigger mutual exclusivity error, got: %v", err)
	}
}

func TestRunExplain_SessionWithCheckpointStillMutuallyExclusive(t *testing.T) {
	// Test that --session with --checkpoint is still an error
	var buf, errBuf bytes.Buffer

	err := runExplain(context.Background(), &buf, &errBuf, "some-session", "", "some-checkpoint", "", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error when --session and --checkpoint both specified")
	}
	if !strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %v", err)
	}
}

func TestRunExplain_SessionWithCommitStillMutuallyExclusive(t *testing.T) {
	// Test that --session with --commit is still an error
	var buf, errBuf bytes.Buffer

	err := runExplain(context.Background(), &buf, &errBuf, "some-session", "some-commit", "", "", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error when --session and --commit both specified")
	}
	if !strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %v", err)
	}
}

func TestFormatCheckpointOutput_WithAuthor(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	author := checkpoint.Author{
		Name:  "Alice Developer",
		Email: "alice@example.com",
	}

	// With author, should show author line
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, author, true, false, &bytes.Buffer{})

	if !strings.Contains(output, "  author   Alice Developer <alice@example.com>") {
		t.Errorf("expected author line in output, got:\n%s", output)
	}
}

func TestFormatCheckpointOutput_EmptyAuthor(t *testing.T) {
	// Test backwards compatibility: when no transcript exists, use stored prompts
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	// Empty author - should not show author line
	author := checkpoint.Author{}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, author, true, false, &bytes.Buffer{})

	if strings.Contains(output, "  author") {
		t.Errorf("expected no author line for empty author, got:\n%s", output)
	}
}

func TestGetAssociatedCommits(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	checkpointID := id.MustCheckpointID("abc123def456")

	// Create first commit without checkpoint trailer
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-2 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create commit with matching checkpoint trailer
	if err := os.WriteFile(testFile, []byte("with checkpoint"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg := trailers.FormatCheckpoint("feat: add feature", checkpointID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Alice Developer",
			Email: "alice@example.com",
			When:  time.Now().Add(-1 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create checkpoint commit: %v", err)
	}

	// Create another commit without checkpoint trailer
	if err := os.WriteFile(testFile, []byte("after checkpoint"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("unrelated commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create unrelated commit: %v", err)
	}

	// Test: should find the one commit with matching checkpoint
	commits, err := getAssociatedCommits(context.Background(), repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}

	if len(commits) != 1 {
		t.Fatalf("expected 1 associated commit, got %d", len(commits))
	}

	commit := commits[0]
	if commit.Author != "Alice Developer" {
		t.Errorf("expected author 'Alice Developer', got %q", commit.Author)
	}
	if !strings.Contains(commit.Message, "feat: add feature") {
		t.Errorf("expected message to contain 'feat: add feature', got %q", commit.Message)
	}
	if len(commit.ShortSHA) != 7 {
		t.Errorf("expected 7-char short SHA, got %d chars: %q", len(commit.ShortSHA), commit.ShortSHA)
	}
	if len(commit.SHA) != 40 {
		t.Errorf("expected 40-char full SHA, got %d chars", len(commit.SHA))
	}
}

func TestGetAssociatedCommits_NoMatches(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create commit without checkpoint trailer
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("regular commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	// Search for a checkpoint ID that doesn't exist (valid format: 12 hex chars)
	checkpointID := id.MustCheckpointID("aaaa11112222")
	commits, err := getAssociatedCommits(context.Background(), repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}

	if len(commits) != 0 {
		t.Errorf("expected 0 associated commits, got %d", len(commits))
	}
}

func TestGetAssociatedCommits_MultipleMatches(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	checkpointID := id.MustCheckpointID("abc123def456")

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-3 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create first commit with checkpoint trailer
	if err := os.WriteFile(testFile, []byte("first"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg := trailers.FormatCheckpoint("first checkpoint commit", checkpointID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-2 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create first checkpoint commit: %v", err)
	}

	// Create second commit with same checkpoint trailer (e.g., amend scenario)
	if err := os.WriteFile(testFile, []byte("second"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg = trailers.FormatCheckpoint("second checkpoint commit", checkpointID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-1 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create second checkpoint commit: %v", err)
	}

	// Test: should find both commits with matching checkpoint
	commits, err := getAssociatedCommits(context.Background(), repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}

	if len(commits) != 2 {
		t.Fatalf("expected 2 associated commits, got %d", len(commits))
	}

	// Should be in reverse chronological order (newest first)
	if !strings.Contains(commits[0].Message, "second") {
		t.Errorf("expected newest commit first, got %q", commits[0].Message)
	}
	if !strings.Contains(commits[1].Message, "first") {
		t.Errorf("expected older commit second, got %q", commits[1].Message)
	}
}

func TestFormatCheckpointOutput_WithAssociatedCommits(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-02-04-test-session",
			CreatedAt:                 time.Date(2026, 2, 4, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	associatedCommits := []associatedCommit{
		{
			SHA:      "abc123def4567890abc123def4567890abc12345",
			ShortSHA: "abc123d",
			Message:  "feat: add feature",
			Author:   "Alice Developer",
			Date:     time.Date(2026, 2, 4, 11, 0, 0, 0, time.UTC),
		},
		{
			SHA:      "def456abc7890123def456abc7890123def45678",
			ShortSHA: "def456a",
			Message:  "fix: update feature",
			Author:   "Bob Developer",
			Date:     time.Date(2026, 2, 4, 12, 0, 0, 0, time.UTC),
		},
	}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), associatedCommits, checkpoint.Author{}, true, false, &bytes.Buffer{})

	// Should show commits section with count
	if !strings.Contains(output, "  commits  (2)") {
		t.Errorf("expected 'Commits: (2)' in output, got:\n%s", output)
	}
	// Should show commit details
	if !strings.Contains(output, "abc123d") {
		t.Errorf("expected short SHA 'abc123d' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "def456a") {
		t.Errorf("expected short SHA 'def456a' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "feat: add feature") {
		t.Errorf("expected commit message in output, got:\n%s", output)
	}
	if !strings.Contains(output, "fix: update feature") {
		t.Errorf("expected commit message in output, got:\n%s", output)
	}
	// Should show date in format YYYY-MM-DD
	if !strings.Contains(output, "2026-02-04") {
		t.Errorf("expected date in output, got:\n%s", output)
	}
}

// createMergeCommit creates a merge commit with two parents using go-git plumbing APIs.
// Returns the merge commit hash.
func createMergeCommit(t *testing.T, repo *git.Repository, parent1, parent2 plumbing.Hash, treeHash plumbing.Hash, message string) plumbing.Hash {
	t.Helper()

	sig := object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}
	commit := object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{parent1, parent2},
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("failed to encode merge commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store merge commit: %v", err)
	}
	return hash
}

func TestGetBranchCheckpoints_WithMergeFromMain(t *testing.T) {
	// Regression test: when main is merged into a feature branch, getBranchCheckpoints
	// should still find feature branch checkpoints from before the merge.
	// The old repo.Log() approach did a full DAG walk, entering main's history through
	// merge commits and eventually hitting consecutiveMainLimit, silently dropping
	// older feature branch checkpoints.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit on master
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-5 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create feature branch from initial commit
	featureBranch := plumbing.NewBranchReferenceName("feature/test")
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   initialCommit,
		Branch: featureBranch,
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}

	// Create first feature checkpoint commit (BEFORE the merge)
	cpID1 := id.MustCheckpointID("aaa111bbb222")
	if err := os.WriteFile(testFile, []byte("feature work 1"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	featureCommit1, err := w.Commit(trailers.FormatCheckpoint("feat: first feature", cpID1), &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create first feature commit: %v", err)
	}

	// Switch to master and add commits (simulating work on main)
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	}); err != nil {
		t.Fatalf("failed to checkout master: %v", err)
	}
	if err := os.WriteFile(testFile, []byte("main work"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	mainCommit, err := w.Commit("main: add work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Switch back to feature branch
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	}); err != nil {
		t.Fatalf("failed to checkout feature branch: %v", err)
	}

	// Create merge commit: merge main into feature (feature is first parent, main is second parent)
	featureCommitObj, commitObjErr := repo.CommitObject(featureCommit1)
	if commitObjErr != nil {
		t.Fatalf("failed to get feature commit object: %v", commitObjErr)
	}
	featureTree, treeErr := featureCommitObj.Tree()
	if treeErr != nil {
		t.Fatalf("failed to get feature commit tree: %v", treeErr)
	}
	mergeHash := createMergeCommit(t, repo, featureCommit1, mainCommit, featureTree.Hash, "Merge branch 'master' into feature/test")

	// Update feature branch ref to point to merge commit
	ref := plumbing.NewHashReference(featureBranch, mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to update feature branch ref: %v", err)
	}

	// Reset worktree to merge commit
	if err := w.Reset(&git.ResetOptions{Commit: mergeHash, Mode: git.HardReset}); err != nil {
		t.Fatalf("failed to reset to merge: %v", err)
	}

	// Create second feature checkpoint commit (AFTER the merge)
	cpID2 := id.MustCheckpointID("ccc333ddd444")
	if err := os.WriteFile(testFile, []byte("feature work 2"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit(trailers.FormatCheckpoint("feat: second feature", cpID2), &git.CommitOptions{
		Author:    &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-1 * time.Hour)},
		Parents:   []plumbing.Hash{mergeHash},
		Committer: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-1 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create second feature commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Test getAssociatedCommits - should find BOTH feature checkpoint commits
	// by walking first-parent chain (skipping the merge's second parent into main)
	commits1, err := getAssociatedCommits(context.Background(), repo, cpID1, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits for cpID1 error: %v", err)
	}
	if len(commits1) != 1 {
		t.Errorf("expected 1 commit for cpID1 (first feature checkpoint), got %d", len(commits1))
	}

	commits2, err := getAssociatedCommits(context.Background(), repo, cpID2, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits for cpID2 error: %v", err)
	}
	if len(commits2) != 1 {
		t.Errorf("expected 1 commit for cpID2 (second feature checkpoint), got %d", len(commits2))
	}
}

func TestGetBranchCheckpoints_MergeCommitAtHEAD(t *testing.T) {
	// Test that when HEAD itself is a merge commit, walkFirstParentCommits
	// correctly follows the first parent (feature branch history) and
	// doesn't walk into the second parent (main branch history).
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit on master
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-5 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create feature branch
	featureBranch := plumbing.NewBranchReferenceName("feature/merge-at-head")
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   initialCommit,
		Branch: featureBranch,
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}

	// Create feature checkpoint commit
	cpID := id.MustCheckpointID("eee555fff666")
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	featureCommit, err := w.Commit(trailers.FormatCheckpoint("feat: feature work", cpID), &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Switch to master and add a commit
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	}); err != nil {
		t.Fatalf("failed to checkout master: %v", err)
	}
	mainFile := filepath.Join(tmpDir, "main.txt")
	if err := os.WriteFile(mainFile, []byte("main work"), 0o644); err != nil {
		t.Fatalf("failed to write main file: %v", err)
	}
	if _, err := w.Add("main.txt"); err != nil {
		t.Fatalf("failed to add main file: %v", err)
	}
	mainCommit, err := w.Commit("main: add work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-2 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Switch back to feature and create merge commit AT HEAD
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	}); err != nil {
		t.Fatalf("failed to checkout feature branch: %v", err)
	}

	featureCommitObj, commitObjErr := repo.CommitObject(featureCommit)
	if commitObjErr != nil {
		t.Fatalf("failed to get feature commit object: %v", commitObjErr)
	}
	featureTree, treeErr := featureCommitObj.Tree()
	if treeErr != nil {
		t.Fatalf("failed to get feature commit tree: %v", treeErr)
	}
	mergeHash := createMergeCommit(t, repo, featureCommit, mainCommit, featureTree.Hash, "Merge branch 'master' into feature/merge-at-head")

	// Update feature branch ref to merge commit (HEAD IS the merge)
	ref := plumbing.NewHashReference(featureBranch, mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to update feature branch ref: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// HEAD is the merge commit itself.
	// getAssociatedCommits should walk: merge -> featureCommit -> initial
	// and find the checkpoint on featureCommit.
	commits, err := getAssociatedCommits(context.Background(), repo, cpID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 associated commit when HEAD is merge commit, got %d", len(commits))
	}
	if !strings.Contains(commits[0].Message, "feat: feature work") {
		t.Errorf("expected feature commit message, got %q", commits[0].Message)
	}
}

func TestWalkFirstParentCommits_SkipsMergeParents(t *testing.T) {
	// Verify that walkFirstParentCommits follows only first parents and doesn't
	// enter the second parent (merge source) of merge commits.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit (shared ancestor)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("A: initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-5 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create feature branch with one commit
	featureBranch := plumbing.NewBranchReferenceName("feature/walk-test")
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   initialCommit,
		Branch: featureBranch,
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}
	if err := os.WriteFile(testFile, []byte("feature"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	featureCommit, err := w.Commit("B: feature work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Create main branch commit (will be merge source)
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	}); err != nil {
		t.Fatalf("failed to checkout master: %v", err)
	}
	mainFile := filepath.Join(tmpDir, "main.txt")
	if err := os.WriteFile(mainFile, []byte("main"), 0o644); err != nil {
		t.Fatalf("failed to write main file: %v", err)
	}
	if _, err := w.Add("main.txt"); err != nil {
		t.Fatalf("failed to add main file: %v", err)
	}
	mainCommit, err := w.Commit("C: main work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Switch to feature and create merge commit
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	}); err != nil {
		t.Fatalf("failed to checkout feature: %v", err)
	}
	featureCommitObj, commitObjErr := repo.CommitObject(featureCommit)
	if commitObjErr != nil {
		t.Fatalf("failed to get feature commit object: %v", commitObjErr)
	}
	featureTree, treeErr := featureCommitObj.Tree()
	if treeErr != nil {
		t.Fatalf("failed to get feature commit tree: %v", treeErr)
	}
	mergeHash := createMergeCommit(t, repo, featureCommit, mainCommit, featureTree.Hash, "M: merge main into feature")

	// Walk should visit: M (merge) -> B (feature) -> A (initial)
	// It should NOT visit C (main work), because that's the second parent of the merge.
	var visited []string
	err = walkFirstParentCommits(context.Background(), repo, mergeHash, 0, func(c *object.Commit) error {
		visited = append(visited, strings.Split(c.Message, "\n")[0])
		return nil
	})
	if err != nil {
		t.Fatalf("walkFirstParentCommits error: %v", err)
	}

	expected := []string{"M: merge main into feature", "B: feature work", "A: initial"}
	if len(visited) != len(expected) {
		t.Fatalf("expected %d commits visited, got %d: %v", len(expected), len(visited), visited)
	}
	for i, msg := range expected {
		if visited[i] != msg {
			t.Errorf("commit %d: expected %q, got %q", i, msg, visited[i])
		}
	}

	// Verify C was NOT visited
	for _, msg := range visited {
		if strings.Contains(msg, "C: main work") {
			t.Error("walkFirstParentCommits visited main branch commit (second parent of merge) - should only follow first parents")
		}
	}
}

func TestFormatCheckpointOutput_NoCommitsOnBranch(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-02-04-test-session",
			CreatedAt:                 time.Date(2026, 2, 4, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	// No associated commits - use empty slice (not nil) to indicate "searched but found none"
	associatedCommits := []associatedCommit{}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), associatedCommits, checkpoint.Author{}, true, false, &bytes.Buffer{})

	// Should show message indicating no commits found
	if !strings.Contains(output, "  commits  (none on this branch)") {
		t.Errorf("expected 'Commits: No commits found on this branch' in output, got:\n%s", output)
	}
}

func TestGetAssociatedCommits_SearchAllFindsMergedBranchCommits(t *testing.T) {
	// Regression test: --search-all should find checkpoint commits that live on
	// a feature branch merged into main via a true merge commit. These commits
	// are on the second parent of the merge, so first-parent-only traversal
	// won't find them — but --search-all should use full DAG walk.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	checkpointID := id.MustCheckpointID("aabb11223344")

	// Create initial commit on main
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	mainBase, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create a "feature branch" commit with checkpoint trailer (will become second parent)
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	featureMsg := trailers.FormatCheckpoint("feat: add feature", checkpointID)
	featureCommit, err := w.Commit(featureMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Feature Dev", Email: "dev@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Move HEAD back to mainBase to simulate being on main
	// Create a new commit on "main" that diverges
	if err := os.WriteFile(testFile, []byte("main work"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	mainCommitObj, err := repo.CommitObject(mainBase)
	if err != nil {
		t.Fatalf("failed to get main base commit: %v", err)
	}
	mainTree, err := mainCommitObj.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Create a second main commit (to diverge from feature)
	mainTip := createCommitWithTree(t, repo, mainTree.Hash, []plumbing.Hash{mainBase}, "main: parallel work")

	// Create merge commit: first parent = mainTip, second parent = featureCommit
	featureCommitObj, err := repo.CommitObject(featureCommit)
	if err != nil {
		t.Fatalf("failed to get feature commit: %v", err)
	}
	featureTree, err := featureCommitObj.Tree()
	if err != nil {
		t.Fatalf("failed to get feature tree: %v", err)
	}
	mergeHash := createMergeCommit(t, repo, mainTip, featureCommit, featureTree.Hash, "Merge feature into main")

	// Point HEAD at merge commit
	ref := plumbing.NewHashReference("refs/heads/main", mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	headRef := plumbing.NewSymbolicReference("HEAD", "refs/heads/main")
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}

	// Without --search-all (first-parent only): should NOT find the feature commit
	// because it's on the second parent of the merge
	commits, err := getAssociatedCommits(context.Background(), repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("expected 0 commits without --search-all (first-parent only), got %d", len(commits))
	}

	// With --search-all (full DAG walk): SHOULD find the feature commit
	commits, err = getAssociatedCommits(context.Background(), repo, checkpointID, true)
	if err != nil {
		t.Fatalf("getAssociatedCommits --search-all error: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit with --search-all, got %d", len(commits))
	}
	if commits[0].Author != "Feature Dev" {
		t.Errorf("expected author 'Feature Dev', got %q", commits[0].Author)
	}
}

func TestGetBranchCheckpoints_DefaultBranchFindsMergedCheckpoints(t *testing.T) {
	// Regression test: on the default branch, getBranchCheckpoints should find
	// checkpoint commits that came in via merge commits (second parents).
	// First-parent-only traversal would miss these.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit on master (this is the default branch)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	masterBase, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create a feature branch commit with checkpoint trailer
	cpID := id.MustCheckpointID("fea112233344")
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	featureCommit, err := w.Commit(trailers.FormatCheckpoint("feat: add feature", cpID), &git.CommitOptions{
		Author: &object.Signature{Name: "Feature Dev", Email: "dev@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Get tree hashes for creating commits via plumbing
	masterBaseObj, err := repo.CommitObject(masterBase)
	if err != nil {
		t.Fatalf("failed to get master base: %v", err)
	}
	masterTree, err := masterBaseObj.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}
	featureObj, err := repo.CommitObject(featureCommit)
	if err != nil {
		t.Fatalf("failed to get feature commit: %v", err)
	}
	featureTree, err := featureObj.Tree()
	if err != nil {
		t.Fatalf("failed to get feature tree: %v", err)
	}

	// Create a second commit on master (diverge from feature)
	masterTip := createCommitWithTree(t, repo, masterTree.Hash, []plumbing.Hash{masterBase}, "main: parallel work")

	// Create merge commit on master: first parent = masterTip, second parent = featureCommit
	mergeHash := createMergeCommit(t, repo, masterTip, featureCommit, featureTree.Hash, "Merge feature into master")

	// Point master at merge commit
	ref := plumbing.NewHashReference("refs/heads/master", mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to set ref: %v", err)
	}
	headRef := plumbing.NewSymbolicReference("HEAD", "refs/heads/master")
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}

	// Write committed checkpoint metadata so getBranchCheckpoints can find it
	store := checkpoint.NewGitStore(repo)
	if err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session",
		Strategy:     "manual-commit",
		FilesTouched: []string{"test.txt"},
		Prompts:      []string{"add feature"},
	}); err != nil {
		t.Fatalf("failed to write committed checkpoint: %v", err)
	}

	// getBranchCheckpoints on master should find the checkpoint from the merged feature branch
	points, err := getBranchCheckpoints(context.Background(), repo, 100)
	if err != nil {
		t.Fatalf("getBranchCheckpoints error: %v", err)
	}

	// Should find at least the checkpoint from the merged feature branch
	var found bool
	for _, p := range points {
		if p.CheckpointID == cpID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected to find checkpoint %s from merged feature branch on default branch, got %d points: %v", cpID, len(points), points)
	}
}

func TestGetBranchCheckpoints_ReadsPromptFromCommittedCheckpoint(t *testing.T) {
	// Verifies that getBranchCheckpoints populates RewindPoint.SessionPrompt
	// from prompt.txt on entire/checkpoints/v1 (committed checkpoint) without
	// needing to read/parse the full transcript.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create a checkpoint ID and write committed checkpoint with prompt data
	cpID, err := id.NewCheckpointID("aabb11223344")
	if err != nil {
		t.Fatalf("failed to create checkpoint ID: %v", err)
	}

	expectedPrompt := "Refactor the authentication module to use JWT tokens"
	store := checkpoint.NewGitStore(repo)
	if err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "2026-02-27-test-session",
		Strategy:     "manual-commit",
		FilesTouched: []string{"auth.go"},
		Prompts:      []string{expectedPrompt},
	}); err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Create a user commit with the Entire-Checkpoint trailer
	if err := os.WriteFile(testFile, []byte("updated with auth changes"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg := trailers.FormatCheckpoint("Refactor auth module", cpID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit with checkpoint trailer: %v", err)
	}

	// Call getBranchCheckpoints and verify prompt is populated
	points, err := getBranchCheckpoints(context.Background(), repo, 10)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	var foundCommitted bool
	for _, p := range points {
		if p.CheckpointID == cpID {
			foundCommitted = true
			if !p.IsLogsOnly {
				t.Error("expected committed checkpoint to have IsLogsOnly=true")
			}
			if p.SessionPrompt != expectedPrompt {
				t.Errorf("expected SessionPrompt = %q, got %q", expectedPrompt, p.SessionPrompt)
			}
			break
		}
	}

	if !foundCommitted {
		t.Errorf("expected to find committed checkpoint %s, got %d points", cpID, len(points))
	}
}

func TestGetBranchCheckpoints_V2OnlyCheckpointDiscoverable(t *testing.T) {
	// When the v1 metadata branch doesn't exist but v2 has the checkpoint,
	// getBranchCheckpoints should still find committed checkpoints.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("initial"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Enable v2 via settings.
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o644,
	))

	cpID := id.MustCheckpointID("dd11ee22ff33")
	expectedPrompt := "Create the v2-only checkpoint test file"

	// Write checkpoint ONLY to v2 store.
	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	require.NoError(t, v2Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2-only",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		Prompts:      []string{expectedPrompt},
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	// Create a user commit with the Entire-Checkpoint trailer.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("updated"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	commitMsg := trailers.FormatCheckpoint("Create v2 test file", cpID)
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Verify no v1 metadata branch exists.
	_, v1Err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, v1Err, "v1 metadata branch should not exist")

	// getBranchCheckpoints should find the v2-only checkpoint.
	points, err := getBranchCheckpoints(context.Background(), repo, 10)
	require.NoError(t, err)

	var found bool
	for _, p := range points {
		if p.CheckpointID == cpID {
			found = true
			require.Equal(t, expectedPrompt, p.SessionPrompt,
				"prompt should be read from v2 /main when v1 is absent")
			break
		}
	}
	require.True(t, found, "v2-only checkpoint should be discoverable in branch listing")
}

func TestGetBranchCheckpoints_V2PromptFallbackWhenV1Deleted(t *testing.T) {
	// When v2 is preferred and v1 metadata branch is deleted after dual-write,
	// prompts should still be readable from v2 /main.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("initial"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o644,
	))

	cpID := id.MustCheckpointID("aa11bb22cc33")
	expectedPrompt := "Dual-write prompt visible after v1 deletion"

	// Dual-write: checkpoint in both v1 and v2.
	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, "origin")
	require.NoError(t, v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		Prompts:      []string{expectedPrompt},
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	require.NoError(t, v2Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-dual",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")),
		Prompts:      []string{expectedPrompt},
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	// Create user commit with checkpoint trailer.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("updated"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	commitMsg := trailers.FormatCheckpoint("Dual-write commit", cpID)
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Delete the v1 metadata branch to simulate it being unavailable.
	require.NoError(t, repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)))

	// getBranchCheckpoints should still find the checkpoint and read prompt from v2.
	points, err := getBranchCheckpoints(context.Background(), repo, 10)
	require.NoError(t, err)

	var found bool
	for _, p := range points {
		if p.CheckpointID == cpID {
			found = true
			require.Equal(t, expectedPrompt, p.SessionPrompt,
				"prompt should be read from v2 /main after v1 deletion")
			break
		}
	}
	require.True(t, found, "checkpoint should be discoverable after v1 branch deletion")
}

func TestResolvePromptTree_PrefersV2WhenEnabled(t *testing.T) {
	t.Parallel()

	v1 := &object.Tree{}
	v2 := &object.Tree{}

	require.Same(t, v2, resolvePromptTree(v1, v2, true), "should prefer v2 when enabled")
	require.Same(t, v1, resolvePromptTree(v1, v2, false), "should prefer v1 when v2 disabled")
	require.Same(t, v1, resolvePromptTree(v1, nil, true), "should fall back to v1 when v2 is nil")
	require.Same(t, v2, resolvePromptTree(nil, v2, false), "should use v2 as last resort when v1 is nil")
	require.Nil(t, resolvePromptTree(nil, nil, true), "should return nil when both are nil")
}

func TestHasAnyChanges_FirstCommitReturnsTrue(t *testing.T) {
	// First commit (no parent) should always return true
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	if !hasAnyChanges(commit) {
		t.Error("hasAnyChanges() should return true for first commit (no parent)")
	}
}

func TestHasAnyChanges_MetadataOnlyChangeReturnsTrue(t *testing.T) {
	// Unlike hasCodeChanges, hasAnyChanges uses tree hash comparison and
	// does not filter out .entire/ metadata files. A metadata-only change
	// should return true because the tree hash differs from the parent's.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with only .entire/ metadata changes
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", "session-123")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write metadata file: %v", err)
	}
	if _, err := w.Add(".entire"); err != nil {
		t.Fatalf("failed to add .entire: %v", err)
	}
	commitHash, err := w.Commit("metadata only commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// hasAnyChanges compares tree hashes, so metadata-only changes DO count
	// (unlike hasCodeChanges which filters .entire/ files)
	if !hasAnyChanges(commit) {
		t.Error("hasAnyChanges() should return true for metadata-only changes (tree hash differs)")
	}
}

func TestHasAnyChanges_NoOpTreeChangeReturnsFalse(t *testing.T) {
	// When a commit has the same tree hash as its parent (no-op commit),
	// hasAnyChanges should return false
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	firstHash, err := w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create a second commit with the exact same tree (allow-empty equivalent)
	firstCommit, err := repo.CommitObject(firstHash)
	if err != nil {
		t.Fatalf("failed to get first commit: %v", err)
	}

	sig := object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}
	emptyCommit := object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      "no-op commit with same tree",
		TreeHash:     firstCommit.TreeHash,
		ParentHashes: []plumbing.Hash{firstHash},
	}
	obj := repo.Storer.NewEncodedObject()
	if err := emptyCommit.Encode(obj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}
	secondHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}

	secondCommit, err := repo.CommitObject(secondHash)
	if err != nil {
		t.Fatalf("failed to get second commit: %v", err)
	}

	// Same tree hash as parent → no changes
	if hasAnyChanges(secondCommit) {
		t.Error("hasAnyChanges() should return false when tree hash matches parent (no-op commit)")
	}
}

// createCommitWithTree creates a commit with a specific tree and parent hashes.
func createCommitWithTree(t *testing.T, repo *git.Repository, treeHash plumbing.Hash, parents []plumbing.Hash, message string) plumbing.Hash {
	t.Helper()
	sig := object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}
	commit := object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}
	return hash
}

func TestExtractIntent_PrefersScopedPrompt(t *testing.T) {
	t.Parallel()
	got := extractIntent([]string{"add explain --generate flag", "later prompt"}, "fallback prompt\nline2")
	want := "add explain --generate flag"
	if got != want {
		t.Errorf("extractIntent scoped\n got: %q\nwant: %q", got, want)
	}
}

func TestExtractIntent_FallsBackToFirstLineOfContent(t *testing.T) {
	t.Parallel()
	got := extractIntent(nil, "first content line\nsecond line")
	want := "first content line"
	if got != want {
		t.Errorf("extractIntent fallback\n got: %q\nwant: %q", got, want)
	}
}

func TestExtractIntent_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := extractIntent(nil, ""); got != "" {
		t.Errorf("extractIntent empty: got %q want empty", got)
	}
	if got := extractIntent([]string{""}, ""); got != "" {
		t.Errorf("extractIntent empty-string-prompt: got %q want empty", got)
	}
}

func TestExtractIntent_TruncatesLongPrompts(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 500)
	got := extractIntent([]string{long}, "")
	if len(got) >= len(long) {
		t.Errorf("expected truncation; got %d chars", len(got))
	}
}

func TestBuildNoSummaryMarkdown_IntentAndAffordance(t *testing.T) {
	t.Parallel()
	got := buildNoSummaryMarkdown("add explain --generate flag", nil, "Run `entire explain --generate abc`.")
	if !strings.Contains(got, "## Intent\n\nadd explain --generate flag\n") {
		t.Fatalf("missing intent section:\n%s", got)
	}
	// escapeSummaryText replaces every backtick with U+2018 (‘), so both
	// backticks in "Run `entire explain --generate abc`." map to ‘.
	if !strings.Contains(got, "## Summary\n\n*Run ‘entire explain --generate abc‘.*\n") {
		t.Fatalf("missing italic summary affordance:\n%s", got)
	}
	if strings.Contains(got, "## Files") {
		t.Fatalf("did not expect Files when files=nil:\n%s", got)
	}
}

func TestBuildNoSummaryMarkdown_RendersFilesWhenProvided(t *testing.T) {
	t.Parallel()
	got := buildNoSummaryMarkdown("intent", []string{"a.go", "b.go"}, "hint")
	if !strings.Contains(got, "## Files (2)\n\n- `a.go`\n- `b.go`\n") {
		t.Fatalf("expected Files section with count and list:\n%s", got)
	}
}

func TestBuildNoSummaryMarkdown_EmptyIntentShowsPlaceholder(t *testing.T) {
	t.Parallel()
	got := buildNoSummaryMarkdown("", nil, "hint")
	if !strings.Contains(got, "## Intent\n\n*(no prompt recorded)*\n") {
		t.Fatalf("expected italic placeholder:\n%s", got)
	}
}

func TestRenderExplainBody_NoColorReturnsRawMarkdown(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer // not a TTY → shouldUseColor false
	got := renderExplainBody(&buf, "## Intent\n\nfoo\n")
	if got != "## Intent\n\nfoo\n" {
		t.Errorf("expected raw markdown when no color\n got: %q", got)
	}
}
