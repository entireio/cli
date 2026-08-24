package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

func TestContextRegistryNamespaceAndPermissions(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	a := contextRegistryPath(base, "https://us.auth.entire.io", "account-a")
	b := contextRegistryPath(base, "https://eu.auth.entire.io", "account-a")
	c := contextRegistryPath(base, "https://us.auth.entire.io", "account-b")
	if a == b || a == c || b == c {
		t.Fatal("registry namespaces must include both Core URL and account ID")
	}
	registry := contextRegistry{Sessions: []localContextSession{{SessionID: "s1", LastSeen: time.Now()}}}
	if err := writeContextRegistry(a, registry); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry mode = %o, want 600", got)
	}
	if got := filepath.Dir(a); got == base {
		t.Fatal("registry must live in a namespaced directory")
	}
	dirInfo, err := os.Stat(filepath.Dir(a))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("registry directory mode = %o, want 700", got)
	}
}

func TestPruneContextRegistry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := contextRegistry{Sessions: []localContextSession{
		{SessionID: "recent", LastSeen: now.Add(-29 * 24 * time.Hour)},
		{SessionID: "old", LastSeen: now.Add(-31 * 24 * time.Hour)},
	}}
	pruneContextRegistry(&r, now)
	if len(r.Sessions) != 1 || r.Sessions[0].SessionID != "recent" {
		t.Fatalf("pruned sessions = %+v", r.Sessions)
	}
}

func TestReadContextRegistryPersistsPruning(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "namespace", "registry.json")
	registry := contextRegistry{Sessions: []localContextSession{
		{SessionID: "recent", LastSeen: time.Now()},
		{SessionID: "old", LastSeen: time.Now().Add(-31 * 24 * time.Hour)},
	}}
	if err := writeContextRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := readContextRegistry(path); err != nil {
		t.Fatal(err)
	}
	persisted, err := readContextRegistryNoLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Sessions) != 1 || persisted.Sessions[0].SessionID != "recent" {
		t.Fatalf("persisted sessions = %+v", persisted.Sessions)
	}
}

func TestTranscriptPathWithinSessionDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	inside := filepath.Join(dir, "session.jsonl")
	outside := filepath.Join(filepath.Dir(dir), "outside.jsonl")
	if !transcriptPathWithinSessionDir(inside, dir) {
		t.Fatal("path beneath session dir rejected")
	}
	if transcriptPathWithinSessionDir(outside, dir) {
		t.Fatal("path outside session dir accepted")
	}
	t.Run("symlink escape", func(t *testing.T) {
		t.Parallel()
		outsideDir := t.TempDir()
		outsideTarget := filepath.Join(outsideDir, "transcript.jsonl")
		if err := os.WriteFile(outsideTarget, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "linked-transcript.jsonl")
		if err := os.Symlink(outsideTarget, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if transcriptPathWithinSessionDir(link, dir) {
			t.Fatal("symlink escaping session dir accepted")
		}
		if file, err := openTranscriptWithinSessionDir(link, dir); err == nil {
			_ = file.Close()
			t.Fatal("opened transcript through symlink escaping session dir")
		}
	})
}

func TestRemoveContextNamespaceWaitsForRegistryWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context", "namespace", "registry.json")
	if err := writeContextRegistry(path, contextRegistry{}); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(filepath.Dir(path) + ".lock")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- removeContextNamespace(context.Background(), path) }()
	select {
	case err := <-done:
		release()
		t.Fatalf("remove completed while writer lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("namespace still exists after serialized removal: %v", err)
	}
}

func TestRemoveContextNamespaceWithoutExistingCacheRoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "context", "namespace", "registry.json")
	if err := removeContextNamespace(t.Context(), path); err != nil {
		t.Fatalf("remove absent namespace: %v", err)
	}
}

func TestRefreshContextRegistrySessionUpdatesHeartbeat(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "context", "namespace", "registry.json")
	old := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	now := old.Add(3 * time.Hour)
	if err := writeContextRegistry(path, contextRegistry{Sessions: []localContextSession{{SessionID: "s1", LastSeen: old}}}); err != nil {
		t.Fatal(err)
	}
	if err := refreshContextRegistrySession(context.Background(), path, "", "s1", now); err != nil {
		t.Fatal(err)
	}
	got, err := readContextRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || !got.Sessions[0].LastSeen.Equal(now) {
		t.Fatalf("heartbeat = %+v, want %s", got.Sessions, now)
	}
}

func TestCurrentContextRegistryPathHonorsEnvToken(t *testing.T) {
	const (
		coreURL   = "https://ci-core.example"
		accountID = "ci-runner"
	)
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(auth.EnvTokenVar, makeJWT(t, `{"alg":"RS256"}`, `{"sub":"`+accountID+`","aud":"`+coreURL+`"}`))

	got, err := currentContextRegistryPath(t.Context())
	if err != nil {
		t.Fatalf("resolve env-token context registry: %v", err)
	}
	want := contextRegistryPath(filepath.Join(cacheRoot, "entire", "context"), coreURL, accountID)
	if got != want {
		t.Fatalf("registry path = %q, want %q", got, want)
	}
}

// TestLocalContextSessionLiveExcludesDeadOwner pins the liveness predicate that
// keeps a crashed agent's session from being offered as live context. A
// heartbeat inside localContextRecentWindow is not proof the agent survived the
// seconds after it, so a positively dead owner must be excluded while an
// unjudgeable one stays (see localContextSessionLive).
func TestLocalContextSessionLiveExcludesDeadOwner(t *testing.T) {
	t.Parallel()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		session localContextSession
		want    bool
	}{
		{"no owner recorded falls back to recency", localContextSession{SessionID: "s"}, true},
		{"live owner is offered", localContextSession{SessionID: "s", Owner: &proclive.Identity{PID: os.Getpid(), Host: host}}, true},
		{
			// Same PID, different start fingerprint: the PID was reused, so the
			// recorded process is gone. This is the deterministic "dead" signal
			// available on every platform that can introspect processes at all
			// (a boot-id mismatch is not — macOS reports no boot id).
			"dead owner is not offered",
			localContextSession{SessionID: "s", Owner: &proclive.Identity{PID: os.Getpid(), Host: host, Start: "not-the-real-start-fingerprint"}},
			false,
		},
		{"owner on another host is unjudgeable and kept", localContextSession{SessionID: "s", Owner: &proclive.Identity{PID: os.Getpid(), Host: host + "-elsewhere"}}, true},
	} {
		if got := localContextSessionLive(tc.session); got != tc.want {
			t.Errorf("%s: localContextSessionLive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLocalContextSessionLiveRejectsExitedProcess is the real-process half of
// the guarantee above: it spawns a process, records its true identity, waits
// for it to exit, and requires the entry to stop being offered — with a fresh
// heartbeat, so only liveness (not the recency window) can exclude it.
func TestLocalContextSessionLiveRejectsExitedProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a probe process: %v", err)
	}
	pid := cmd.Process.Pid
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	session := localContextSession{SessionID: "s", LastSeen: time.Now(), Owner: &proclive.Identity{PID: pid, Host: host}}
	if proclive.Check(*session.Owner) == proclive.LivenessUnknown {
		t.Skip("platform cannot introspect processes; liveness degrades to the recency window by design")
	}
	if localContextSessionLive(session) {
		t.Fatal("a session whose agent process already exited must not be offered as live context")
	}
}

// TestRefreshContextRegistrySessionRefreshesOwner covers the agent-restart case:
// the heartbeat must re-resolve the owner rather than leave the fingerprint of
// a process that is gone, otherwise a restarted agent's still-live session
// would be judged dead forever.
func TestRefreshContextRegistrySessionRefreshesOwner(t *testing.T) {
	t.Parallel()
	path := contextRegistryPath(t.TempDir(), "https://us.auth.entire.io", "account-a")
	stale := &proclive.Identity{PID: os.Getpid(), Host: "some-other-host", Boot: "stale-boot"}
	if err := writeContextRegistry(path, contextRegistry{Sessions: []localContextSession{
		{SessionID: "s1", LastSeen: time.Now().Add(-time.Hour), Owner: stale},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := refreshContextRegistrySession(t.Context(), path, "", "s1", now); err != nil {
		t.Fatal(err)
	}
	registry, err := readContextRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Sessions) != 1 {
		t.Fatalf("sessions = %+v", registry.Sessions)
	}
	got := registry.Sessions[0]
	if !got.LastSeen.Equal(now.UTC()) && !got.LastSeen.Equal(now) {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, now)
	}
	if got.Owner != nil && got.Owner.Boot == "stale-boot" {
		t.Error("heartbeat must re-resolve the owner, not keep the stale fingerprint")
	}
}

// TestLoadLocalContextEvidenceExcludesDeadSession is the wiring half of the
// liveness guarantee: the predicate must actually be consulted by the evidence
// loader, not merely exist. Two sessions are registered for two authorized
// repos with identical, freshly heartbeated transcripts; only the one whose
// owner process is gone must drop out. Both entries sit well inside
// localContextRecentWindow, so recency cannot be what excludes it.
func TestLoadLocalContextEvidenceExcludesDeadSession(t *testing.T) {
	const (
		coreURL    = "https://live-core.example"
		accountID  = "live-account"
		liveRepo   = "01M0K7TYTF4GK0ZWR0YZS2SBRP"
		deadRepo   = "01M0K7TYFPXNV8XH2KVXGXKGVA"
		targetRepo = "01M0K7TYFPXNV8XH2KVXGXKGVH"
	)
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(auth.EnvTokenVar, makeJWT(t, `{"alg":"RS256"}`, `{"sub":"`+accountID+`","aud":"`+coreURL+`"}`))

	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// A real process that has already exited is the dead owner.
	probe := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	if err := probe.Start(); err != nil {
		t.Skipf("cannot spawn a probe process: %v", err)
	}
	deadPID := probe.Process.Pid
	if err := probe.Wait(); err != nil {
		t.Fatal(err)
	}
	deadOwner := proclive.Identity{PID: deadPID, Host: host}
	if proclive.Check(deadOwner) == proclive.LivenessUnknown {
		t.Skip("platform cannot introspect processes; liveness degrades to the recency window by design")
	}

	sessionDir := t.TempDir()
	transcript := filepath.Join(sessionDir, "session.json")
	// Gemini's transcript shape keeps the fixture readable; the loader only
	// needs user/assistant entries it can lexically score against the query.
	if err := os.WriteFile(transcript, []byte(`{"messages":[{"type":"user","content":"explain checkpoint condensation"},{"type":"gemini","content":"condensation rewrites shadow checkpoints"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	registryPath, err := currentContextRegistryPath(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	entry := func(repoID, sessionID string, owner *proclive.Identity) localContextSession {
		return localContextSession{
			RepoID: repoID, RepoName: "ctxtest/" + sessionID, SessionID: sessionID, Agent: string(agent.AgentTypeGemini),
			WorktreeRoot: sessionDir, GitCommonDir: sessionDir, SessionDir: sessionDir,
			TranscriptPath: transcript, LastSeen: now, Owner: owner,
		}
	}
	liveOwner := proclive.Identity{PID: os.Getpid(), Host: host}
	if err := writeContextRegistry(registryPath, contextRegistry{Sessions: []localContextSession{
		entry(liveRepo, "live-session", &liveOwner),
		entry(deadRepo, "dead-session", &deadOwner),
	}}); err != nil {
		t.Fatal(err)
	}

	allowed := map[string]struct{}{liveRepo: {}, deadRepo: {}}
	got, err := loadLocalContextEvidence(t.Context(), "checkpoint condensation", targetRepo, "current-session", allowed)
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, e := range got {
		ids = append(ids, e.SessionID)
	}
	if len(ids) != 1 || ids[0] != "live-session" {
		t.Fatalf("local live evidence sessions = %v, want exactly [live-session]; a session whose agent exited must not be offered", ids)
	}
}
