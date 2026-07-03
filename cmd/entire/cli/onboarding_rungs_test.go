package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/onboarding"
	"github.com/entireio/cli/internal/coreapi"
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

func TestAuthRung_MissingWhenStoredTokenEmpty(t *testing.T) {
	t.Parallel()
	active := &contexts.Context{Name: testCtxProd, Handle: "peyton"}
	deps := onboardingRungDeps{
		envToken: func() string { return "" },
		listContexts: func() ([]*contexts.Context, string, error) {
			return []*contexts.Context{active}, testCtxProd, nil
		},
		tokenForContext: func(*contexts.Context) (string, error) { return "", nil },
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateMissing {
		t.Errorf("State = %v, want StateMissing for an empty stored token", check.State)
	}
}

// A keyring/token-store failure is an infrastructure problem, not "not
// logged in" — rendering Missing would send an already-logged-in user into a
// browser login that fails against the same broken keyring.
func TestAuthRung_UnknownWhenKeyringFails(t *testing.T) {
	t.Parallel()
	active := &contexts.Context{Name: testCtxProd, Handle: "peyton"}
	deps := onboardingRungDeps{
		envToken: func() string { return "" },
		listContexts: func() ([]*contexts.Context, string, error) {
			return []*contexts.Context{active}, testCtxProd, nil
		},
		tokenForContext: func(*contexts.Context) (string, error) {
			return "", errors.New("keyring: locked")
		},
	}

	check := authRung(deps).Check(context.Background())

	if check.State != onboarding.StateUnknown {
		t.Errorf("State = %v, want StateUnknown for a keyring failure", check.State)
	}
	if check.Hint != "entire auth status" {
		t.Errorf("Hint = %q, want the diagnose command", check.Hint)
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

// gitremote returns a distinguishable "no remote" answer as (forge "", nil
// error) via empty forge with an error mentioning the missing remote; a repo
// genuinely without a GitHub origin is NotApplicable, but a resolution
// FAILURE (git exec error, canceled context) must not masquerade as a
// permanent "no GitHub origin".
func TestMirrorRung_UnknownWhenOriginResolutionFails(t *testing.T) {
	t.Parallel()
	deps := onboardingRungDeps{
		resolveOrigin: func(context.Context) (forge, owner, repo string, err error) {
			return "", "", "", errors.New("git remote get-url: context canceled")
		},
	}

	check := mirrorRung(deps).Check(context.Background())

	if check.State != onboarding.StateUnknown {
		t.Errorf("State = %v, want StateUnknown when origin resolution fails", check.State)
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

	if _, _, ok := cache.get("acme/api", now); ok {
		t.Error("empty cache should miss")
	}

	cache.put("acme/api", true, now)
	mirrored, _, ok := cache.get("acme/api", now.Add(5*time.Minute))
	if !ok || !mirrored {
		t.Errorf("fresh entry: get = (%v, %v), want (true, true)", mirrored, ok)
	}

	if _, _, ok := cache.get("acme/api", now.Add(16*time.Minute)); ok {
		t.Error("entry past TTL should miss")
	}

	if _, _, ok := cache.get("other/repo", now); ok {
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

	if _, _, ok := cache.get("acme/api", time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)); ok {
		t.Error("corrupt cache file should behave as a miss")
	}
	// And put should recover by rewriting the file.
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cache.put("acme/api", false, now)
	mirrored, _, ok := cache.get("acme/api", now)
	if !ok || mirrored {
		t.Errorf("after recovery put: get = (%v, %v), want (false, true)", mirrored, ok)
	}
}

type fakeMirrorLister struct {
	origin   string
	mirrored map[string]bool // slug -> mirrored
	err      error
	calls    int
}

func (f *fakeMirrorLister) ListAvailableMirrors(_ context.Context, params coreapi.ListAvailableMirrorsParams) (*coreapi.ListAvailableMirrorsOutputBody, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := &coreapi.ListAvailableMirrorsOutputBody{}
	owner, _ := params.Owner.Get()
	for slug, mirrored := range f.mirrored {
		status := coreapi.AvailableMirrorStatusAvailable
		if mirrored {
			status = coreapi.AvailableMirrorStatusMirrored
		}
		parts := strings.SplitN(slug, "/", 2)
		if parts[0] != owner {
			continue
		}
		out.Available = append(out.Available, coreapi.AvailableMirror{Owner: parts[0], Repo: parts[1], Status: status})
	}
	return out, nil
}

func (f *fakeMirrorLister) CoreOrigin() string { return f.origin }

// The probe must consult every distinct core the mirror could live on: the
// active context's core AND the default cluster the create offer targets.
// Otherwise a mirror created on the default cluster is invisible to an
// active context fronting a different federation, and the rung re-offers
// creation forever.
func TestProbeMirrorAcross_FindsMirrorOnSecondCore(t *testing.T) {
	t.Parallel()
	active := &fakeMirrorLister{origin: "https://eu.core", mirrored: map[string]bool{}}
	cluster := &fakeMirrorLister{origin: "https://us.core", mirrored: map[string]bool{"acme/api": true}}

	mirrored, err := probeMirrorAcross(context.Background(), []availableMirrorLister{active, cluster}, "acme", "api")
	if err != nil || !mirrored {
		t.Errorf("probeMirrorAcross = (%v, %v), want (true, nil)", mirrored, err)
	}
}

func TestProbeMirrorAcross_DedupesSameOrigin(t *testing.T) {
	t.Parallel()
	a := &fakeMirrorLister{origin: "https://us.core", mirrored: map[string]bool{}}
	b := &fakeMirrorLister{origin: "https://us.core", mirrored: map[string]bool{}}

	mirrored, err := probeMirrorAcross(context.Background(), []availableMirrorLister{a, b}, "acme", "api")
	if err != nil || mirrored {
		t.Errorf("probeMirrorAcross = (%v, %v), want (false, nil)", mirrored, err)
	}
	if a.calls+b.calls != 1 {
		t.Errorf("same-origin cores queried %d times, want 1", a.calls+b.calls)
	}
}

func TestProbeMirrorAcross_AllCoresFailingIsAnError(t *testing.T) {
	t.Parallel()
	a := &fakeMirrorLister{origin: "https://us.core", err: errors.New("offline")}
	b := &fakeMirrorLister{origin: "https://eu.core", err: errors.New("offline")}

	if _, err := probeMirrorAcross(context.Background(), []availableMirrorLister{a, b}, "acme", "api"); err == nil {
		t.Error("want error when every core is unreachable")
	}
}

func TestProbeMirrorAcross_PartialFailureStillAnswers(t *testing.T) {
	t.Parallel()
	broken := &fakeMirrorLister{origin: "https://eu.core", err: errors.New("offline")}
	working := &fakeMirrorLister{origin: "https://us.core", mirrored: map[string]bool{"acme/api": true}}

	mirrored, err := probeMirrorAcross(context.Background(), []availableMirrorLister{broken, working}, "acme", "api")
	if err != nil || !mirrored {
		t.Errorf("probeMirrorAcross = (%v, %v), want (true, nil) from the reachable core", mirrored, err)
	}
}

// Probe failures are cached briefly so an authed-but-offline terminal hangs
// on the probe once per failure-TTL, not on every `entire status`.
func TestMirrorProbeCache_UnreachableEntriesExpireSooner(t *testing.T) {
	t.Parallel()
	cache := mirrorProbeCache{path: filepath.Join(t.TempDir(), "mirror.json"), ttl: 15 * time.Minute, failureTTL: 5 * time.Minute}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	cache.putUnreachable("acme/api", now)

	if _, unreachable, ok := cache.get("acme/api", now.Add(4*time.Minute)); !ok || !unreachable {
		t.Errorf("fresh failure entry: get = (unreachable=%v, ok=%v), want (true, true)", unreachable, ok)
	}
	if _, _, ok := cache.get("acme/api", now.Add(6*time.Minute)); ok {
		t.Error("failure entry past failureTTL should miss")
	}
}

func TestImportScanFingerprint_ChangesWithInputs(t *testing.T) {
	t.Parallel()
	base := []importScanInput{
		{Path: "/t/a.jsonl", ModTime: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Size: 100},
		{Path: "/t/b.jsonl", ModTime: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), Size: 200},
	}
	fp := importScanFingerprint(base, "abc123")

	reordered := []importScanInput{base[1], base[0]}
	if importScanFingerprint(reordered, "abc123") != fp {
		t.Error("fingerprint must be order-independent")
	}

	touched := []importScanInput{base[0], {Path: base[1].Path, ModTime: base[1].ModTime.Add(time.Second), Size: base[1].Size}}
	if importScanFingerprint(touched, "abc123") == fp {
		t.Error("appending to a transcript (mtime change) must change the fingerprint")
	}

	if importScanFingerprint(base, "def456") == fp {
		t.Error("a moved metadata branch tip (new checkpoints/imports) must change the fingerprint")
	}
}

func TestImportScanCache_HitRequiresMatchingFingerprint(t *testing.T) {
	t.Parallel()
	cache := importScanCache{path: filepath.Join(t.TempDir(), "imports.json")}
	statuses := []agentImportStatus{{Agent: "claude-code", Sessions: 2, UnimportedTurns: 5}}

	if _, ok := cache.get("/repo", "fp1"); ok {
		t.Error("empty cache should miss")
	}

	cache.put("/repo", "fp1", statuses)
	got, ok := cache.get("/repo", "fp1")
	if !ok || len(got) != 1 || got[0].UnimportedTurns != 5 {
		t.Errorf("get = (%+v, %v), want cached statuses", got, ok)
	}

	if _, ok := cache.get("/repo", "fp2"); ok {
		t.Error("stale fingerprint should miss")
	}
	if _, ok := cache.get("/other", "fp1"); ok {
		t.Error("different repo should miss")
	}
}

// An explicit `entire enable` must not be starved by a cached probe failure:
// the user asked for setup, so they consent to paying the probe again. Only
// unreachable entries are dropped — successful results stay cached.
func TestMirrorProbeCache_ClearUnreachable(t *testing.T) {
	t.Parallel()
	cache := mirrorProbeCache{path: filepath.Join(t.TempDir(), "mirror.json"), ttl: 15 * time.Minute, failureTTL: 5 * time.Minute}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cache.put("acme/api", true, now)
	cache.putUnreachable("acme/other", now)

	cache.clearUnreachable()

	if mirrored, _, ok := cache.get("acme/api", now); !ok || !mirrored {
		t.Error("clearUnreachable must keep successful results")
	}
	if _, _, ok := cache.get("acme/other", now); ok {
		t.Error("clearUnreachable must drop failure entries")
	}
}
