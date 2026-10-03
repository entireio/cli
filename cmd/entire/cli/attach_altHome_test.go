package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// altHomeAgent wraps a real AgentHomeProvider agent (e.g. Claude Code) to pin
// its "active" SessionHome and GetSessionBaseDir to test-controlled
// directories, bypassing CLAUDE_CONFIG_DIR resolution and the opt-in CLI
// probe entirely (both of which are process-global state unsafe to depend on
// in a test — this machine may have a real `claude` binary on PATH).
// SessionPathUnder and SessionBaseDirUnder are promoted unchanged from the
// embedded provider: both are documented pure path arithmetic with no
// environment access, so they stay correct under the fake home.
type altHomeAgent struct {
	agent.AgentHomeProvider

	baseDir string
	home    string
}

// GetSessionBaseDir and SessionHome always return a nil error: their
// interfaces (SessionBaseDirProvider, AgentHomeProvider) both allow failure,
// but this test double's directories are always valid test fixtures.
func (a *altHomeAgent) GetSessionDir(_ string) (string, error) {
	return filepath.Join(a.baseDir, "active-project"), nil
}
func (a *altHomeAgent) GetSessionBaseDir() (string, error) { return a.baseDir, nil } //nolint:unparam // interface requires an error return
func (a *altHomeAgent) SessionHome() (string, error)       { return a.home, nil }    //nolint:unparam // interface requires an error return

// newAltHomeAgent builds an altHomeAgent wrapping Claude Code with the given
// active home. baseDir is derived the same way ClaudeCodeAgent.GetSessionBaseDir
// derives it (home/projects), matching production.
func newAltHomeAgent(t *testing.T, home string) *altHomeAgent {
	t.Helper()
	base, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := agent.AsAgentHomeProvider(base)
	if !ok {
		t.Fatal("claude-code does not implement AgentHomeProvider")
	}
	return &altHomeAgent{
		AgentHomeProvider: provider,
		baseDir:           filepath.Join(home, "projects"),
		home:              home,
	}
}

// writeClaudeTranscriptUnder writes a minimal transcript for sessionID under
// home, following Claude Code's <home>/projects/<project>/<id>.jsonl layout
// (an arbitrary project subdirectory name — the search walks every
// subdirectory of the base dir, it does not depend on sanitized-cwd naming).
func writeClaudeTranscriptUnder(t *testing.T, home, sessionID string) string {
	t.Helper()
	projectDir := filepath.Join(home, "projects", "some-project")
	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolateAgentHomesRegistry points ENTIRE_CONFIG_DIR at a fresh t.TempDir()
// so KnownAgentHomes/RememberAgentHome never touch the real config
// directory or another test's recorded homes (the per-process go-test
// fallback in internal/testdirs is shared across the whole test binary).
func isolateAgentHomesRegistry(t *testing.T) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
}

// TestResolveAndValidateTranscript_FindsTranscriptUnderKnownNonActiveHome is
// the feature this change adds: a transcript that exists only under a home
// Entire previously recorded for this agent, not under the active home the
// current process resolved. Before this change searchTranscriptInProjectDirs
// only ever walked the active base directory, so this test fails without it.
func TestResolveAndValidateTranscript_FindsTranscriptUnderKnownNonActiveHome(t *testing.T) {
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)

	const sessionID = "test-alt-home-session"
	activeHome := t.TempDir()
	altHome := t.TempDir()

	ag := newAltHomeAgent(t, activeHome)
	if err := agent.RememberAgentHome(ag.Type(), altHome); err != nil {
		t.Fatal(err)
	}
	wantPath := writeClaudeTranscriptUnder(t, altHome, sessionID)

	gotPath, gotAltHome, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupAllowFetch)
	if err != nil {
		t.Fatalf("resolveAndValidateTranscript() error = %v, want the transcript found under altHome", err)
	}
	if gotPath != wantPath {
		t.Errorf("resolveAndValidateTranscript() path = %q, want %q", gotPath, wantPath)
	}
	if gotAltHome != filepath.Clean(altHome) {
		t.Errorf("resolveAndValidateTranscript() altHome = %q, want %q", gotAltHome, filepath.Clean(altHome))
	}
}

// TestResolveAndValidateTranscript_ActiveHomeWinsOverKnownHome pins that a
// recorded non-active home never shadows the active one: when the session
// exists under both, attach must still resolve the active copy, and the
// alt-home notice must not fire (gotAltHome == "").
func TestResolveAndValidateTranscript_ActiveHomeWinsOverKnownHome(t *testing.T) {
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)

	const sessionID = "test-active-wins-session"
	activeHome := t.TempDir()
	altHome := t.TempDir()

	ag := newAltHomeAgent(t, activeHome)
	if err := agent.RememberAgentHome(ag.Type(), altHome); err != nil {
		t.Fatal(err)
	}
	wantPath := writeClaudeTranscriptUnder(t, activeHome, sessionID)
	writeClaudeTranscriptUnder(t, altHome, sessionID) // also present under altHome — must not win

	gotPath, gotAltHome, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupAllowFetch)
	if err != nil {
		t.Fatalf("resolveAndValidateTranscript() error = %v", err)
	}
	if gotPath != wantPath {
		t.Errorf("resolveAndValidateTranscript() path = %q, want the ACTIVE copy %q", gotPath, wantPath)
	}
	if gotAltHome != "" {
		t.Errorf("resolveAndValidateTranscript() altHome = %q, want \"\" (active home hit, no notice)", gotAltHome)
	}
}

// TestResolveAndValidateTranscript_NoRecordedHomes_MatchesPriorBehavior is
// the regression guard for every single-profile user: with nothing ever
// recorded in the registry, resolution must behave exactly as it did before
// this change — found only when present under the active base dir, "" for
// altHome either way.
func TestResolveAndValidateTranscript_NoRecordedHomes_MatchesPriorBehavior(t *testing.T) {
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)

	const sessionID = "test-no-known-homes-session"
	activeHome := t.TempDir()
	ag := newAltHomeAgent(t, activeHome)

	// Not found anywhere: error names the (single) directory searched, same
	// shape as before this change.
	_, _, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupAllowFetch)
	if err == nil {
		t.Fatal("expected a not-found error with nothing recorded and no transcript present")
	}
	if !strings.Contains(err.Error(), filepath.Join(activeHome, "projects")) {
		t.Errorf("error = %v, want it to name the active base directory", err)
	}

	// Found under the active dir: works exactly as before, no notice.
	wantPath := writeClaudeTranscriptUnder(t, activeHome, sessionID)
	gotPath, gotAltHome, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupAllowFetch)
	if err != nil {
		t.Fatalf("resolveAndValidateTranscript() error = %v", err)
	}
	if gotPath != wantPath {
		t.Errorf("resolveAndValidateTranscript() path = %q, want %q", gotPath, wantPath)
	}
	if gotAltHome != "" {
		t.Errorf("resolveAndValidateTranscript() altHome = %q, want \"\"", gotAltHome)
	}
}

// TestSearchTranscriptInProjectDirs_SkipsGoneHomeWithoutError covers a home
// recorded in a previous session whose directory has since been removed
// (e.g. a wrapper script's relocation directory was deleted): it must be
// skipped silently, not surfaced as a search error, and a still-present home
// recorded alongside it must still be searched.
func TestSearchTranscriptInProjectDirs_SkipsGoneHomeWithoutError(t *testing.T) {
	isolateAgentHomesRegistry(t)

	const sessionID = "test-gone-home-session"
	activeHome := t.TempDir()
	goneHome := filepath.Join(t.TempDir(), "deleted-wrapper-home")
	presentHome := t.TempDir()

	ag := newAltHomeAgent(t, activeHome)
	if err := agent.RememberAgentHome(ag.Type(), goneHome); err != nil {
		t.Fatal(err)
	}
	if err := agent.RememberAgentHome(ag.Type(), presentHome); err != nil {
		t.Fatal(err)
	}
	wantPath := writeClaudeTranscriptUnder(t, presentHome, sessionID)

	gotPath, gotAltHome, err := searchTranscriptInProjectDirs(sessionID, ag)
	if err != nil {
		t.Fatalf("searchTranscriptInProjectDirs() error = %v, want the gone home skipped silently", err)
	}
	if gotPath != wantPath {
		t.Errorf("searchTranscriptInProjectDirs() path = %q, want %q", gotPath, wantPath)
	}
	if gotAltHome != filepath.Clean(presentHome) {
		t.Errorf("searchTranscriptInProjectDirs() altHome = %q, want %q", gotAltHome, filepath.Clean(presentHome))
	}
}

// TestSearchTranscriptInProjectDirs_TieBreaksLeastRecentlyUsedHomeFirst pins the
// order KnownAgentHomes documents ("least-recently-used first") as an observable
// behavior, not just a doc comment: when a session with the same ID exists
// under two different non-active homes — e.g. a transcript directory copied
// wholesale into a newer relocation wrapper, keeping the same session IDs —
// the least recently used home must win, deterministically,
// every run. searchTranscriptInProjectDirs itself provides the other half
// (first candidate with a hit wins); this test exercises both together so a
// change to either silently flipping the tie-break is caught here, not left
// as an unobserved property of two separate functions.
func TestSearchTranscriptInProjectDirs_TieBreaksLeastRecentlyUsedHomeFirst(t *testing.T) {
	isolateAgentHomesRegistry(t)

	const sessionID = "test-tie-break-session"
	activeHome := t.TempDir()
	olderHome := t.TempDir()
	newerHome := t.TempDir()

	ag := newAltHomeAgent(t, activeHome)
	// Recording order is the tie-break: olderHome first, newerHome second.
	if err := agent.RememberAgentHome(ag.Type(), olderHome); err != nil {
		t.Fatal(err)
	}
	if err := agent.RememberAgentHome(ag.Type(), newerHome); err != nil {
		t.Fatal(err)
	}
	wantPath := writeClaudeTranscriptUnder(t, olderHome, sessionID)
	writeClaudeTranscriptUnder(t, newerHome, sessionID) // same session ID, must not win

	homes := agent.KnownAgentHomes(ag.Type())
	if len(homes) != 2 || homes[0] != filepath.Clean(olderHome) || homes[1] != filepath.Clean(newerHome) {
		t.Fatalf("KnownAgentHomes() = %v, want [%q, %q] (least-recently-used first)", homes, filepath.Clean(olderHome), filepath.Clean(newerHome))
	}

	gotPath, gotAltHome, err := searchTranscriptInProjectDirs(sessionID, ag)
	if err != nil {
		t.Fatalf("searchTranscriptInProjectDirs() error = %v", err)
	}
	if gotPath != wantPath {
		t.Errorf("searchTranscriptInProjectDirs() path = %q, want the OLDER-recorded home's copy %q", gotPath, wantPath)
	}
	if gotAltHome != filepath.Clean(olderHome) {
		t.Errorf("searchTranscriptInProjectDirs() altHome = %q, want %q (least-recently-used home)", gotAltHome, filepath.Clean(olderHome))
	}
}

func TestPrintAltHomeNotice(t *testing.T) {
	t.Parallel()

	t.Run("no notice for the active home", func(t *testing.T) {
		t.Parallel()
		var out strings.Builder
		printAltHomeNotice(&out, "")
		if out.Len() != 0 {
			t.Errorf("printAltHomeNotice(\"\") wrote %q, want nothing", out.String())
		}
	})

	t.Run("states the home as a verified fact, never a command", func(t *testing.T) {
		t.Parallel()
		var out strings.Builder
		const home = "/home/u/.claude-work"
		printAltHomeNotice(&out, home)
		got := out.String()
		if !strings.Contains(got, home) {
			t.Errorf("printAltHomeNotice(%q) = %q, want it to name the home", home, got)
		}
		if strings.Contains(got, "entire session resume") || strings.Contains(got, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("printAltHomeNotice(%q) = %q, must not suggest a command", home, got)
		}
	})
}

func TestSearchTranscriptInProjectDirs_SkipsUnsafeCandidates(t *testing.T) {
	for _, name := range []types.AgentName{agent.AgentNameClaudeCode, agent.AgentNameCopilotCLI} {
		t.Run(string(name), func(t *testing.T) {
			isolateAgentHomesRegistry(t)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("COPILOT_HOME", t.TempDir())
			ag, err := agent.Get(name)
			require.NoError(t, err)
			provider, ok := agent.AsAgentHomeProvider(ag)
			require.True(t, ok)
			first, second := t.TempDir(), t.TempDir()
			require.NoError(t, agent.RememberAgentHome(ag.Type(), first))
			require.NoError(t, agent.RememberAgentHome(ag.Type(), second))
			const id = "safe-candidate"
			rel := filepath.Join("projects", "project", id+".jsonl")
			if name == agent.AgentNameCopilotCLI {
				rel = filepath.Join("session-state", id, "events.jsonl")
			}
			testutil.WriteFile(t, second, rel, "{}\n")
			firstPath, secondPath := filepath.Join(first, rel), filepath.Join(second, rel)
			require.NoError(t, os.MkdirAll(filepath.Dir(firstPath), 0o700))
			testutil.SkipWithoutSymlinks(t)
			require.NoError(t, os.Symlink(secondPath, firstPath))
			path, home, err := searchTranscriptInProjectDirs(id, ag)
			require.NoError(t, err)
			require.Equal(t, secondPath, path)
			require.Equal(t, second, home)
			require.True(t, agent.HomeConfinesTranscript(provider, home, path))
		})
	}
}

// TestResolveAndValidateTranscript_FindsCopilotTranscriptUnderKnownHome covers
// agents whose layout is keyed by session ID alone (Copilot CLI, Codex). They
// have no per-project directories to walk, so the fallback search used to
// return before trying any recorded home.
func TestResolveAndValidateTranscript_FindsCopilotTranscriptUnderKnownHome(t *testing.T) {
	isolateAgentHomesRegistry(t)
	activeHome := t.TempDir()
	t.Setenv("COPILOT_HOME", activeHome)
	t.Setenv("ENTIRE_TEST_COPILOT_SESSION_DIR", "")
	setupAttachTestRepo(t)

	const sessionID = "test-copilot-alt-home"
	altHome := t.TempDir()
	ag, err := agent.Get(agent.AgentNameCopilotCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.RememberAgentHome(ag.Type(), altHome); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(altHome, "session-state", sessionID)
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(sessionDir, "events.jsonl")
	if err := os.WriteFile(wantPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gotPath, gotAltHome, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupLocalOnly)
	if err != nil {
		t.Fatalf("resolveAndValidateTranscript() error = %v, want the transcript found under the recorded home", err)
	}
	if gotPath != wantPath {
		t.Errorf("resolveAndValidateTranscript() path = %q, want %q", gotPath, wantPath)
	}
	if gotAltHome != filepath.Clean(altHome) {
		t.Errorf("resolveAndValidateTranscript() altHome = %q, want %q", gotAltHome, filepath.Clean(altHome))
	}
}

// TestReadAttachTranscript_ConfinesAlternateHome pins that a transcript found
// under a recorded alternate home is read through the confined reader, so a
// symlinked leaf there cannot redirect the read, and that the home is the one
// attach persists.
func TestReadAttachTranscript_ConfinesAlternateHome(t *testing.T) {
	t.Parallel()

	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	altHome := t.TempDir()
	path := writeClaudeTranscriptUnder(t, altHome, "test-confined-attach")
	if got := attachAgentHome(ag, path, altHome); got != altHome {
		t.Fatalf("attachAgentHome() = %q, want the alternate home %q", got, altHome)
	}
	data, err := readAttachTranscript(ag, path, altHome)
	if err != nil {
		t.Fatalf("readAttachTranscript() error = %v", err)
	}
	if string(data) != `{"type":"user"}` {
		t.Errorf("readAttachTranscript() = %q", data)
	}

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := readAttachTranscript(ag, path, altHome); !errors.Is(err, osroot.ErrSymlinkedPath) {
		t.Errorf("readAttachTranscript() error = %v, want %v", err, osroot.ErrSymlinkedPath)
	}
}

func TestResolveAgentAndTranscript_CodexHistoricalHome(t *testing.T) {
	for _, tree := range []string{"sessions", "archived_sessions"} {
		t.Run(tree, func(t *testing.T) {
			isolateAgentHomesRegistry(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
			setupAttachTestRepo(t)

			const sessionID = "12345678-1234-1234-1234-123456789abc"
			home := t.TempDir()
			require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, home))
			name := filepath.Join(tree, "2026", "10", "01", "rollout-2026-10-01T00-00-00-"+sessionID+".jsonl")
			testutil.WriteFile(t, home, name, "{}\n")
			canonicalHome, err := filepath.EvalSymlinks(home)
			require.NoError(t, err)

			var output bytes.Buffer
			ag, path, foundHome, err := resolveAgentAndTranscript(context.Background(), &output, sessionID, agent.AgentNameCodex, nil)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(canonicalHome, name), path)
			require.Equal(t, canonicalHome, foundHome)
			require.Contains(t, output.String(), "Found transcript under a different agent home: "+canonicalHome)
			data, err := readAttachTranscript(ag, path, foundHome)
			require.NoError(t, err)
			require.Equal(t, "{}\n", string(data))
		})
	}
}

func TestResolveAgentAndTranscript_CanonicalizesLinkedHistoricalHome(t *testing.T) {
	isolateAgentHomesRegistry(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	setupAttachTestRepo(t)

	home := t.TempDir()
	linkedHome := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, linkedHome); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeClaudeCode, linkedHome))
	const sessionID = "test-linked-historical-home"
	writeClaudeTranscriptUnder(t, home, sessionID)
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)

	var output bytes.Buffer
	_, path, foundHome, err := resolveAgentAndTranscript(context.Background(), &output, sessionID, agent.AgentNameClaudeCode, nil)
	require.NoError(t, err)
	require.Equal(t, canonicalHome, foundHome)
	require.Equal(t, filepath.Join(canonicalHome, "projects", "some-project", sessionID+".jsonl"), path)
}

func TestResolveAgentAndTranscript_ExistingBoundaryRejectsRedirection(t *testing.T) {
	isolateAgentHomesRegistry(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	setupAttachTestRepo(t)
	const sessionID = "test-existing-boundary"
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	require.NoError(t, err)
	path, err := resolveTranscriptPath(context.Background(), sessionID, ag)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	testutil.WriteFile(t, home, "secret.jsonl", "private")
	if err := os.Symlink(filepath.Join(home, "secret.jsonl"), path); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	state := &session.State{AgentType: ag.Type(), AgentHome: home, TranscriptPath: path}
	var output bytes.Buffer
	ag, foundPath, foundHome, err := resolveAgentAndTranscript(context.Background(), &output, sessionID, ag.Name(), state)
	if err == nil {
		_, err = readAttachTranscript(ag, foundPath, foundHome)
	}
	require.Error(t, err, "reattaching must not read through an established session boundary")
}

func TestResolveAgentAndTranscript_AutoDetectionCannotDowngradeBoundary(t *testing.T) {
	isolateAgentHomesRegistry(t)
	setupAttachTestRepo(t)
	home, cursorDir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)
	const sessionID = "test-attach-boundary-autodetect"
	testutil.WriteFile(t, cursorDir, sessionID+".jsonl", "{}\n")
	state := &session.State{AgentType: agent.AgentTypeClaudeCode, AgentHome: home,
		TranscriptPath: filepath.Join(home, "projects", "project", sessionID+".jsonl")}
	var output bytes.Buffer
	_, _, _, err := resolveAgentAndTranscript(context.Background(), &output, sessionID, agent.AgentNameClaudeCode, state)
	require.Error(t, err, "auto-detecting a home-less agent must not erase the recorded boundary")
}

func TestResolveAgentAndTranscript_ProvisionalHomeAllowsSessionOverride(t *testing.T) {
	isolateAgentHomesRegistry(t)
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", project)
	setupAttachTestRepo(t)
	const sessionID = "test-provisional-home"
	testutil.WriteFile(t, project, sessionID+".jsonl", "{}\n")
	for _, priorPath := range []string{"", filepath.Join(project, sessionID+".jsonl")} {
		state := &session.State{AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: priorPath}
		var output bytes.Buffer
		ag, path, foundHome, err := resolveAgentAndTranscript(context.Background(), &output, sessionID, agent.AgentNameClaudeCode, state)
		require.NoError(t, err)
		require.Empty(t, foundHome)
		data, err := readAttachTranscript(ag, path, foundHome)
		require.NoError(t, err)
		require.Equal(t, "{}\n", string(data))
	}
}
func TestProbeSessionHome_CodexSkipsUnsafeMatch(t *testing.T) {
	t.Parallel()
	testutil.SkipWithoutSymlinks(t)
	home := t.TempDir()
	id := "0198fe1a-2c07-76c1-8bfe-codexsession"
	live := filepath.Join(home, "sessions", "2026", "10", "03")
	archived := filepath.Join(home, "archived_sessions", "2026", "10", "02")
	require.NoError(t, os.MkdirAll(live, 0700))
	require.NoError(t, os.MkdirAll(archived, 0700))
	unsafe := filepath.Join(live, "rollout-2026-10-03-"+id+".jsonl")
	safe := filepath.Join(archived, "rollout-2026-10-02-"+id+".jsonl")
	require.NoError(t, os.WriteFile(safe, []byte("{}\n"), 0600))
	require.NoError(t, os.Symlink(safe, unsafe))
	ag, err := agent.Get(agent.AgentNameCodex)
	require.NoError(t, err)
	provider, ok := agent.AsAgentHomeProvider(ag)
	require.True(t, ok)
	require.True(t, agent.HomeConfinesTranscript(provider, home, safe))
	require.Equal(t, safe, probeSessionHome(home, provider, id), "unsafe live candidate must not mask valid archived candidate")
}
func TestAlternateHomes_SkipsUnreadableTranscript(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	active, unreadableHome, readableHome := t.TempDir(), t.TempDir(), t.TempDir()
	ag := newAltHomeAgent(t, active)
	sessionID := "review-permission-session"
	bad := writeClaudeTranscriptUnder(t, unreadableHome, sessionID)
	good := writeClaudeTranscriptUnder(t, readableHome, sessionID)
	require.NoError(t, agent.RememberAgentHome(ag.Type(), unreadableHome))
	require.NoError(t, agent.RememberAgentHome(ag.Type(), readableHome))
	require.NoError(t, os.Chmod(bad, 0000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(bad, 0600)) })
	_, err := agent.ReadTranscriptFileUnderHome(bad, unreadableHome)
	if err == nil {
		t.Skip("filesystem does not enforce file permissions")
	}
	require.ErrorIs(t, err, os.ErrPermission)
	hit, home, err := searchTranscriptInProjectDirs(sessionID, ag)
	require.NoError(t, err)
	require.Equal(t, good, hit, "unreadable candidate must not mask valid later home")
	require.Equal(t, readableHome, home)
}

type candidateHomeAgent struct {
	*altHomeAgent

	candidates []string
}

func (a *candidateHomeAgent) ResolveSessionFile(_ string, _ string) string {
	return filepath.Join(filepath.Dir(a.home), "outside.jsonl")
}

func (a *candidateHomeAgent) ResolveSessionFileCandidates(_ string, _ string) []string {
	return a.candidates
}

func TestDiscoverSessionFile_ValidatesEveryCandidate(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	good := writeClaudeTranscriptUnder(t, home, "candidate")
	directory := filepath.Join(home, "projects", "some-project", "directory.jsonl")
	require.NoError(t, os.Mkdir(directory, 0o700))
	ag := &candidateHomeAgent{altHomeAgent: newAltHomeAgent(t, home), candidates: []string{
		filepath.Join(filepath.Dir(home), "outside.jsonl"),
		filepath.Join(home, "projects", "some-project", "missing.jsonl"), directory, good,
	}}
	store, err := agent.OpenSessionStoreAt(ag, home)
	require.NoError(t, err)
	require.Equal(t, good, discoverSessionFile(store, ag, ag.baseDir, "candidate", home))
	require.Empty(t, discoverSessionFile(store, ag, ag.baseDir, "../escape", home))
}
