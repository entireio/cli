package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestRunSelectedImports_PreservesTurnAnchorSelection(t *testing.T) {
	// Not parallel: onboarding resolves repository and state from CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "first")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "first")
	recorded := testutil.GetHeadHash(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "second")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "second")
	fallback := testutil.GetHeadHash(t, dir)
	t.Chdir(dir)

	transcripts := t.TempDir()
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		fmt.Sprintf(`{"type":"user","toolUseResult":{"gitOperation":{"commit":{"sha":%q,"kind":"committed"}}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"committed"}]}}`, recorded),
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	testutil.WriteFile(t, transcripts, "mixed.jsonl", content)
	imp := importerForAgent(fakeAgent{typ: testAgentClaude})
	require.NotNil(t, imp)
	selected := fixedDiscoverImporter{Importer: imp, sessions: []agentimport.SessionFile{{
		Path: filepath.Join(transcripts, "mixed.jsonl"), SessionID: "mixed",
	}}}
	var out bytes.Buffer
	runSelectedImports(context.Background(), &out, dir, fallback, []eligibleImport{{imp: selected, displayName: "Claude Code"}})
	require.Contains(t, out.String(), "Imported 2 turn(s)")
	for turn, want := range map[string]string{"u1": recorded, "u2": fallback} {
		cid := agentimport.DeriveCheckpointID("mixed", turn)
		for _, suffix := range []string{"/metadata.json", "/0/metadata.json"} {
			raw := testutil.RunGit(t, dir, "show", "entire/checkpoints/v1:"+cid.Path()+suffix)
			var metadata struct {
				CommitSHA string `json:"commit_sha"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
			require.Equal(t, want, metadata.CommitSHA, "%s%s", cid, suffix)
		}
	}
}

// An anchorless repo must not be ASKED whether to import. Every imported
// checkpoint needs a validated anchor, so a repo that cannot produce one has
// only one honest answer, and the previous flow discovered history, prompted,
// took the user's "yes", and then discarded it with a note. Discovery is
// allowed to run (it is read-only, and skipping it would make a repo with no
// history print a skip notice it does not need); the prompt is the thing that
// must not happen.
func TestMaybeOfferSessionImport_AnchorlessRepoNeverPrompts(t *testing.T) {
	// Not parallel: overrides package seams and chdirs.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	// Force interactive, or the prompt seam below is unreachable and its
	// t.Fatal is decoration: a non-interactive run returns at the no-opt-in
	// branch before the prompt whether or not the anchor gate exists, so the
	// test would pin only the notice and not the thing it is named for.
	t.Setenv("ENTIRE_TEST_TTY", "1")

	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport {
			return []eligibleImport{{displayName: "Claude Code", sessionCount: 1}}
		},
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) {
			t.Fatal("an anchorless repo must not be asked whether to import")
			return nil, nil
		},
		func(context.Context, io.Writer, string, string, []eligibleImport) {
			t.Fatal("an anchorless repo must not reach the importer")
		})

	var out bytes.Buffer
	maybeOfferSessionImport(context.Background(), &out, nil, EnableOptions{}, true)

	require.Contains(t, out.String(), "skipping agent history import")
	require.Contains(t, out.String(), "without a valid anchor commit")
	require.Contains(t, out.String(), "entire import <agent>")
	require.NotContains(t, out.String(), "Imported ")
	require.Empty(t, testutil.RunGit(t, dir, "for-each-ref", "--format=%(refname)", "refs/entire", "refs/heads/entire"))
}
