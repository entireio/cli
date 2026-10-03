package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Each test below is written as an attack against validateAdoptSourceTranscript's
// recorded-AgentHome acceptance route (P2 of the agent-storage-roots work):
// a transcript path the FALLBACK route (agent.AgentForTranscriptPath) would
// never have matched, crafted so that ONLY a bug in the recorded-home gates
// would let it through.

// Attack 1: a maximally permissive recorded home ("/") paired with a
// transcript that is not shaped like any agent's session file at all. Gate 3
// (SessionPathUnder's layout test) must reject this on its own, independent
// of containment.
func TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile(t *testing.T) {
	// Isolate the fallback route's agent.GetSessionDir resolution: without
	// this, an unset CLAUDE_CONFIG_DIR (TestMain only unsets it, it does not
	// point it anywhere) resolves to the developer's real ~/.claude via
	// os.UserHomeDir, which CLAUDE.md forbids tests from touching.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	key := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(key, []byte("not a transcript"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &session.State{
		SessionID:      "attack-root-home",
		AgentType:      agent.AgentTypeClaudeCode,
		AgentHome:      "/",
		TranscriptPath: key,
	}

	err := validateAdoptSourceTranscript(source, t.TempDir())
	if err == nil {
		t.Fatalf("validateAdoptSourceTranscript accepted a non-session file under AgentHome \"/\": %s", key)
	}
	if !strings.Contains(err.Error(), "unexpected transcript path") {
		t.Fatalf("error = %v, want unexpected transcript path", err)
	}
}

// Attack 2: a transcript that is lexically shaped exactly right and
// genuinely lies under the recorded home, but is actually a symlink to a
// file outside it. filepath.Abs does not resolve symlinks, so the lexical
// gates alone would accept this; only EvalSymlinks-then-recheck catches it.
func TestValidateAdoptSourceTranscript_RejectsTranscriptSymlinkedOutOfHome(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("requires os.Symlink support")
	}
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	home := t.TempDir()
	rememberAdoptHome(t, home)
	projectsDir := filepath.Join(home, "projects", "adopt-test")
	if err := os.MkdirAll(projectsDir, 0o750); err != nil {
		t.Fatal(err)
	}

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_ed25519")
	if err := os.WriteFile(secret, []byte("super secret key material"), 0o600); err != nil {
		t.Fatal(err)
	}

	transcriptPath := filepath.Join(projectsDir, "session.jsonl")
	if err := os.Symlink(secret, transcriptPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	source := &session.State{
		SessionID:      "session",
		AgentType:      agent.AgentTypeClaudeCode,
		AgentHome:      home,
		TranscriptPath: transcriptPath,
	}

	err := validateAdoptSourceTranscript(source, t.TempDir())
	if err == nil {
		t.Fatal("validateAdoptSourceTranscript accepted a transcript symlinked outside its recorded home")
	}
}

// Regression guard for the fix itself: a recorded home that is ITSELF a
// legitimate symlink (e.g. a relocated ~/.claude-work -> /mnt/...) must still
// be accepted, with the transcript it genuinely contains. Comparing a
// resolved transcript against an unresolved home would break this.
func TestValidateAdoptSourceTranscript_AcceptsLegitimatelySymlinkedHome(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("requires os.Symlink support")
	}
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	worktree := t.TempDir()
	realHome := t.TempDir()
	projectsDir := adoptClaudeProject(t, realHome, worktree)
	if err := os.MkdirAll(projectsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	realTranscript := filepath.Join(projectsDir, "session.jsonl")
	if err := os.WriteFile(realTranscript, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	linkedHome := filepath.Join(t.TempDir(), "claude-work")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	rememberAdoptHome(t, linkedHome)
	linkedTranscript := filepath.Join(adoptClaudeProject(t, linkedHome, worktree), "session.jsonl")
	testutil.WriteFile(t, projectsDir, "task.jsonl", "{}\n")
	linkedTask := filepath.Join(adoptClaudeProject(t, linkedHome, worktree), "task.jsonl")

	source := &session.State{
		SessionID:      "session",
		AgentType:      agent.AgentTypeClaudeCode,
		AgentHome:      linkedHome,
		TranscriptPath: linkedTranscript,
		TaskRecords:    []session.TaskRecord{{AgentID: "task", DeclaredTranscriptPath: linkedTask}},
	}

	if err := validateAdoptSourceTranscript(source, worktree); err != nil {
		t.Fatalf("validateAdoptSourceTranscript rejected a legitimately symlinked home: %v", err)
	}

	resolvedTranscript, err := filepath.EvalSymlinks(realTranscript)
	if err != nil {
		t.Fatal(err)
	}
	if source.TranscriptPath != resolvedTranscript {
		t.Fatalf("TranscriptPath = %q, want resolved form %q (so a later reader is confined to the real directory)",
			source.TranscriptPath, resolvedTranscript)
	}
	canonicalHome, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatal(err)
	}
	if source.AgentHome != canonicalHome {
		t.Fatalf("AgentHome = %q, want canonical home %q", source.AgentHome, canonicalHome)
	}
	if got := source.TaskRecords[0].DeclaredTranscriptPath; got != filepath.Join(adoptClaudeProject(t, canonicalHome, worktree), "task.jsonl") {
		t.Fatalf("task transcript was lost when canonicalizing its home: %q", got)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("private content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(realTranscript); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, realTranscript); err != nil {
		t.Fatal(err)
	}
	if data, readErr := agent.ReadTranscriptFileUnderHome(source.TranscriptPath, source.AgentHome); readErr == nil {
		t.Fatalf("read swapped transcript after adoption: %q", data)
	}
	source.AgentHome = linkedHome
	source.SessionID = "missing"
	source.TranscriptPath = filepath.Join(adoptClaudeProject(t, linkedHome, worktree), "missing.jsonl")
	if err := validateAdoptSourceTranscript(source, worktree); err != nil {
		t.Fatalf("missing transcript under a trusted linked home: %v", err)
	}
	if source.AgentHome != canonicalHome || source.TranscriptPath != filepath.Join(adoptClaudeProject(t, canonicalHome, worktree), "missing.jsonl") {
		t.Fatalf("missing transcript has inconsistent coordinates: home %q, path %q", source.AgentHome, source.TranscriptPath)
	}
	if source.TaskRecords[0].DeclaredTranscriptPath == "" {
		t.Fatal("canonical task path was lost under a linked home")
	}
}

// A trailing separator on the recorded home is a cosmetic difference, not a
// reason to refuse an otherwise legitimate home.
//
// A SINGLE trailing separator is not enough to isolate what this test claims
// to guard (recordedHomeAcceptsPath's home = filepath.Clean(home)):
// agent.PathHasDirPrefix already appends one trailing separator itself when
// the directory doesn't already end with one, so a single extra separator
// passes with or without the Clean call and this test would not catch its
// removal. A DOUBLE trailing separator does isolate it: PathHasDirPrefix's
// "doesn't already end with one" check is a no-op when the home already ends
// in a separator (even a doubled one), so an uncleaned "home//" never
// prefix-matches the transcript path's single-separator spelling (produced
// via filepath.Join, which always collapses). Only filepath.Clean collapsing
// "home//" to "home" before the prefix check makes this accept.
func TestValidateAdoptSourceTranscript_AcceptsHomeWithTrailingSeparator(t *testing.T) {
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	worktree := t.TempDir()
	home := t.TempDir()
	rememberAdoptHome(t, home)
	projectsDir := adoptClaudeProject(t, home, worktree)
	if err := os.MkdirAll(projectsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projectsDir, "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doubledSep := string(filepath.Separator) + string(filepath.Separator)
	source := &session.State{
		SessionID:      "session",
		AgentType:      agent.AgentTypeClaudeCode,
		AgentHome:      home + doubledSep,
		TranscriptPath: transcriptPath,
	}

	if err := validateAdoptSourceTranscript(source, worktree); err != nil {
		t.Fatalf("validateAdoptSourceTranscript rejected a recorded home with a doubled trailing separator: %v", err)
	}
}

// Type confusion: the state claims Claude Code, but the recorded home and
// transcript are shaped like Codex's own layout
// (<home>/sessions/YYYY/MM/DD/rollout-*.jsonl, not Claude's
// <home>/projects/.../*.jsonl). The recorded home only relaxes the LOCATION
// test; it must never relax the LAYOUT test that stands in for identity.
func TestValidateAdoptSourceTranscript_RejectsTypeConfusion(t *testing.T) {
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	home := t.TempDir()
	rememberAdoptHome(t, home)
	codexSessionDir := filepath.Join(home, "sessions", "2026", "01", "01")
	if err := os.MkdirAll(codexSessionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(codexSessionDir, "rollout-2026-01-01T00-00-00-abc123.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &session.State{
		SessionID:      "attack-type-confusion",
		AgentType:      agent.AgentTypeClaudeCode, // claims Claude Code...
		AgentHome:      home,                      // ...but this is a Codex-shaped home and path
		TranscriptPath: transcriptPath,
	}

	err := validateAdoptSourceTranscript(source, t.TempDir())
	if err == nil {
		t.Fatal("validateAdoptSourceTranscript accepted a Codex-shaped transcript under a Claude Code AgentType")
	}
}

// AgentHome empty must behave byte-identically to every pre-existing
// session: only the fallback route runs, exactly as before this change.
func TestValidateAdoptSourceTranscript_EmptyAgentHomeUnchanged(t *testing.T) {
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &session.State{
		SessionID:      "attack-empty-agent-home",
		AgentType:      agent.AgentTypeClaudeCode,
		AgentHome:      "",
		TranscriptPath: transcriptPath,
	}
	sourceWorktree := t.TempDir()

	err := validateAdoptSourceTranscript(source, sourceWorktree)
	if err == nil {
		t.Fatal("validateAdoptSourceTranscript accepted an arbitrary path with no AgentHome and no matching fallback agent")
	}
	if !strings.Contains(err.Error(), "unexpected transcript path") {
		t.Fatalf("error = %v, want unexpected transcript path (the pre-AgentHome fallback message)", err)
	}
	if source.TranscriptPath != transcriptPath {
		t.Fatalf("TranscriptPath mutated with AgentHome empty: got %q, want unchanged %q", source.TranscriptPath, transcriptPath)
	}
}

// The real cross-instance case this task exists to fix: two instances of
// Claude Code on the same machine under different CLAUDE_CONFIG_DIR values
// (wclaude / pclaude from the bug report). The source session recorded its
// OWN home (simulating wclaude); the adopting process here runs under a
// DIFFERENT CLAUDE_CONFIG_DIR (simulating pclaude) that does not name the
// source's recorded home anywhere. Under the pre-P2 code, which re-derives
// the session directory from the adopting process's own environment, this
// was rejected — the reported bug. Only the recorded-home route can accept
// it.
func TestSessionAdopt_AcceptsCrossInstanceRecordedHome(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	// The adopting process's own relocated home — pclaude's — which must play
	// no part in recognizing a transcript recorded under wclaude's home.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-personal"))

	// wclaude's home, as it would have been resolved and recorded by the
	// PROCESS THAT STARTED THE SESSION — independent of whatever the
	// adopting process's environment says.
	sourceAgentHome := filepath.Join(t.TempDir(), "claude-work")
	projectsDir := adoptClaudeProject(t, sourceAgentHome, sourceRepo)
	if err := os.MkdirAll(projectsDir, 0o750); err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-cross-instance"
	transcriptPath := filepath.Join(projectsDir, sessionID+".jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"update target file"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rememberAdoptHome(t, sourceAgentHome)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	lastInteraction := time.Now().Add(-1 * time.Minute)
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		AgentHome:             sourceAgentHome,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "update target file",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed under cross-instance recorded home: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target repo")
	}
	resolvedTranscript, err := filepath.EvalSymlinks(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.TranscriptPath != resolvedTranscript {
		t.Fatalf("TranscriptPath = %q, want resolved %q", adopted.TranscriptPath, resolvedTranscript)
	}
	if adopted.AgentHome != sourceAgentHome {
		t.Fatalf("AgentHome = %q, want the recorded source home %q carried over", adopted.AgentHome, sourceAgentHome)
	}
}

// TestValidateAdoptSourceTaskRecords_ClearsUnauthorizedDeclaredPath is the
// regression test for Item 1 of the agent-storage-roots p4 report:
// cloneAdoptSourceState's `adopted := *source` copies TaskRecords wholesale,
// and (before this fix) nothing validated TaskRecord.DeclaredTranscriptPath —
// condensation's materializeTaskRecords (manual_commit_condensation.go) reads
// it as a candidate, unconditionally and agent-agnostically, and materializes
// it whole into the pushed checkpoint's task payload. No symlink is needed to
// demonstrate this: an attacker who can write the source worktree's session
// state JSON (the shared/CI-checkout, container-mount, or untrusted-agent
// threat model this whole task line is about) just names any readable file on
// the machine as a plausible task record's DeclaredTranscriptPath.
//
// This source state records no AgentHome at all — the shape of the bug that
// predates AgentHome recording entirely, and the shape an attacker gets for
// free by simply not bothering to set one. Before the fix, cloning carried
// the arbitrary path through unchanged. After the fix, the fallback route
// (agent.AgentForTranscriptPath, independent of AgentHome) rejects a path
// that is not a real Claude Code session file under sourceWorktree, and the
// record's DeclaredTranscriptPath is cleared.
func TestValidateAdoptSourceTaskRecords_ClearsUnauthorizedDeclaredPath(t *testing.T) {
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	victim := filepath.Join(t.TempDir(), "id_ed25519")
	const secret = "EXFILTRATED-MARKER-task-record super secret key material\n"
	if err := os.WriteFile(victim, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &session.State{
		SessionID: "attack-task-record",
		AgentType: agent.AgentTypeClaudeCode,
		TaskRecords: []session.TaskRecord{
			{
				ToolUseID:              "toolu_attack_1",
				AgentID:                "agent_attack_1",
				DeclaredTranscriptPath: victim,
			},
		},
	}

	newAdoptPathAuthorizer(t.TempDir()).validateTaskRecords(source)

	if got := source.TaskRecords[0].DeclaredTranscriptPath; got != "" {
		t.Fatalf("DeclaredTranscriptPath = %q, want cleared — materializeTaskRecords reads this path whole "+
			"and unconditionally into the pushed checkpoint's task payload", got)
	}
}

// Companion to the attack test above: a task record whose DeclaredTranscriptPath
// genuinely belongs to the recorded AgentHome, laid out exactly the way
// resolveTaskTranscriptPath's own fallback expects
// (<transcriptDir>/<sessionID>/subagents/agent-<id>.jsonl), must survive
// unchanged. Clearing indiscriminately would make honest adopted sessions
// silently lose subagent transcripts for agents whose DeclaredTranscriptPath
// cannot always be rediscovered by that fallback (e.g. Factory AI Droid's
// Worker sessions live at a sibling session path entirely, not nested under
// the parent's TranscriptPath directory) — this guards the Claude Code case,
// which the fallback CAN rediscover, so a regression here would be silent.
func TestValidateAdoptSourceTaskRecords_PreservesAuthorizedDeclaredPath(t *testing.T) {
	// See TestValidateAdoptSourceTranscript_RootHomeRejectsNonSessionFile for why.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	worktree := t.TempDir()
	home := t.TempDir()
	rememberAdoptHome(t, home)
	subagentsDir := filepath.Join(adoptClaudeProject(t, home, worktree), "sess-1", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	subagentTranscript := filepath.Join(subagentsDir, "agent-child1.jsonl")
	if err := os.WriteFile(subagentTranscript, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &session.State{
		SessionID: "attack-task-record-honest",
		AgentType: agent.AgentTypeClaudeCode,
		AgentHome: home,
		TaskRecords: []session.TaskRecord{
			{
				ToolUseID:              "toolu_honest_1",
				AgentID:                "child1",
				DeclaredTranscriptPath: subagentTranscript,
			},
		},
	}

	newAdoptPathAuthorizer(worktree).validateTaskRecords(source)

	resolved, err := filepath.EvalSymlinks(subagentTranscript)
	if err != nil {
		t.Fatal(err)
	}
	if got := source.TaskRecords[0].DeclaredTranscriptPath; got != resolved {
		t.Fatalf("DeclaredTranscriptPath = %q, want the legitimate subagent transcript %q preserved (resolved form)",
			got, resolved)
	}
}

func rememberAdoptHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	if err := agent.RememberAgentHome(agent.AgentTypeClaudeCode, home); err != nil {
		t.Fatal(err)
	}
}

func adoptClaudeProject(t *testing.T, home, worktree string) string {
	t.Helper()
	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := ag.(agent.WorktreeSessionDirProvider)
	if !ok {
		t.Fatal("Claude Code must provide its worktree session directory")
	}
	return provider.SessionDirUnder(home, worktree)
}

func TestValidateAdoptSourceTranscript_RejectsLinksInsideTrustedHome(t *testing.T) {
	for _, targetName := range []string{"target.jsonl", "missing.jsonl"} {
		t.Run(targetName, func(t *testing.T) {
			home := t.TempDir()
			rememberAdoptHome(t, home)
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
			testutil.WriteFile(t, home, "projects/proj/target.jsonl", "{}\n")
			path := filepath.Join(home, "projects", "proj", "session.jsonl")
			if err := os.Symlink(targetName, path); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
			source := &session.State{AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: path}
			if err := validateAdoptSourceTranscript(source, t.TempDir()); err == nil {
				t.Fatal("adoption accepted a transcript link below its trusted home")
			}
		})
	}
}

func TestValidateAdoptSourceTranscript_RejectsUnrecognizedHome(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	path := filepath.Join(home, "projects", "private", "secret.jsonl")
	testutil.WriteFile(t, home, "projects/private/secret.jsonl", "private transcript")
	source := &session.State{SessionID: "session", AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: path}
	if err := validateAdoptSourceTranscript(source, t.TempDir()); err == nil {
		t.Fatal("unrecognized source home authorized its own transcript")
	}
	source.TaskRecords = []session.TaskRecord{{DeclaredTranscriptPath: path}}
	newAdoptPathAuthorizer(t.TempDir()).validateTaskRecords(source)
	if source.TaskRecords[0].DeclaredTranscriptPath != "" {
		t.Fatal("unrecognized source home authorized a task transcript")
	}
}

func TestValidateAdoptSourceTranscript_AcceptsActiveHomeWithoutRegistry(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	worktree := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	project := adoptClaudeProject(t, home, worktree)
	path := filepath.Join(project, "session.jsonl")
	testutil.WriteFile(t, project, "session.jsonl", "{}\n")
	source := &session.State{SessionID: "session", AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: path}
	if err := validateAdoptSourceTranscript(source, worktree); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAdoptSourceTranscript_ActiveHomeRejectsSymlinkFallback(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	worktree := t.TempDir()
	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := ag.GetSessionDir(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("private content"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.Symlink(secret, path); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	source := &session.State{AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: path}
	if err := validateAdoptSourceTranscript(source, worktree); err == nil {
		t.Fatal("active-store fallback bypassed recorded-home symlink rejection")
	}
}

// TestValidateAdoptSourceTranscript_LegacyAcceptsLinkedProjectsDir covers a
// session adopted before homes were recorded whose ~/.claude/projects is a
// link to another disk. Its transcript does not resolve beneath the home, so
// it cannot be upgraded to a confined home, but ownership still authorizes it
// under the legacy read protocol.
func TestValidateAdoptSourceTranscript_LegacyAcceptsLinkedProjectsDir(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "projects")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	worktree := t.TempDir()
	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := ag.GetSessionDir(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &session.State{SessionID: "session", AgentType: agent.AgentTypeClaudeCode, TranscriptPath: path}
	if err := validateAdoptSourceTranscript(source, worktree); err != nil {
		t.Fatalf("validateAdoptSourceTranscript() error = %v, want the legacy session accepted", err)
	}
	if source.AgentHome != "" {
		t.Errorf("AgentHome = %q, want empty (legacy read protocol)", source.AgentHome)
	}
	if source.TranscriptPath != path {
		t.Errorf("TranscriptPath = %q, want %q unchanged", source.TranscriptPath, path)
	}
}
