package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// depTestOldTag is the below-min_version tag used across dependency tests.
const depTestOldTag = "v0.1.0"

// withIsolatedPath shrinks $PATH to git's dir plus the system basics, so a
// developer whose shell PATH includes a real managed plugin dir (the normal
// case for anyone using entire plugins) can't leak entire-* binaries into
// dependencySatisfied's LookPath fallback.
// withIsolatedPluginEnv is the isolation every plugin test needs: a scratch
// managed dir plus a PATH that cannot see the developer's real plugins. The two
// were always applied together, 20 times, never one without the other.
func withIsolatedPluginEnv(t *testing.T) {
	t.Helper()
	withPluginDir(t)
	withIsolatedPath(t)
}

func withIsolatedPath(t *testing.T) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(gitPath), "/usr/bin", "/bin"}, string(filepath.ListSeparator)))
}

func TestDependentsOf(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	mustSave := func(m *PluginManifest) {
		t.Helper()
		if err := SavePluginManifest(m); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(&PluginManifest{Name: "brain", RepoURL: "https://x.example/entire-brain", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "sem"}}})
	mustSave(&PluginManifest{Name: "viz", RepoURL: "https://x.example/entire-viz", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "sem"}, {Name: "run"}}})
	mustSave(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: "v1.0.0"})

	deps, err := DependentsOf("sem")
	if err != nil {
		t.Fatalf("DependentsOf: %v", err)
	}
	if len(deps) != 2 || deps[0] != "brain" || deps[1] != "viz" {
		t.Errorf("DependentsOf(sem) = %v, want [brain viz]", deps)
	}
	if deps, err := DependentsOf("brain"); err != nil || len(deps) != 0 {
		t.Errorf("DependentsOf(brain) = %v, want none", deps)
	}
}

func TestDependencySatisfied_ManagedManifest(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	if err := SavePluginManifest(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: "v0.3.0"}); err != nil {
		t.Fatal(err)
	}
	st, err := dependencySatisfied(PluginRequirement{Name: "sem", MinVersion: "v0.2.0"})
	if err != nil || !st.Satisfied || st.Warning != "" {
		t.Errorf("satisfied above min: %+v err=%v", st, err)
	}
	// The manifest it loaded comes back, so callers don't re-read the file.
	if st.Manifest == nil || st.Manifest.Tag != "v0.3.0" {
		t.Errorf("manifest not returned: %+v", st.Manifest)
	}
	st, err = dependencySatisfied(PluginRequirement{Name: "sem", MinVersion: "v0.4.0"})
	if err != nil || st.Satisfied {
		t.Errorf("below min must be unsatisfied: %+v err=%v", st, err)
	}
	if st.Manifest == nil {
		t.Error("the upgrade path needs the installed manifest")
	}
	st, err = dependencySatisfied(PluginRequirement{Name: "ghost"})
	if err != nil || st.Satisfied || st.Manifest != nil {
		t.Errorf("missing dep must be unsatisfied with no manifest: %+v err=%v", st, err)
	}
}

func TestPlanDependencyInstalls(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	// sem installed but old; run missing and resolvable via index;
	// path-only is satisfied via local-dev managed entry (no manifest).
	if err := SavePluginManifest(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: depTestOldTag}); err != nil {
		t.Fatal(err)
	}
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "run", RepoURL: newTaggedPluginRepo(t, "", "v1.0.0")},
	}}

	plan, err := PlanDependencyInstalls(ctx, []PluginRequirement{
		{Name: "sem", MinVersion: "v0.2.0"},
		{Name: "run"},
	}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 2 {
		t.Fatalf("actions = %+v, want 2", plan.Actions)
	}
	bySemName := map[string]DepAction{}
	for _, a := range plan.Actions {
		bySemName[a.Name] = a
	}
	if a := bySemName["sem"]; !a.Upgrade || a.CurrentTag != depTestOldTag || a.RepoURL != "https://x.example/entire-sem" {
		t.Errorf("sem action = %+v, want upgrade from its recorded repo", a)
	}
	if a := bySemName["run"]; a.Upgrade || a.RepoURL == "" {
		t.Errorf("run action = %+v, want a fresh install resolved from the index", a)
	}
}

func TestPlanDependencyInstalls_UnresolvableDep(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	_, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "mystery"}}, &PluginIndex{Version: 1})
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Errorf("err = %v, want unresolvable-dependency error naming the plugin", err)
	}
}

func TestPlanDependencyInstalls_VisitedSetBreaksCycles(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	// a requires b; b requires a. Both missing, both resolvable. The
	// visited set must terminate planning with each appearing once.
	repoA := newTaggedPluginRepo(t, "name: cyca\nrequires:\n  - name: cycb\n", "v1.0.0")
	repoB := newTaggedPluginRepo(t, "name: cycb\nrequires:\n  - name: cyca\n", "v1.0.0")
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "cyca", RepoURL: repoA},
		{Name: "cycb", RepoURL: repoB},
	}}
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "cyca"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 2 {
		t.Errorf("actions = %+v, want exactly [cyca cycb]", plan.Actions)
	}
}

func TestRunPluginDoctor_FlagsMissingAndOutdatedDeps(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	if err := SavePluginManifest(&PluginManifest{Name: "brain", RepoURL: "https://x.example/entire-brain", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "sem", MinVersion: "v0.2.0"}, {Name: "ghost"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SavePluginManifest(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: depTestOldTag}); err != nil {
		t.Fatal(err)
	}
	issues, err := RunPluginDoctor(context.Background())
	if err != nil {
		t.Fatalf("RunPluginDoctor: %v", err)
	}
	var problems []string
	for _, i := range issues {
		problems = append(problems, i.Problem)
	}
	joined := strings.Join(problems, " | ")
	if !strings.Contains(joined, "requires sem >= v0.2.0") {
		t.Errorf("doctor missed outdated dep: %s", joined)
	}
	if !strings.Contains(joined, `requires "ghost"`) {
		t.Errorf("doctor missed missing dep: %s", joined)
	}
	// brain and sem have manifests but no bin entries in this synthetic
	// setup — doctor flags that too.
	if !strings.Contains(joined, "no entry in the managed bin dir") {
		t.Errorf("doctor missed manifest-without-bin: %s", joined)
	}
}

func TestPlanDependencyInstalls_WalksSatisfiedTransitives(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	// sem is installed and satisfied, but its own recorded requirement
	// ("leaf") is missing — e.g. removed with --force after install.
	// Planning a parent that requires sem must surface leaf.
	if err := SavePluginManifest(&PluginManifest{
		Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "leaf"}},
	}); err != nil {
		t.Fatal(err)
	}
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "leaf", RepoURL: newTaggedPluginRepo(t, "", "v1.0.0")},
	}}
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "sem"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Name != "leaf" {
		t.Errorf("actions = %+v, want [leaf] via satisfied sem's manifest", plan.Actions)
	}
}

func TestPlanDependencyInstalls_WarnsOnUninspectableDep(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	// The dependency's repo has no tags, so its own requirements can't be
	// inspected during planning. The action must still be planned, with a
	// warning instead of silence.
	untaggedRepo := newTaggedPluginRepo(t, "") // commit, no tags
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "leaf", RepoURL: untaggedRepo},
	}}
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "leaf"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Name != "leaf" {
		t.Fatalf("actions = %+v, want [leaf]", plan.Actions)
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "leaf") && strings.Contains(w, "not inspected") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want uninspectable-dep warning naming leaf", plan.Warnings)
	}
}

// The manifest's SHA256 covers the downloaded asset — usually an archive that
// is discarded with the staging dir — so it can never detect tampering of the
// thing actually executed. BinarySHA256 covers the installed binary, which is
// what doctor re-hashes.
func TestRunPluginDoctor_DetectsTamperedBinary(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)

	dir, err := EnsurePluginPkgDir("demo")
	if err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, pluginBinaryName("demo"))
	if err := os.WriteFile(binPath, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(binPath)
	if err != nil {
		t.Fatal(err)
	}
	save := func(m *PluginManifest) {
		t.Helper()
		if err := SavePluginManifest(m); err != nil {
			t.Fatal(err)
		}
	}
	base := &PluginManifest{Name: "demo", RepoURL: "https://x.example/entire-demo", Tag: "v1.0.0", BinarySHA256: digest}
	save(base)

	doctorProblems := func() string {
		t.Helper()
		issues, err := RunPluginDoctor(context.Background())
		if err != nil {
			t.Fatalf("RunPluginDoctor: %v", err)
		}
		var problems []string
		for _, i := range issues {
			problems = append(problems, i.Problem)
		}
		return strings.Join(problems, " | ")
	}

	// Intact binary: no integrity complaint.
	if got := doctorProblems(); strings.Contains(got, "no longer matches") {
		t.Errorf("doctor flagged an intact binary: %s", got)
	}

	// Swapped out under the managed dir — the case the digest exists for.
	if err := os.WriteFile(binPath, []byte("malicious"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := doctorProblems(); !strings.Contains(got, "no longer matches the digest recorded at install") {
		t.Errorf("doctor missed a tampered binary: %s", got)
	}

	// A manifest predating integrity recording has nothing to compare, so
	// staying quiet is correct rather than nagging about a likely-fine plugin.
	save(&PluginManifest{Name: "demo", RepoURL: base.RepoURL, Tag: base.Tag})
	if got := doctorProblems(); strings.Contains(got, "no longer matches") {
		t.Errorf("doctor flagged a manifest without BinarySHA256: %s", got)
	}

	// An unverified install is a standing fact worth surfacing.
	save(&PluginManifest{Name: "demo", RepoURL: base.RepoURL, Tag: base.Tag, Unverified: true})
	if got := doctorProblems(); !strings.Contains(got, "without checksum verification") {
		t.Errorf("doctor did not surface the unverified install: %s", got)
	}
}

// Diamond dependency with differing minimums: A needs sem >= v1.0.0, B needs
// sem >= v2.0.0, and sem is installed at v1.5.0. A name-only visited set
// marked sem handled on the first (satisfied) requirement and skipped the
// second entirely — no action, no warning — so the install completed leaving B
// against a too-old sem. Doctor caught it after the fact, but the install
// should plan the upgrade rather than defer the discovery.
func TestPlanDependencyInstalls_StricterConstraintOnVisitedDep(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	const dep, depRepo = "sem", "https://x.example/entire-sem"
	if err := SavePluginManifest(&PluginManifest{
		Name: dep, RepoURL: depRepo, Tag: "v1.5.0",
	}); err != nil {
		t.Fatal(err)
	}
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{{Name: dep}}}

	// Ordered weakest-first: the weak requirement is satisfied and would have
	// closed the name off to the stricter one that follows.
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{
		{Name: dep, MinVersion: "v1.0.0"},
		{Name: dep, MinVersion: "v2.0.0"},
	}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("actions = %+v, want exactly one upgrade for sem", plan.Actions)
	}
	a := plan.Actions[0]
	if a.Name != dep || !a.Upgrade || a.CurrentTag != "v1.5.0" || a.MinVersion != "v2.0.0" {
		t.Errorf("action = %+v, want sem upgrade from v1.5.0 for min v2.0.0", a)
	}

	// The reverse order must not double-plan: the stricter requirement is
	// handled first and the weaker one adds nothing.
	plan, err = PlanDependencyInstalls(context.Background(), []PluginRequirement{
		{Name: dep, MinVersion: "v2.0.0"},
		{Name: dep, MinVersion: "v1.0.0"},
	}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls (reversed): %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Errorf("reversed actions = %+v, want exactly one", plan.Actions)
	}

	// A dep with no minimum at all stays satisfied and unplanned.
	plan, err = PlanDependencyInstalls(context.Background(), []PluginRequirement{
		{Name: dep},
	}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls (no minimum): %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("no-minimum actions = %+v, want none", plan.Actions)
	}
}

// A missing dependency resolves by name through the index and nowhere else.
// entire-plugin.yml has no repo_url field, so a plugin author cannot point a
// dependency install at a URL of their choosing — planning used to contact that
// URL before the confirmation prompt. The capability moved to the user, who can
// install an out-of-catalog dependency themselves and thereby satisfy it.
func TestPlanDependencyInstalls_MissingDepMustBeIndexed(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	listed := newTaggedPluginRepo(t, "", "v1.0.0")
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{{Name: "listed", RepoURL: listed}}}

	// In the index: planned, from the catalog's URL.
	plan, err := PlanDependencyInstalls(ctx, []PluginRequirement{{Name: "listed"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].RepoURL != listed {
		t.Fatalf("actions = %+v, want one action using the index URL", plan.Actions)
	}

	// Not in the index: refused, with the two ways forward named.
	_, err = PlanDependencyInstalls(ctx, []PluginRequirement{{Name: "absent"}}, idx)
	if err == nil {
		t.Fatal("planning accepted a dependency that is not in the index")
	}
	for _, want := range []string{"not in the plugin index", "plugin search", "install it yourself"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}

	// A user who installs the dependency themselves satisfies the requirement,
	// which is the escape hatch — held by the user, not the plugin author.
	if err := SavePluginManifest(&PluginManifest{
		Name: "absent", RepoURL: "https://x.example/entire-absent", Tag: "v1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanDependencyInstalls(ctx, []PluginRequirement{{Name: "absent"}}, idx)
	if err != nil {
		t.Fatalf("a self-installed dependency should satisfy the requirement: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("actions = %+v, want none", plan.Actions)
	}
}

// ExecuteDepPlan installs the newest published tag, which may still be below
// the minimum the plan computed. Reporting success there would silently defeat
// the guarantee the plan was built on and leave doctor complaining forever
// about a dependency the command just claimed to install.
func TestExecuteDepPlan_RejectsOutcomeBelowMinVersion(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")

	// Newest available is v0.1.0; the plan demands v9.0.0.
	plan := &DepPlan{Actions: []DepAction{{Name: "demo", RepoURL: repoURL, MinVersion: "v9.0.0"}}}
	_, err := ExecuteDepPlan(context.Background(), plan, false)
	if err == nil {
		t.Fatal("ExecuteDepPlan reported success for an unmet minimum")
	}
	for _, want := range []string{"demo", "v0.1.0", "v9.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	// The check runs after the install, so the dependency is on disk at the
	// too-old version. That is deliberate and recoverable: doctor reports
	// "requires demo >= v9.0.0 but v0.1.0 is installed", which is accurate and
	// actionable, rather than leaving nothing behind to diagnose.
	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil || m.Tag != remoteTestTagOld {
		t.Errorf("expected demo installed at %s, got %+v (%v)", remoteTestTagOld, m, err)
	}

	// A minimum the newest tag does satisfy succeeds. Upgrade:true because the
	// failed action above already installed it, so this needs Force.
	plan = &DepPlan{Actions: []DepAction{{Name: "demo", RepoURL: repoURL, MinVersion: remoteTestTagOld, Upgrade: true}}}
	if _, err := ExecuteDepPlan(context.Background(), plan, false); err != nil {
		t.Errorf("satisfiable minimum should install: %v", err)
	}
}

// Doctor's repair suggestion has to be a command that works. A plugin installed
// with --allow-unverified needs the same flag on the reinstall, or the fix
// fails with errUnverifiedAsset on exactly the plugins doctor flags.
func TestDoctorReinstallCommand_CarriesAllowUnverified(t *testing.T) {
	t.Parallel()
	verified := &PluginManifest{Name: "demo", RepoURL: "https://x.example/entire-demo"}
	if got := reinstallCommand(verified); strings.Contains(got, "allow-unverified") {
		t.Errorf("verified install should not need the opt-in: %q", got)
	}
	unverified := &PluginManifest{Name: "demo", RepoURL: "https://x.example/entire-demo", Unverified: true}
	got := reinstallCommand(unverified)
	if !strings.Contains(got, "--force") || !strings.Contains(got, "--allow-unverified") {
		t.Errorf("unverified install needs both flags to be reinstallable: %q", got)
	}
}
