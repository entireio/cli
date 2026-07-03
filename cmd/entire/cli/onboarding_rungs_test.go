package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/onboarding"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

func TestHooksRung_DoneWithInstalledAgents(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		installedAgents: func(context.Context) []string { return []string{"Claude Code", "Cursor"} },
	}

	check := hooksRung(deps).Check(context.Background())

	if check.State != onboarding.StateDone {
		t.Errorf("State = %v, want StateDone", check.State)
	}
	if check.Detail != "Claude Code, Cursor" {
		t.Errorf("Detail = %q, want agent list", check.Detail)
	}
}

func TestHooksRung_MissingWhenNoAgents(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		installedAgents: func(context.Context) []string { return nil },
	}

	check := hooksRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing", check.State)
	}
	if check.Hint != "entire enable" {
		t.Errorf("Hint = %q, want %q", check.Hint, "entire enable")
	}
}

func TestAuthRung_DoneWithEnvToken(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		envToken: func() string { return "some-jwt" },
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateDone {
		t.Errorf("State = %v, want StateDone", check.State)
	}
	if check.Detail != "using ENTIRE_TOKEN" {
		t.Errorf("Detail = %q, want %q", check.Detail, "using ENTIRE_TOKEN")
	}
}

func TestAuthRung_DoneWithActiveContextToken(t *testing.T) {
	t.Parallel()
	active := &contexts.Context{Name: testCtxProd, Handle: "peyton", CoreURL: "https://core.example"}
	deps := onboardingRungDeps{
		envToken: func() string { return "" },
		listContexts: func() ([]*contexts.Context, string, error) {
			return []*contexts.Context{active}, testCtxProd, nil
		},
		tokenForContext: func(c *contexts.Context) (string, error) {
			if c != active {
				t.Errorf("tokenForContext called with %+v, want active context", c)
			}
			return "stored-jwt", nil
		},
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateDone {
		t.Errorf("State = %v, want StateDone", check.State)
	}
	if check.Detail != "peyton" {
		t.Errorf("Detail = %q, want handle %q", check.Detail, "peyton")
	}
}

func TestAuthRung_MissingWhenNoContexts(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		envToken:     func() string { return "" },
		listContexts: func() ([]*contexts.Context, string, error) { return nil, "", nil },
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing", check.State)
	}
	if check.Hint != "entire auth login" {
		t.Errorf("Hint = %q, want %q", check.Hint, "entire auth login")
	}
}

func TestAuthRung_MissingWhenStoredTokenGone(t *testing.T) {
	t.Parallel()
	active := &contexts.Context{Name: testCtxProd, Handle: "peyton"}
	deps := onboardingRungDeps{
		envToken: func() string { return "" },
		listContexts: func() ([]*contexts.Context, string, error) {
			return []*contexts.Context{active}, testCtxProd, nil
		},
		tokenForContext: func(*contexts.Context) (string, error) {
			return "", errors.New("keyring: not found")
		},
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing", check.State)
	}
}

func TestAuthRung_UnknownWhenContextStoreUnreadable(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		envToken: func() string { return "" },
		listContexts: func() ([]*contexts.Context, string, error) {
			return nil, "", errors.New("contexts.json: corrupt")
		},
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateUnknown {
		t.Errorf("State = %v, want StateUnknown for unreadable context store", check.State)
	}
}

func TestMirrorRung_NotApplicableWithoutGitHubOrigin(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (forge, owner, repo string, err error) {
			return "gl", testOwnerAcme, testRepoAPI, nil // GitLab origin
		},
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateNotApplicable {
		t.Errorf("State = %v, want StateNotApplicable for non-GitHub origin", check.State)
	}
}

func TestMirrorRung_NotApplicableWithoutOrigin(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (forge, owner, repo string, err error) {
			return "", "", "", errors.New("no origin remote")
		},
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateNotApplicable {
		t.Errorf("State = %v, want StateNotApplicable when origin is missing", check.State)
	}
}

func TestMirrorRung_BlockedWhenNotLoggedIn(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (string, string, string, error) {
			return "gh", testOwnerAcme, testRepoAPI, nil
		},
		authed: func(context.Context) bool { return false },
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateBlocked {
		t.Errorf("State = %v, want StateBlocked without login", check.State)
	}
	if check.Hint != "entire auth login" {
		t.Errorf("Hint = %q, want login hint", check.Hint)
	}
}

func TestMirrorRung_DoneWhenMirrored(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (string, string, string, error) {
			return "gh", "Acme", "API", nil // mixed case from remote URL
		},
		authed: func(context.Context) bool { return true },
		probeMirror: func(_ context.Context, owner, repo string) (bool, error) {
			if owner != testOwnerAcme || repo != testRepoAPI {
				t.Errorf("probeMirror(%q, %q), want lowercased acme/api", owner, repo)
			}
			return true, nil
		},
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateDone {
		t.Errorf("State = %v, want StateDone", check.State)
	}
	if check.Detail != "github.com/acme/api" {
		t.Errorf("Detail = %q, want slug", check.Detail)
	}
}

func TestMirrorRung_MissingWhenNotMirrored(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (string, string, string, error) {
			return "gh", testOwnerAcme, testRepoAPI, nil
		},
		authed:      func(context.Context) bool { return true },
		probeMirror: func(context.Context, string, string) (bool, error) { return false, nil },
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing", check.State)
	}
	if check.Hint != "entire repo mirror create github.com/acme/api" {
		t.Errorf("Hint = %q, want mirror create hint with slug", check.Hint)
	}
}

func TestMirrorRung_UnknownWhenProbeFails(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (string, string, string, error) {
			return "gh", testOwnerAcme, testRepoAPI, nil
		},
		authed:      func(context.Context) bool { return true },
		probeMirror: func(context.Context, string, string) (bool, error) { return false, errors.New("timeout") },
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateUnknown {
		t.Errorf("State = %v, want StateUnknown on probe failure (offline)", check.State)
	}
}

func TestImportRung_NotApplicableWithoutHistory(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		discoverImports: func(context.Context) ([]agentImportStatus, error) { return nil, nil },
	}

	check := importRung(deps).Check(context.Background())

	if check.State != onboarding.StateNotApplicable {
		t.Errorf("State = %v, want StateNotApplicable with no discoverable history", check.State)
	}
}

func TestImportRung_DoneWhenAllImported(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		discoverImports: func(context.Context) ([]agentImportStatus, error) {
			return []agentImportStatus{{Agent: "claude-code", Sessions: 7, UnimportedTurns: 0}}, nil
		},
	}

	check := importRung(deps).Check(context.Background())

	if check.State != onboarding.StateDone {
		t.Errorf("State = %v, want StateDone when history is fully imported", check.State)
	}
	if check.Detail != "7 sessions imported" {
		t.Errorf("Detail = %q, want %q", check.Detail, "7 sessions imported")
	}
}

func TestImportRung_MissingWhenUnimportedHistory(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		discoverImports: func(context.Context) ([]agentImportStatus, error) {
			return []agentImportStatus{
				{Agent: "claude-code", Sessions: 12, UnimportedTurns: 40},
				{Agent: "cursor", Sessions: 3, UnimportedTurns: 0},
			}, nil
		},
	}

	check := importRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing", check.State)
	}
	if check.Detail != "12 claude-code sessions found, not imported" {
		t.Errorf("Detail = %q, want unimported summary", check.Detail)
	}
	if check.Hint != "entire import claude-code" {
		t.Errorf("Hint = %q, want import command for the unimported agent", check.Hint)
	}
}

func TestImportRung_UnknownOnDiscoveryError(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		discoverImports: func(context.Context) ([]agentImportStatus, error) {
			return nil, errors.New("checkpoint store unavailable")
		},
	}

	check := importRung(deps).Check(context.Background())

	if check.State != onboarding.StateUnknown {
		t.Errorf("State = %v, want StateUnknown on discovery error", check.State)
	}
}

func TestImportRung_SingularSessionCopy(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		discoverImports: func(context.Context) ([]agentImportStatus, error) {
			return []agentImportStatus{{Agent: "claude-code", Sessions: 1, UnimportedTurns: 3}}, nil
		},
	}

	check := importRung(deps).Check(context.Background())

	if check.Detail != "1 claude-code session found, not imported" {
		t.Errorf("Detail = %q, want singular 'session'", check.Detail)
	}
}

// A partial prior import (some turns already imported) must not claim every
// discovered session is unimported.
func TestImportRung_PartialImportWording(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		discoverImports: func(context.Context) ([]agentImportStatus, error) {
			return []agentImportStatus{
				{Agent: "claude-code", Sessions: 10, UnimportedTurns: 4, ImportedTurns: 36},
			}, nil
		},
	}

	check := importRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing", check.State)
	}
	if check.Detail != "claude-code history partially imported (4 turns pending)" {
		t.Errorf("Detail = %q, want partial-import wording", check.Detail)
	}
}

func TestMirrorProbeCache_RoundTripAndTTL(t *testing.T) {
	t.Parallel()
	cache := mirrorProbeCache{path: filepath.Join(t.TempDir(), "mirror.json"), ttl: 15 * time.Minute}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	if _, ok := cache.get("acme/api", now); ok {
		t.Error("empty cache should miss")
	}

	cache.put("acme/api", true, now)
	mirrored, ok := cache.get("acme/api", now.Add(5*time.Minute))
	if !ok || !mirrored {
		t.Errorf("fresh entry: get = (%v, %v), want (true, true)", mirrored, ok)
	}

	if _, ok := cache.get("acme/api", now.Add(16*time.Minute)); ok {
		t.Error("entry past TTL should miss")
	}

	if _, ok := cache.get("other/repo", now); ok {
		t.Error("unrelated slug should miss")
	}
}

func TestMirrorProbeCache_CorruptFileIsAMiss(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "mirror.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := mirrorProbeCache{path: path, ttl: 15 * time.Minute}

	if _, ok := cache.get("acme/api", time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)); ok {
		t.Error("corrupt cache file should behave as a miss")
	}
	// And put should recover by rewriting the file.
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cache.put("acme/api", false, now)
	mirrored, ok := cache.get("acme/api", now)
	if !ok || mirrored {
		t.Errorf("after recovery put: get = (%v, %v), want (false, true)", mirrored, ok)
	}
}
