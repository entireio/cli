package recap

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

func TestLoadRecap_NoSessions(t *testing.T) {
	newIsolatedRepo(t) // t.Chdir set, must NOT t.Parallel()
	out, err := LoadRecap(ctx(), LoadOpts{
		Scope: ScopeLocal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(out.Sessions))
	}
}

func TestLoadRecap_SingleLocalSession(t *testing.T) {
	repo := newIsolatedRepo(t)
	started := time.Now().Add(-2 * time.Hour)
	writeSessionState(t, repo, &session.State{
		SessionID:    "sess-1",
		BaseCommit:   "abc1234",
		StartedAt:    started,
		StepCount:    3,
		FilesTouched: []string{"cmd/entire/cli/checkpoint/store.go"},
		WorktreePath: repo,
	})
	out, err := LoadRecap(ctx(), LoadOpts{Scope: ScopeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	s := out.Sessions[0]
	if s.SessionID != "sess-1" {
		t.Errorf("wrong session id: %s", s.SessionID)
	}
	if s.Source != SourceLocal {
		t.Errorf("expected SourceLocal, got %v", s.Source)
	}
}

func TestLoadOpts_Defaults(t *testing.T) {
	t.Parallel()
	opts := LoadOpts{}
	opts.applyDefaults()
	if opts.Scope != ScopeCurrent {
		t.Errorf("expected ScopeCurrent default, got %v", opts.Scope)
	}
}

func TestLoadRecap_ScopeLocalForcesNoAPI(t *testing.T) {
	repo := newIsolatedRepo(t)
	writeSessionState(t, repo, &session.State{
		SessionID:    "sess-local-only",
		BaseCommit:   "abc1234",
		StartedAt:    time.Now().Add(-1 * time.Hour),
		StepCount:    1,
		FilesTouched: []string{"foo.go"},
	})
	// Even with EnrichFromAPI=true and a token provider, ScopeLocal
	// must prevent any api interaction and leave Source=SourceLocal.
	called := false
	provider := func() (string, error) {
		called = true
		return "fake-token", nil
	}
	out, err := LoadRecap(ctx(), LoadOpts{
		Scope:         ScopeLocal,
		EnrichFromAPI: true,
		TokenProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("token provider should NOT be called for ScopeLocal")
	}
	if out.Sessions[0].Source != SourceLocal {
		t.Errorf("expected SourceLocal, got %v", out.Sessions[0].Source)
	}
}

func TestLoadRecap_EnrichmentOff(t *testing.T) {
	repo := newIsolatedRepo(t)
	writeSessionState(t, repo, &session.State{
		SessionID:    "sess-3",
		BaseCommit:   "abc1234",
		StartedAt:    time.Now().Add(-1 * time.Hour),
		StepCount:    1,
		FilesTouched: []string{"cmd/entire/cli/checkpoint/store.go"},
	})
	out, err := LoadRecap(ctx(), LoadOpts{
		Scope:         ScopeLocal,
		EnrichFromAPI: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Sessions[0].Source != SourceLocal {
		t.Errorf("expected SourceLocal with enrichment off, got %v", out.Sessions[0].Source)
	}
	if len(out.Sessions[0].Labels) != 0 {
		t.Errorf("expected no labels, got %v", out.Sessions[0].Labels)
	}
}
