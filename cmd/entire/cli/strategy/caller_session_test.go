package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// clearCallerSessionEnv unsets every agent's caller-session variable, derived
// from the registry rather than hand-listed so it cannot rot when an agent is
// added. Necessary because `go test` is routinely run from inside one of these
// agents, whose variable would otherwise leak into every case below.
func clearCallerSessionEnv(t *testing.T) {
	t.Helper()
	for _, name := range agent.CallerSessionEnvVars() {
		t.Setenv(name, "")
	}
}

// callerSessionRepo sets up an isolated repo, chdirs into it, and returns the
// worktree root as git resolves it (symlinks and all, e.g. /var →
// /private/var on macOS) so a state's WorktreePath can be made to match.
func callerSessionRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	resolved, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("paths.WorktreeRoot() error = %v", err)
	}
	return resolved
}

func saveState(t *testing.T, id, worktree string, lastInteraction time.Time) {
	t.Helper()
	state := &SessionState{
		SessionID:           id,
		BaseCommit:          "abc1234",
		WorktreePath:        worktree,
		StartedAt:           lastInteraction,
		LastInteractionTime: &lastInteraction,
		Phase:               "idle",
	}
	if err := SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState(%q) error = %v", id, err)
	}
}

// The reported bug, as a test: an agent asks which session is running it from
// a worktree that has no sessions of its own. Before the caller tier, the
// answer was another worktree's live session, indistinguishable from a real
// one — which is what got fed to `session adopt`.
func TestResolveCallerSession_PrefersTheCallerOverAnotherWorktreesSession(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	saveState(t, "elsewhere-session", "/some/other/worktree", time.Now())
	saveState(t, "caller-session", "/a/third/worktree", time.Now().Add(-2*time.Hour))
	t.Setenv("CODEX_SESSION_ID", "caller-session")

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "caller-session" {
		t.Errorf("SessionID = %q, want %q (the caller, not the most recent)", got.SessionID, "caller-session")
	}
	if got.Resolution != ResolutionCallerEnv {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionCallerEnv)
	}
	if !got.Tracked {
		t.Error("Tracked = false; state was saved for this session")
	}
	if !got.Resolution.IsCaller() {
		t.Error("IsCaller() = false for the caller-env tier")
	}
}

func TestResolveCallerSession_FallsBackToThisWorktree(t *testing.T) {
	clearCallerSessionEnv(t)
	worktree := callerSessionRepo(t)

	saveState(t, "elsewhere-session", "/some/other/worktree", time.Now())
	saveState(t, "local-session", worktree, time.Now().Add(-time.Hour))

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "local-session" {
		t.Errorf("SessionID = %q, want %q (prefer this worktree over a newer foreign one)", got.SessionID, "local-session")
	}
	if got.Resolution != ResolutionWorktree {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionWorktree)
	}
	if got.Resolution.IsCaller() {
		t.Error("IsCaller() = true for the worktree tier; it is an inference, not an identification")
	}
}

// The weakest tier must still be reachable — it is what a human asking "what
// has been happening in this repo" wants — but it must announce itself, since
// nothing about it relates to the caller.
func TestResolveCallerSession_LabelsTheCrossWorktreeFallback(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	saveState(t, "elsewhere-session", "/some/other/worktree", time.Now())

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "elsewhere-session" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "elsewhere-session")
	}
	if got.Resolution != ResolutionOtherWorktree {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionOtherWorktree)
	}
	if got.Resolution.IsCaller() {
		t.Error("IsCaller() = true for the other-worktree tier; this is the one an agent must never act on")
	}
}

func TestResolveCallerSession_NothingFound(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	got := ResolveCallerSession(context.Background())
	if got.Found() {
		t.Errorf("ResolveCallerSession() = %+v, want no session", got)
	}
	if got.Resolution != ResolutionNone {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionNone)
	}
}

// An agent named its session but Entire holds no state for it: hooks are not
// installed, they failed, or the first turn has not landed. The ID and the
// agent are still reported, because "you are in session X and Entire is not
// recording it" is a diagnosis — and crucially it must NOT fall through to a
// weaker tier and answer with someone else's session.
func TestResolveCallerSession_ReportsAnUntrackedCallerRatherThanGuessing(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	saveState(t, "elsewhere-session", "/some/other/worktree", time.Now())
	t.Setenv("CURSOR_CONVERSATION_ID", "untracked-caller")

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "untracked-caller" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "untracked-caller")
	}
	if got.Tracked {
		t.Error("Tracked = true; no state was saved for this session")
	}
	if got.Resolution != ResolutionCallerEnv {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionCallerEnv)
	}
	if got.AgentType == "" {
		t.Error("AgentType is empty; the environment names the agent even when state is missing")
	}
}

// A tracked claim is the one worth REPORTING when the other is untracked — a
// session Entire holds is more useful than an ID it knows nothing about — but
// reporting is not identifying. The untracked claim has no recorded owner, so
// ancestry cannot place it and cannot rule it out as the nearer of the two.
func TestResolveCallerSession_TrackedClaimIsReportedButStillAmbiguous(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	saveState(t, "inner-session", "/some/other/worktree", time.Now())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-untracked")
	t.Setenv("CODEX_SESSION_ID", "inner-session")

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "inner-session" {
		t.Errorf("SessionID = %q, want the tracked claim %q", got.SessionID, "inner-session")
	}
	if !got.Tracked {
		t.Error("Tracked = false; the tracked candidate should have been reported")
	}
	if got.Resolution != ResolutionCallerAmbiguous {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionCallerAmbiguous)
	}
}

// Two tracked claims that ancestry cannot order — neither recorded an owner —
// is not an identification. The most recently active one is still reported as
// the most plausible guess, but the resolution says it is a guess, so nothing
// acts on it. Picking one and calling it "your own session" is the failure the
// whole type exists to prevent.
func TestResolveCallerSession_UnrankableNestedClaimsAreAmbiguous(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	saveState(t, "outer-session", "/some/other/worktree", time.Now().Add(-time.Hour))
	saveState(t, "inner-session", "/some/other/worktree", time.Now())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-session")
	t.Setenv("CODEX_SESSION_ID", "inner-session")

	got := ResolveCallerSession(context.Background())
	if got.Resolution != ResolutionCallerAmbiguous {
		t.Errorf("Resolution = %q, want %q — nothing ordered the two claims", got.Resolution, ResolutionCallerAmbiguous)
	}
	if got.Resolution.IsCaller() {
		t.Error("IsCaller() = true for an unordered pair of claims")
	}
	if got.SessionID != "inner-session" {
		t.Errorf("SessionID = %q, want the most plausible guess %q", got.SessionID, "inner-session")
	}
}

// Same rule with no state at all: two untracked claims cannot be ordered by
// anything — no owner to compare, no interaction time — so a pick would be
// registry order in disguise.
func TestResolveCallerSession_UntrackedMultipleClaimsAreAmbiguous(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	t.Setenv("CLAUDE_CODE_SESSION_ID", "untracked-outer")
	t.Setenv("CODEX_SESSION_ID", "untracked-inner")

	got := ResolveCallerSession(context.Background())
	if got.Resolution != ResolutionCallerAmbiguous {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionCallerAmbiguous)
	}
	if got.Tracked {
		t.Error("Tracked = true; no state was saved for either claim")
	}
	if !got.Found() {
		t.Error("Found() = false; being inside an untracked session is still worth reporting")
	}
}

// An unsafe value reads as absent at the agent layer, so the resolver must
// degrade to a weaker tier rather than carry it — never resolve a path from an
// ID the environment supplied unchecked.
func TestResolveCallerSession_IgnoresAnUnsafePublishedID(t *testing.T) {
	clearCallerSessionEnv(t)
	worktree := callerSessionRepo(t)

	saveState(t, "local-session", worktree, time.Now())
	t.Setenv("CODEX_SESSION_ID", "../../../etc/passwd")

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "local-session" || got.Resolution != ResolutionWorktree {
		t.Errorf("ResolveCallerSession() = %+v, want the worktree tier", got)
	}
}

func TestSessionResolution_IsCaller(t *testing.T) {
	cases := map[SessionResolution]bool{
		ResolutionCallerEnv:       true,
		ResolutionAncestry:        true,
		ResolutionCallerAmbiguous: false,
		ResolutionWorktree:        false,
		ResolutionOtherWorktree:   false,
		ResolutionNone:            false,
	}
	for resolution, want := range cases {
		if got := resolution.IsCaller(); got != want {
			t.Errorf("SessionResolution(%q).IsCaller() = %v, want %v", resolution, got, want)
		}
	}
}

// The ancestry tier, exercised with a real identity rather than a fixture:
// proclive.Chain() is the documented way tests obtain live identities at known
// depths, so recording chain[0] as a session's owner makes that session
// genuinely our nearest-ancestor match.
//
// This tier is what covers the agents that publish no session ID — Gemini CLI
// and opencode — so it must not be left to the environment-variable tests.
func TestResolveCallerSession_MatchesOwnerByProcessAncestry(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes; the ancestry tier is unavailable here by design")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible; nothing to match against")
	}
	owner := chain[0]

	newer := time.Now()
	older := newer.Add(-2 * time.Hour)
	// A newer session elsewhere that ancestry must lose to, so a pass cannot
	// come from the recency tiers underneath.
	saveState(t, "newer-elsewhere", "/some/other/worktree", newer)

	owned := &SessionState{
		SessionID:           "ancestry-owned",
		BaseCommit:          "abc1234",
		WorktreePath:        "/a/third/worktree",
		StartedAt:           older,
		LastInteractionTime: &older,
		Phase:               "idle",
		Owner:               &owner,
	}
	if err := SaveSessionState(context.Background(), owned); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "ancestry-owned" {
		t.Errorf("SessionID = %q, want %q (our nearest-ancestor owner)", got.SessionID, "ancestry-owned")
	}
	if got.Resolution != ResolutionAncestry {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionAncestry)
	}
	if !got.Resolution.IsCaller() {
		t.Error("IsCaller() = false for the ancestry tier")
	}
}

// What the environment tier is actually for, now that a nearer owner outranks
// it: naming a session ancestry cannot rank. A session whose owner was never
// recorded (no turn has started yet) is invisible to the process walk, and on
// a platform that cannot introspect at all the walk finds nothing — the
// published ID is the only evidence there is, and it is good evidence.
func TestResolveCallerSession_EnvClaimWinsWhenAncestryCannotRank(t *testing.T) {
	clearCallerSessionEnv(t)
	worktree := callerSessionRepo(t)

	now := time.Now()
	// Neither session records an owner, so depth is unresolved for both and
	// only the environment distinguishes them.
	saveState(t, "local-session", worktree, now)
	saveState(t, "env-named", "/some/other/worktree", now.Add(-2*time.Hour))
	t.Setenv("CODEX_SESSION_ID", "env-named")

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "env-named" || got.Resolution != ResolutionCallerEnv {
		t.Errorf("ResolveCallerSession() = %+v, want env-named via caller-env", got)
	}
	if !got.Resolution.IsCaller() {
		t.Error("IsCaller() = false; a single unopposed claim is an identification")
	}
}

// An inherited environment ID must not outrank the session that actually
// spawned us. Codex launching Gemini or opencode is the shape: the inner agent
// publishes no ID of its own, so CODEX_SESSION_ID survives in our environment
// through the inner agent's process and is the ONLY env claim — while the
// inner session's owner is our nearest ancestor. Accepting the lone claim
// names the outer session and marks it IsCaller, which is precisely the
// "safe to act on" mistake the tiering exists to prevent.
func TestResolveCallerSession_InheritedEnvIDLosesToTheNearerOwner(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes; ancestry cannot arbitrate here by design")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible; nothing to match against")
	}
	// chain[0] is load-bearing: the winner lands at depth 0, our immediate
	// parent, which is what lets claimsRuledOut believe it despite the
	// unplaced outer claim. At any greater depth this case is ambiguous
	// instead — see UnplacedClaimShadowsADistantAncestryWinner.
	nearest := chain[0]

	now := time.Now()
	// The outer agent: env-claimed, tracked, but its owner is not in our
	// ancestry at all — it reached us only by inheritance.
	saveState(t, "outer-env-claimed", "/some/other/worktree", now)
	// The inner agent: publishes nothing, but its owner IS our nearest ancestor.
	inner := &SessionState{
		SessionID:           "inner-real-caller",
		BaseCommit:          "abc1234",
		WorktreePath:        "/a/third/worktree",
		StartedAt:           now.Add(-2 * time.Hour),
		LastInteractionTime: ptrTime(now.Add(-2 * time.Hour)),
		Phase:               "idle",
		Owner:               &nearest,
	}
	if err := SaveSessionState(context.Background(), inner); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	t.Setenv("CODEX_SESSION_ID", "outer-env-claimed")

	got := ResolveCallerSession(context.Background())
	if got.SessionID != "inner-real-caller" {
		t.Errorf("SessionID = %q, want %q — the inherited claim outranked the real caller",
			got.SessionID, "inner-real-caller")
	}
	if got.Resolution != ResolutionAncestry {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionAncestry)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// Mixed tracked and untracked claims must not resolve to an identification.
// A tracked outer Claude session launching an as-yet-untracked Codex session
// leaves both variables set, but only the outer one has state, so only it can
// enter the ranking — and it then wins by default and is reported as the
// caller. The inner claim is the likelier caller and nothing ruled it out:
// an untracked session has no recorded owner, so ancestry cannot place it.
func TestResolveCallerSession_MixedTrackedAndUntrackedClaimsAreAmbiguous(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	saveState(t, "outer-tracked", "/some/other/worktree", time.Now())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-tracked")
	t.Setenv("CODEX_SESSION_ID", "inner-untracked")

	got := ResolveCallerSession(context.Background())
	if got.Resolution != ResolutionCallerAmbiguous {
		t.Errorf("Resolution = %q, want %q — an untracked claim cannot be ruled out as nearer",
			got.Resolution, ResolutionCallerAmbiguous)
	}
	if got.Resolution.IsCaller() {
		t.Error("IsCaller() = true while an unplaceable claim exists")
	}
	// The tracked session is still the more useful thing to report.
	if got.SessionID != "outer-tracked" {
		t.Errorf("SessionID = %q, want the tracked claim %q", got.SessionID, "outer-tracked")
	}
}

// An unplaced claim can shadow an ancestry winner that is not our parent.
// A tracked opencode session launching an untracked Codex session publishes
// only CODEX_SESSION_ID: opencode enters the ranking through ancestry, Codex
// cannot enter at all for want of state, and opencode is then reported as the
// identified caller. The Codex claim is demonstrably in our ancestry — it put
// a variable in our environment — at a depth we cannot measure, so it could
// be the nearer of the two.
func TestResolveCallerSession_UnplacedClaimShadowsADistantAncestryWinner(t *testing.T) {
	clearCallerSessionEnv(t)
	callerSessionRepo(t)

	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes; ancestry cannot arbitrate here by design")
	}
	chain := ancestry.Chain()
	if len(chain) < 2 {
		t.Skip("need an ancestor beyond our parent: depth 0 is decisive on its own")
	}
	// Deliberately NOT chain[0]: an owner that is our immediate parent cannot
	// be shadowed, and that case is covered separately.
	distant := chain[1]

	now := time.Now()
	owned := &SessionState{
		SessionID:           "opencode-outer",
		BaseCommit:          "abc1234",
		WorktreePath:        "/some/other/worktree",
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               "idle",
		Owner:               &distant,
	}
	if err := SaveSessionState(context.Background(), owned); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	t.Setenv("CODEX_SESSION_ID", "codex-inner-untracked")

	got := ResolveCallerSession(context.Background())
	if got.Resolution != ResolutionCallerAmbiguous {
		t.Errorf("Resolution = %q, want %q — an unmeasurable claim could sit nearer than depth %d",
			got.Resolution, ResolutionCallerAmbiguous, 1)
	}
	if got.Resolution.IsCaller() {
		t.Error("IsCaller() = true while an unplaced claim could be nearer")
	}
}
