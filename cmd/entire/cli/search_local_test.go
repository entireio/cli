package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
)

// localFixture describes a single-session checkpoint written into a test
// repo. It exists so individual cases can spell out only the fields they
// care about while the helper fills in sensible defaults for the rest.
type localFixture struct {
	id           string
	branch       string
	filesTouched []string
	prompt       string
	transcript   string
}

// makeLocalSearchRepo initializes a git repo with one user commit and the
// given checkpoints written to entire/checkpoints/v1. Tests exercise the
// real on-disk store rather than a mock, so regressions in either the
// matcher or the underlying ReadCommitted/ReadSessionContent plumbing are
// caught here.
func makeLocalSearchRepo(t *testing.T, fixtures []localFixture) *checkpoint.GitStore {
	t.Helper()

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	store := checkpoint.NewGitStore(repo)

	for _, f := range fixtures {
		err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID: id.MustCheckpointID(f.id),
			SessionID:    "session-" + f.id,
			Strategy:     "manual-commit",
			Branch:       f.branch,
			FilesTouched: f.filesTouched,
			Prompts:      []string{f.prompt},
			Transcript:   redact.AlreadyRedacted([]byte(f.transcript)),
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted %s: %v", f.id, err)
		}
	}

	return store
}

func TestLocalSearch_FindsByTranscriptToken(t *testing.T) {
	t.Parallel()

	store := makeLocalSearchRepo(t, []localFixture{
		{
			id:           "a1b2c3d4e5f6",
			branch:       "main",
			filesTouched: []string{"src/auth.go"},
			prompt:       "Fix the login flow",
			transcript:   "assistant: I rewrote the Lefthook config to call entire instead.\n",
		},
		{
			id:           "b2c3d4e5f6a7",
			branch:       "main",
			filesTouched: []string{"docs/intro.md"},
			prompt:       "Add intro docs",
			transcript:   "assistant: drafted an intro section about onboarding.\n",
		},
	})

	resp, err := localSearchWithStore(context.Background(), store, localSearchInput{Query: "lefthook"})
	if err != nil {
		t.Fatalf("localSearchWithStore error: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("expected 1 match, got %d (results=%v)", resp.Total, resp.Results)
	}
	got := resp.Results[0].Data.ID
	if got != "a1b2c3d4e5f6" {
		t.Errorf("matched checkpoint = %q, want %q", got, "a1b2c3d4e5f6")
	}
	if resp.Results[0].Meta.MatchType != "local" {
		t.Errorf("meta.matchType = %q, want %q", resp.Results[0].Meta.MatchType, "local")
	}
	if resp.Results[0].Meta.Snippet == "" {
		t.Error("expected a non-empty snippet for matched result")
	}
}

func TestLocalSearch_FindsByFilesTouched(t *testing.T) {
	t.Parallel()

	store := makeLocalSearchRepo(t, []localFixture{
		{
			id:           "a1b2c3d4e5f6",
			branch:       "feature",
			filesTouched: []string{"app/login_handler.go"},
			prompt:       "Refactor",
			transcript:   "assistant: refactored handler.\n",
		},
	})

	resp, err := localSearchWithStore(context.Background(), store, localSearchInput{Query: "login_handler"})
	if err != nil {
		t.Fatalf("localSearchWithStore error: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("expected 1 match for files_touched hit, got %d", resp.Total)
	}
}

func TestLocalSearch_BranchFilter(t *testing.T) {
	t.Parallel()

	store := makeLocalSearchRepo(t, []localFixture{
		{
			id:         "a1b2c3d4e5f6",
			branch:     "main",
			prompt:     "Touched main",
			transcript: "assistant: touched something on main.\n",
		},
		{
			id:         "b2c3d4e5f6a7",
			branch:     "feature",
			prompt:     "Touched feature",
			transcript: "assistant: touched something on feature.\n",
		},
	})

	resp, err := localSearchWithStore(context.Background(), store, localSearchInput{
		Branch: "feature",
	})
	if err != nil {
		t.Fatalf("localSearchWithStore error: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("expected 1 match on feature branch, got %d", resp.Total)
	}
	if resp.Results[0].Data.Branch != "feature" {
		t.Errorf("result branch = %q, want %q", resp.Results[0].Data.Branch, "feature")
	}
}

func TestLocalSearch_EmptyQueryMatchesAll(t *testing.T) {
	t.Parallel()

	store := makeLocalSearchRepo(t, []localFixture{
		{id: "a1b2c3d4e5f6", branch: "main", prompt: "one", transcript: "assistant: one.\n"},
		{id: "b2c3d4e5f6a7", branch: "main", prompt: "two", transcript: "assistant: two.\n"},
	})

	resp, err := localSearchWithStore(context.Background(), store, localSearchInput{})
	if err != nil {
		t.Fatalf("localSearchWithStore error: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("empty query: expected 2 matches, got %d", resp.Total)
	}
}

func TestLocalSearch_NoMatches(t *testing.T) {
	t.Parallel()

	store := makeLocalSearchRepo(t, []localFixture{
		{
			id:         "a1b2c3d4e5f6",
			branch:     "main",
			prompt:     "hello",
			transcript: "assistant: hello world\n",
		},
	})

	resp, err := localSearchWithStore(context.Background(), store, localSearchInput{Query: "kubernetes"})
	if err != nil {
		t.Fatalf("localSearchWithStore error: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("expected 0 matches, got %d", resp.Total)
	}
}

func TestWriteLocalFallbackHint_PrintsWhenRemoteEmptyAndLocalHasCheckpoints(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeLocalFallbackHint(&buf, &search.Response{Total: 0}, "lefthook", 3)

	out := buf.String()
	if !strings.Contains(out, "0 results from the search service") {
		t.Fatalf("hint missing service-empty phrase: %q", out)
	}
	if !strings.Contains(out, "3 local checkpoint") {
		t.Fatalf("hint missing local count: %q", out)
	}
	if !strings.Contains(out, "--local \"lefthook\"") {
		t.Fatalf("hint missing suggested --local invocation: %q", out)
	}
}

func TestWriteLocalFallbackHint_OmitsQuotesForEmptyQuery(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeLocalFallbackHint(&buf, &search.Response{Total: 0}, "   ", 1)

	out := buf.String()
	if !strings.Contains(out, "--local`") {
		t.Fatalf("hint should suggest bare --local for empty query: %q", out)
	}
	if strings.Contains(out, "\"\"") {
		t.Fatalf("hint should not include empty quoted arg: %q", out)
	}
}

func TestWriteLocalFallbackHint_SilentWhenResultsExist(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeLocalFallbackHint(&buf, &search.Response{Total: 5}, "anything", 10)

	if buf.Len() != 0 {
		t.Fatalf("expected no output when remote returned results, got %q", buf.String())
	}
}

func TestWriteLocalFallbackHint_SilentWhenNoLocalCheckpoints(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeLocalFallbackHint(&buf, &search.Response{Total: 0}, "anything", 0)

	if buf.Len() != 0 {
		t.Fatalf("expected no output when local store is empty, got %q", buf.String())
	}
}

func TestWriteLocalFallbackHint_SilentOnNilResponse(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeLocalFallbackHint(&buf, nil, "anything", 5)

	if buf.Len() != 0 {
		t.Fatalf("expected no output on nil response, got %q", buf.String())
	}
}

func TestMakeLocalSnippet_WindowsAroundMatch(t *testing.T) {
	t.Parallel()

	text := "prefix prefix prefix needle suffix suffix suffix"
	snippet := makeLocalSnippet(text, len("prefix prefix prefix "), len("needle"))
	if snippet == "" {
		t.Fatal("expected non-empty snippet")
	}
	if !strings.Contains(snippet, "needle") {
		t.Errorf("snippet missing match token: %q", snippet)
	}
}
