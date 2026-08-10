package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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

func TestMaybeOfferSessionImport_FirstRunGate(t *testing.T) {
	// Not parallel: overrides package seams. No repo needed — the gate returns
	// before any discovery.
	called := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport {
			called = true
			return []eligibleImport{{displayName: "X", sessionCount: 1}}
		}, nil, nil)

	maybeOfferSessionImport(context.Background(), io.Discard, nil, EnableOptions{}, false /* firstRun */)
	if called {
		t.Error("discovery ran on a non-first-run enable; the offer must be gated to first run")
	}
}

func TestMaybeOfferSessionImport_NonInteractiveAutoImportsAll(t *testing.T) {
	// Not parallel: overrides seams and chdirs into a temp repo.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	eligible := []eligibleImport{
		{displayName: testAgentClaude, sessionCount: 3},
		{displayName: "Codex", sessionCount: 1},
	}
	var ran []eligibleImport
	promptCalled := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport { return eligible },
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) {
			promptCalled = true
			return nil, nil
		},
		func(_ context.Context, _ io.Writer, _ string, sel []eligibleImport) { ran = sel },
	)

	// opts.Yes forces the non-interactive path even if a TTY is present.
	maybeOfferSessionImport(context.Background(), io.Discard, nil, EnableOptions{Yes: true}, true)
	if promptCalled {
		t.Error("prompt shown under --yes; non-interactive enable must not prompt")
	}
	if len(ran) != len(eligible) {
		t.Fatalf("imported %d agents, want all %d", len(ran), len(eligible))
	}
}

func TestMaybeOfferSessionImport_NonInteractiveWithoutYesSkips(t *testing.T) {
	// Not parallel: overrides seams and chdirs into a temp repo.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	// No ENTIRE_TEST_TTY => CanPromptInteractively() is false (non-interactive),
	// e.g. a scripted or agent-driven enable.

	promptCalled := false
	var ran []eligibleImport
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport {
			return []eligibleImport{{displayName: testAgentClaude, sessionCount: 3}}
		},
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) {
			promptCalled = true
			return nil, nil
		},
		func(_ context.Context, _ io.Writer, _ string, sel []eligibleImport) { ran = sel },
	)

	// No --yes and no TTY: neither prompt nor auto-import; just hint at the
	// manual command.
	var buf bytes.Buffer
	maybeOfferSessionImport(context.Background(), &buf, nil, EnableOptions{}, true)
	if promptCalled {
		t.Error("prompt shown in a non-interactive context")
	}
	if len(ran) != 0 {
		t.Errorf("auto-imported %d agent(s) without --yes in a non-interactive context; expected skip", len(ran))
	}
	if got := buf.String(); !strings.Contains(got, "entire import") {
		t.Errorf("expected a pointer to 'entire import', got %q", got)
	}
}

func TestMaybeOfferSessionImport_NoEligibleIsNoOp(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	runCalled := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport { return nil },
		nil,
		func(context.Context, io.Writer, string, []eligibleImport) { runCalled = true },
	)

	maybeOfferSessionImport(context.Background(), io.Discard, nil, EnableOptions{Yes: true}, true)
	if runCalled {
		t.Error("import ran with nothing discoverable; expected a silent no-op")
	}
}

func TestMaybeOfferSessionImport_InteractiveUsesSelection(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	// Force interactive so the prompt branch is taken.
	t.Setenv("ENTIRE_TEST_TTY", "1")

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

	maybeOfferSessionImport(context.Background(), io.Discard, nil, EnableOptions{}, true)
	if len(ran) != 1 || ran[0].displayName != testAgentClaude {
		t.Fatalf("imported %+v, want only the user-selected Claude Code", ran)
	}
}

func TestMaybeOfferSessionImport_EmptySelectionSkips(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	runCalled := false
	withImportSeams(t,
		func(context.Context, []agent.Agent, string) []eligibleImport {
			return []eligibleImport{{displayName: testAgentClaude, sessionCount: 3}}
		},
		func(context.Context, io.Writer, []eligibleImport) ([]eligibleImport, error) { return nil, nil },
		func(context.Context, io.Writer, string, []eligibleImport) { runCalled = true },
	)

	maybeOfferSessionImport(context.Background(), io.Discard, nil, EnableOptions{}, true)
	if runCalled {
		t.Error("import ran after an empty selection; expected skip")
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

func TestMaybeOfferSessionImport_PromptErrorIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	t.Setenv("ENTIRE_TEST_TTY", "1")

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

	// A prompt failure must never fail enable: the offer is best-effort, so this
	// simply returns and does not panic or propagate.
	maybeOfferSessionImport(context.Background(), io.Discard, nil, EnableOptions{}, true)
	if runCalled {
		t.Error("import ran after a prompt error; expected skip")
	}
}

// fixedDiscoverImporter wraps a real agentimport.Importer but overrides
// Discover to return a fixed, caller-supplied set of session files instead of
// scanning the agent's real transcript directory. runSelectedImports (unlike
// the standalone `entire import` command) has no --path flag to redirect
// discovery, so this is the seam tests use to feed it a fixture.
type fixedDiscoverImporter struct {
	agentimport.Importer

	sessions []agentimport.SessionFile
}

func (f fixedDiscoverImporter) Discover(string, string, time.Time, []string) ([]agentimport.SessionFile, error) {
	return f.sessions, nil
}

// writeImportProgressFixtureSession writes a 2-turn Claude Code transcript
// fixture, matching the format agentimport's claude importer parses.
func writeImportProgressFixtureSession(t *testing.T, dir, name string) {
	t.Helper()
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	testutil.WriteFile(t, dir, name, content)
}

// TestRunSelectedImports_NonTTYProgressLines proves the wired-in progress
// reporter, running against a plain (non-terminal) writer, prints exactly one
// plain line per session — carrying the agent name, its position, and its
// turn count — writes no ANSI escapes, and leaves the pre-existing final
// summary line unchanged.
func TestRunSelectedImports_NonTTYProgressLines(t *testing.T) {
	// Not parallel: chdirs into a temp repo and performs real checkpoint writes.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	ctx := context.Background()

	sessionsDir := t.TempDir()
	writeImportProgressFixtureSession(t, sessionsDir, "sess1.jsonl")
	writeImportProgressFixtureSession(t, sessionsDir, "sess2.jsonl")

	var claudeImp agentimport.Importer
	for _, imp := range agentimport.All() {
		if imp.Name() == testAgentName {
			claudeImp = imp
		}
	}
	if claudeImp == nil {
		t.Fatal("claude-code importer not registered")
	}
	sessions, err := claudeImp.Discover(dir, sessionsDir, time.Now(), nil)
	if err != nil {
		t.Fatalf("discover fixture sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 fixture sessions, got %d", len(sessions))
	}
	imp := fixedDiscoverImporter{Importer: claudeImp, sessions: sessions}
	agentName := string(claudeImp.AgentType())

	var buf bytes.Buffer
	runSelectedImports(ctx, &buf, dir, []eligibleImport{{imp: imp, displayName: agentName}})
	out := buf.String()

	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("output contains an ESC byte on a non-TTY writer: %q", out)
	}

	wantLines := []string{
		fmt.Sprintf("Importing %s session 1/2 (2 turns)...", agentName),
		fmt.Sprintf("Importing %s session 2/2 (2 turns)...", agentName),
	}
	for _, line := range wantLines {
		if !strings.Contains(out, line) {
			t.Errorf("missing progress line %q in output:\n%s", line, out)
		}
	}
	if got := strings.Count(out, fmt.Sprintf("Importing %s session", agentName)); got != 2 {
		t.Errorf("got %d progress lines, want exactly 2 (one per session):\n%s", got, out)
	}

	if want := "Imported 4 turn(s) from 2 session(s) (0 already imported).\n"; !strings.Contains(out, want) {
		t.Errorf("final summary line missing or changed; want %q in:\n%s", want, out)
	}
}

// TestRunSelectedImports_NonTTYProgressLines_Reimport proves a second,
// idempotent pass over an already-imported corpus (every turn hits
// agentimport's TurnSkipped path, not TurnWritten) still prints one plain
// progress line per session and reports the correct "0 imported" summary —
// the non-TTY side of the bug the P2 Codex's pre-push review caught. The TTY
// side (a fully-skipped session must still sweep the spinner to turn M/M) is
// covered at the agentimport layer by
// TestRun_ReimportFiresTurnSkippedNotTurnWritten; the cli wiring it depends on
// — newImportProgressReporter routing TurnSkipped and TurnWritten through one
// shared advance path — has no direct test because the spinner branch only
// runs against a real terminal.
func TestRunSelectedImports_NonTTYProgressLines_Reimport(t *testing.T) {
	// Not parallel: chdirs into a temp repo and performs real checkpoint writes.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	ctx := context.Background()

	sessionsDir := t.TempDir()
	writeImportProgressFixtureSession(t, sessionsDir, "sess1.jsonl")
	writeImportProgressFixtureSession(t, sessionsDir, "sess2.jsonl")

	var claudeImp agentimport.Importer
	for _, imp := range agentimport.All() {
		if imp.Name() == testAgentName {
			claudeImp = imp
		}
	}
	if claudeImp == nil {
		t.Fatal("claude-code importer not registered")
	}
	sessions, err := claudeImp.Discover(dir, sessionsDir, time.Now(), nil)
	if err != nil {
		t.Fatalf("discover fixture sessions: %v", err)
	}
	agentName := string(claudeImp.AgentType())
	selected := []eligibleImport{{
		imp:         fixedDiscoverImporter{Importer: claudeImp, sessions: sessions},
		displayName: agentName,
	}}

	// First pass actually imports; discard its output.
	runSelectedImports(ctx, io.Discard, dir, selected)

	// Second pass: every turn is already imported, so agentimport.Run's loop
	// only ever calls TurnSkipped for it — this is the scenario that used to
	// leave a TTY reporter frozen at "turn 0/M".
	var buf bytes.Buffer
	runSelectedImports(ctx, &buf, dir, selected)
	out := buf.String()

	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("re-import output contains an ESC byte on a non-TTY writer: %q", out)
	}
	wantLines := []string{
		fmt.Sprintf("Importing %s session 1/2 (2 turns)...", agentName),
		fmt.Sprintf("Importing %s session 2/2 (2 turns)...", agentName),
	}
	for _, line := range wantLines {
		if !strings.Contains(out, line) {
			t.Errorf("missing progress line %q in re-import output:\n%s", line, out)
		}
	}
	if want := "Imported 0 turn(s) from 2 session(s) (4 already imported).\n"; !strings.Contains(out, want) {
		t.Errorf("re-import summary line missing or wrong; want %q in:\n%s", want, out)
	}
}

// cancelOnDiscoverImporter returns fixed sessions but cancels the run context
// as a side effect of Discover, so cancellation lands mid-import (after enable's
// setup has run under a live context) exactly as a real Ctrl-C would.
type cancelOnDiscoverImporter struct {
	agentimport.Importer

	sessions []agentimport.SessionFile
	cancel   context.CancelFunc
}

func (c cancelOnDiscoverImporter) Discover(string, string, time.Time, []string) ([]agentimport.SessionFile, error) {
	c.cancel()
	return c.sessions, nil
}

// recordDiscoverImporter records whether Discover was ever called, so a test can
// assert a later agent was never processed.
type recordDiscoverImporter struct {
	agentimport.Importer

	called *bool
}

func (r recordDiscoverImporter) Discover(string, string, time.Time, []string) ([]agentimport.SessionFile, error) {
	*r.called = true
	return nil, nil
}

// TestRunSelectedImports_CancelStopsImportPhase proves a cancelled context
// (Ctrl-C during enable auto-import) aborts the whole import phase after the
// current agent instead of walking every remaining agent — each of which would
// otherwise run Discover and emit its own "context canceled" note.
func TestRunSelectedImports_CancelStopsImportPhase(t *testing.T) {
	// Not parallel: chdirs into a temp repo and performs real checkpoint writes.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	sessionsDir := t.TempDir()
	writeImportProgressFixtureSession(t, sessionsDir, "sess1.jsonl")

	var claudeImp agentimport.Importer
	for _, imp := range agentimport.All() {
		if imp.Name() == testAgentName {
			claudeImp = imp
		}
	}
	if claudeImp == nil {
		t.Fatal("claude-code importer not registered")
	}
	sessions, err := claudeImp.Discover(dir, sessionsDir, time.Now(), nil)
	if err != nil {
		t.Fatalf("discover fixture sessions: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	first := cancelOnDiscoverImporter{Importer: claudeImp, sessions: sessions, cancel: cancel}
	secondCalled := false
	second := recordDiscoverImporter{Importer: claudeImp, called: &secondCalled}

	var buf bytes.Buffer
	runSelectedImports(ctx, &buf, dir, []eligibleImport{
		{imp: first, displayName: "first"},
		{imp: second, displayName: "second"},
	})
	out := buf.String()

	if secondCalled {
		t.Error("cancelled import must abort the phase; the second agent must not be processed")
	}
	if got := strings.Count(out, "Import cancelled."); got != 1 {
		t.Errorf("want exactly one cancellation note, got %d:\n%s", got, out)
	}
	if strings.Contains(out, "Imported ") {
		t.Errorf("cancelled import must not print a success summary:\n%s", out)
	}
}
