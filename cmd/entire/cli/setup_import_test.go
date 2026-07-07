package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
)

// fakeAgent satisfies agent.Agent via an embedded nil interface; only Type() is
// implemented because that is all the import-offer code calls. Calling any other
// method would panic, which is the intended guard.
type fakeAgent struct {
	agent.Agent

	typ types.AgentType
}

func (f fakeAgent) Type() types.AgentType { return f.typ }

func TestPluralSessions(t *testing.T) {
	t.Parallel()
	cases := map[int]string{0: "0 sessions", 1: "1 session", 2: "2 sessions", 42: "42 sessions"}
	for n, want := range cases {
		if got := pluralSessions(n); got != want {
			t.Errorf("pluralSessions(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestImporterForAgent_MatchesByType(t *testing.T) {
	t.Parallel()
	// Every registered importer must be resolvable from an agent carrying the
	// same AgentType — this is the contract the offer relies on.
	for _, imp := range agentimport.All() {
		ag := fakeAgent{typ: imp.AgentType()}
		got := importerForAgent(ag)
		if got == nil {
			t.Errorf("importerForAgent(%q) = nil, want importer %q", imp.AgentType(), imp.Name())
			continue
		}
		if got.Name() != imp.Name() {
			t.Errorf("importerForAgent(%q) = %q, want %q", imp.AgentType(), got.Name(), imp.Name())
		}
	}
}

func TestImporterForAgent_UnknownTypeReturnsNil(t *testing.T) {
	t.Parallel()
	if got := importerForAgent(fakeAgent{typ: "Definitely Not A Real Agent"}); got != nil {
		t.Errorf("importerForAgent(unknown) = %q, want nil", got.Name())
	}
}

// withImportSeams overrides the package seams and restores them after the test.
// Tests using it must not call t.Parallel (shared package state).
func withImportSeams(t *testing.T, discover func(context.Context, []agent.Agent, string) []eligibleImport, prompt func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error), run func(context.Context, io.Writer, string, []eligibleImport)) {
	t.Helper()
	oldDiscover, oldPrompt, oldRun := sessionImportDiscover, sessionImportPrompt, sessionImportRun
	t.Cleanup(func() {
		sessionImportDiscover, sessionImportPrompt, sessionImportRun = oldDiscover, oldPrompt, oldRun
	})
	if discover != nil {
		sessionImportDiscover = discover
	}
	if prompt != nil {
		sessionImportPrompt = prompt
	}
	if run != nil {
		sessionImportRun = run
	}
}

// The ladder replaced #1595's first-run gate with ground truth: the import
// rung recomputes "unimported history exists" every enable, so re-enabling
// after a full import is a no-op without any persisted gate. These tests
// cover the offer composition (runOnboardingImport) over the same seams.

func TestRunOnboardingImport_NonGranularImportsAll(t *testing.T) {
	// Not parallel: overrides seams and chdirs into a temp repo.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	eligible := []eligibleImport{
		{displayName: testAgentClaude, sessionCount: 3},
		{displayName: "Codex", sessionCount: 1},
	}
	var ran []eligibleImport
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport { return eligible },
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) {
			t.Error("prompt shown for a non-granular import (mode-all / --yes)")
			return nil, nil
		},
		func(_ context.Context, _ io.Writer, _ string, sel []eligibleImport) { ran = sel },
	)

	if err := runOnboardingImport(context.Background(), io.Discard, false, nil); err != nil {
		t.Fatalf("runOnboardingImport() error = %v", err)
	}
	if len(ran) != len(eligible) {
		t.Fatalf("imported %d agents, want all %d", len(ran), len(eligible))
	}
}

func TestRunOnboardingImport_NoEligibleIsNoOp(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	runCalled := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport { return nil },
		nil,
		func(context.Context, io.Writer, string, []eligibleImport) { runCalled = true },
	)

	if err := runOnboardingImport(context.Background(), io.Discard, false, nil); err != nil {
		t.Fatalf("runOnboardingImport() error = %v", err)
	}
	if runCalled {
		t.Error("import ran with nothing discoverable; expected a silent no-op")
	}
}

func TestRunOnboardingImport_GranularUsesSelection(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	eligible := []eligibleImport{
		{displayName: testAgentClaude, sessionCount: 3},
		{displayName: "Codex", sessionCount: 1},
	}
	var ran []eligibleImport
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport { return eligible },
		func(_ context.Context, _ io.Writer, e []eligibleImport) ([]eligibleImport, error) {
			return e[:1], nil // user picks only the first
		},
		func(_ context.Context, _ io.Writer, _ string, sel []eligibleImport) { ran = sel },
	)

	if err := runOnboardingImport(context.Background(), io.Discard, true, nil); err != nil {
		t.Fatalf("runOnboardingImport() error = %v", err)
	}
	if len(ran) != 1 || ran[0].displayName != testAgentClaude {
		t.Fatalf("imported %+v, want only the user-selected Claude Code", ran)
	}
}

func TestRunOnboardingImport_EmptySelectionSkips(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	runCalled := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport {
			return []eligibleImport{{displayName: testAgentClaude, sessionCount: 3}}
		},
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) { return nil, nil },
		func(context.Context, io.Writer, string, []eligibleImport) { runCalled = true },
	)

	if err := runOnboardingImport(context.Background(), io.Discard, true, nil); err != nil {
		t.Fatalf("runOnboardingImport() error = %v", err)
	}
	if runCalled {
		t.Error("import ran after an empty selection; expected skip")
	}
}

func TestRunOnboardingImport_PromptErrorReturnsError(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	runCalled := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport {
			return []eligibleImport{{displayName: testAgentClaude, sessionCount: 3}}
		},
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) {
			return nil, errors.New("terminal exploded")
		},
		func(context.Context, io.Writer, string, []eligibleImport) { runCalled = true },
	)

	// The offer surfaces the error; the runner downgrades it to a printed
	// notice, so enable still never fails.
	if err := runOnboardingImport(context.Background(), io.Discard, true, nil); err == nil {
		t.Error("want an error from a failed selection prompt")
	}
	if runCalled {
		t.Error("import ran after a prompt error; expected skip")
	}
}

func TestRunSelectedImports_UnsatisfiablePolicySkips(t *testing.T) {
	// Not parallel: chdirs into a temp repo and reads CWD-based git state.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	ctx := context.Background()

	// Install a checkpoint policy this CLI cannot satisfy (a future format).
	// The gate must skip the import, matching the standalone `entire import`
	// command's ensureCheckpointPolicyAllowsCheckpointData check.
	repo, err := openRepository(ctx)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	future := checkpointpolicy.Policy{CheckpointVersion: "branch-v99", CheckpointMinVersion: "branch-v99"}
	if _, err := checkpointpolicy.WriteLocal(ctx, repo, plumbing.ZeroHash, future); err != nil {
		t.Fatalf("write local policy: %v", err)
	}
	repo.Close()

	// A nil importer would panic if the import loop ran, so the gate returning
	// before the loop is exactly what keeps this from blowing up.
	var buf bytes.Buffer
	runSelectedImports(ctx, &buf, dir, []eligibleImport{{displayName: testAgentClaude}})

	if got := buf.String(); !strings.Contains(got, "skipping agent history import") {
		t.Errorf("expected a skip note for an unsatisfiable checkpoint policy, got %q", got)
	}
}
