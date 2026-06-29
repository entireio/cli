package review

import (
	"bytes"
	"context"
	"strings"
	"testing"

	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"
)

const (
	tAgentClaude = "claude-code"
	tAgentCodex  = "codex"
	tModelOpus   = "opus"
	tModelSonnet = "sonnet"
)

func configureTestDeps(adapter ...string) Deps {
	set := map[string]struct{}{}
	for _, a := range adapter {
		set[a] = struct{}{}
	}
	return Deps{
		GetAgentsWithHooksInstalled: func(context.Context) []agenttypes.AgentName {
			out := make([]agenttypes.AgentName, 0, len(set))
			for name := range set {
				out = append(out, agenttypes.AgentName(name))
			}
			return out
		},
		NewSilentError: func(err error) error { return err },
		ReviewerFor: func(name string) reviewtypes.AgentReviewer {
			if _, ok := set[name]; ok {
				return &stubReviewer{name: name}
			}
			return nil
		},
	}
}

func TestBuildConfiguredProfile_FromFlags(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code", "codex")
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{
			Agents: []string{"claude-code", "codex"},
			Judge:  "codex",
			Models: []string{"claude-code=opus"},
		},
		&settings.EntireSettings{},
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if len(profile.Agents) != 2 {
		t.Fatalf("agents = %d, want 2: %#v", len(profile.Agents), profile.Agents)
	}
	if got := profile.Agents["claude-code"].Model; got != "opus" {
		t.Errorf("claude-code model = %q, want opus", got)
	}
	if profile.Judge == nil || profile.Judge.Agent != "codex" {
		t.Errorf("judge = %#v, want codex", profile.Judge)
	}
	if profile.Task == "" {
		t.Error("task should default to the built-in general task")
	}
}

func TestBuildConfiguredProfile_FromSlots_AllowsDuplicateAgents(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code", "codex")
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{
			Slots: []string{tAgentClaude + "=" + tModelOpus, tAgentClaude + "=" + tModelSonnet, tAgentClaude, tAgentClaude},
		},
		&settings.EntireSettings{},
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	// Four distinct workers: two different models + two identical default slots.
	if len(profile.Agents) != 4 {
		t.Fatalf("agents = %d, want 4: %#v", len(profile.Agents), profile.Agents)
	}
	models := map[string]int{}
	for _, cfg := range profile.Agents {
		if cfg.Agent != tAgentClaude {
			t.Errorf("worker agent = %q, want claude-code", cfg.Agent)
		}
		models[cfg.Model]++
	}
	if models[tModelOpus] != 1 || models[tModelSonnet] != 1 || models[""] != 2 {
		t.Errorf("model distribution = %#v, want opus:1 sonnet:1 default:2", models)
	}
}

func TestProfileJudge(t *testing.T) {
	t.Parallel()
	t.Run("explicit judge resolves with model", func(t *testing.T) {
		t.Parallel()
		profile := settings.ReviewProfileConfig{
			Agents: map[string]settings.ReviewConfig{tAgentCodex: {Agent: tAgentCodex}},
			Judge:  &settings.ReviewConfig{Agent: tAgentClaude, Model: tModelOpus},
		}
		j, ok := profileJudge(profile)
		if !ok || j.agent != tAgentClaude || j.model != tModelOpus {
			t.Fatalf("got (%#v,%v), want claude-code/opus, true", j, ok)
		}
	})
	t.Run("no judge", func(t *testing.T) {
		t.Parallel()
		profile := settings.ReviewProfileConfig{
			Agents: map[string]settings.ReviewConfig{tAgentCodex: {Agent: tAgentCodex}},
		}
		if _, ok := profileJudge(profile); ok {
			t.Fatal("expected ok=false when no judge is set")
		}
	})
}

func TestBuildConfiguredProfile_Judge(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code", "codex")
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{
			Agents: []string{tAgentClaude, tAgentCodex},
			Judge:  tAgentClaude + "=" + tModelOpus,
		},
		&settings.EntireSettings{},
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if profile.Judge == nil || profile.Judge.Agent != tAgentClaude || profile.Judge.Model != tModelOpus {
		t.Fatalf("judge = %#v, want claude-code/opus", profile.Judge)
	}
	j, ok := profileJudge(profile)
	if !ok || j.agent != tAgentClaude || j.model != tModelOpus {
		t.Errorf("profileJudge = (%#v,%v), want claude-code/opus, true", j, ok)
	}
}

func TestBuildConfiguredProfile_RejectsNonAdapterAgent(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code")
	_, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{"cursor"}},
		&settings.EntireSettings{},
		deps,
	)
	if err == nil {
		t.Fatal("expected error for agent without a review-runner adapter")
	}
}

func TestBuildConfiguredProfile_PreservesExistingTask(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code", "codex")
	s := &settings.EntireSettings{
		ReviewProfiles: map[string]settings.ReviewProfileConfig{
			"general": {
				Task: "Custom task text.",
				Agents: map[string]settings.ReviewConfig{
					"claude-code": {Skills: []string{"/review"}},
				},
			},
		},
	}
	// Only change the worker set; the custom task must survive.
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{"claude-code", "codex"}},
		s,
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if profile.Task != "Custom task text." {
		t.Errorf("task = %q, want preserved custom task", profile.Task)
	}
	// Two reviewers with no explicit judge → one auto-selected.
	if _, ok := profileJudge(profile); !ok {
		t.Error("expected an auto-selected judge for a multi-reviewer profile")
	}
}

func TestSelectReviewProfile_LegacyReviewFallback(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{
		Review: map[string]settings.ReviewConfig{
			tAgentClaude: {Skills: []string{"/review"}, Model: tModelSonnet},
		},
	}

	name, profile, err := selectReviewProfile(s, "")
	if err != nil {
		t.Fatalf("selectReviewProfile: %v", err)
	}
	if name != DefaultProfileName {
		t.Fatalf("profile name = %q, want %s", name, DefaultProfileName)
	}
	cfg, ok := profile.Agents[tAgentClaude]
	if !ok {
		t.Fatalf("legacy agent missing from fallback profile: %#v", profile.Agents)
	}
	if cfg.Model != tModelSonnet || strings.Join(cfg.Skills, ",") != "/review" {
		t.Fatalf("fallback config = %#v, want legacy config", cfg)
	}
	if s.ReviewDefaultProfile != DefaultProfileName {
		t.Fatalf("default profile = %q, want %s", s.ReviewDefaultProfile, DefaultProfileName)
	}
}

func TestSelectReviewProfile_ConfiguredProfilesOverrideLegacyReview(t *testing.T) {
	t.Parallel()
	const securityProfile = "security"
	s := &settings.EntireSettings{
		Review: map[string]settings.ReviewConfig{
			tAgentClaude: {Skills: []string{"/legacy"}},
		},
		ReviewProfiles: map[string]settings.ReviewProfileConfig{
			securityProfile: {
				Agents: map[string]settings.ReviewConfig{tAgentCodex: {Skills: []string{"/review"}}},
			},
		},
		ReviewDefaultProfile: securityProfile,
	}

	name, profile, err := selectReviewProfile(s, "")
	if err != nil {
		t.Fatalf("selectReviewProfile: %v", err)
	}
	if name != securityProfile {
		t.Fatalf("profile name = %q, want %s", name, securityProfile)
	}
	if _, ok := profile.Agents[tAgentClaude]; ok {
		t.Fatalf("legacy review config leaked into configured profile: %#v", profile.Agents)
	}
	if _, ok := s.ReviewProfiles[DefaultProfileName]; ok {
		t.Fatalf("legacy fallback profile was added despite configured profiles: %#v", s.ReviewProfiles)
	}
}

func TestProfileOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{"", ReviewOutputLocal},
		{"local", ReviewOutputLocal},
		{"LOCAL", ReviewOutputLocal},
		{"trail", ReviewOutputTrail},
		{" Trail ", ReviewOutputTrail},
		{"bogus", ReviewOutputLocal},
	}
	for _, c := range cases {
		if got := profileOutput(settings.ReviewProfileConfig{Output: c.raw}); got != c.want {
			t.Errorf("profileOutput(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestBuildConfiguredProfile_OutputTrail(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code", "codex")
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{tAgentClaude}, Output: "trail"},
		&settings.EntireSettings{},
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if profile.Output != ReviewOutputTrail {
		t.Errorf("output = %q, want trail", profile.Output)
	}
}

func TestBuildConfiguredProfile_OutputLocalStoredEmpty(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code")
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{tAgentClaude}, Output: "local"},
		&settings.EntireSettings{},
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if profile.Output != "" {
		t.Errorf("output = %q, want empty (default local stored as omitted)", profile.Output)
	}
}

func TestBuildConfiguredProfile_InvalidOutput(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code")
	_, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{tAgentClaude}, Output: "slack"},
		&settings.EntireSettings{},
		deps,
	)
	if err == nil {
		t.Fatal("expected error for invalid --set-output value")
	}
}

func TestBuildConfiguredProfile_RejectsUnknownJudge(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps("claude-code")
	_, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{tAgentClaude}, Judge: "definitely-not-an-agent"},
		&settings.EntireSettings{},
		deps,
	)
	if err == nil {
		t.Fatal("expected error for --set-judge naming an agent that cannot write a verdict")
	}
}

func TestBuildConfiguredProfile_InvalidModelSpec(t *testing.T) {
	t.Parallel()
	deps := configureTestDeps(tAgentClaude)
	for _, spec := range []string{"no-equals", "=opus", tAgentClaude + "="} {
		t.Run(spec, func(t *testing.T) {
			t.Parallel()
			_, err := buildConfiguredProfile(
				context.Background(),
				DefaultProfileName,
				reviewConfigureOptions{Agents: []string{tAgentClaude}, Models: []string{spec}},
				&settings.EntireSettings{},
				deps,
			)
			if err == nil {
				t.Fatal("expected error for malformed --set-model spec")
			}
		})
	}
}

func TestSaveReviewProfile_ScopeProjectVsLocal(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	ctx := context.Background()

	projProfile := settings.ReviewProfileConfig{
		Task:   "Project task.",
		Agents: map[string]settings.ReviewConfig{tAgentClaude: {Skills: []string{"/review"}}},
	}
	if err := saveReviewProfile(ctx, "general", projProfile, true, reviewScopeProject); err != nil {
		t.Fatalf("save project: %v", err)
	}
	localProfile := settings.ReviewProfileConfig{
		Task:   "Local task.",
		Agents: map[string]settings.ReviewConfig{tAgentCodex: {Skills: []string{"/review"}}},
	}
	if err := saveReviewProfile(ctx, "scratch", localProfile, false, reviewScopeLocal); err != nil {
		t.Fatalf("save local: %v", err)
	}

	// Project file has only the project profile.
	_, projRaw, projExists, err := settings.LoadProjectRaw(ctx)
	if err != nil || !projExists {
		t.Fatalf("project raw: exists=%v err=%v", projExists, err)
	}
	if _, ok := projRaw["review_profiles"]; !ok {
		t.Fatal("project settings missing review_profiles")
	}

	// Both files merge through settings.Load.
	s, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s.ReviewProfiles["general"]; !ok {
		t.Errorf("merged settings missing project profile 'general': %#v", s.ReviewProfiles)
	}
	if _, ok := s.ReviewProfiles["scratch"]; !ok {
		t.Errorf("merged settings missing local profile 'scratch': %#v", s.ReviewProfiles)
	}
	// The local-only profile must not be written to the shared project file.
	projOnly, err := decodeRawReviewProfiles(projRaw)
	if err != nil {
		t.Fatalf("decode project review_profiles: %v", err)
	}
	if _, ok := projOnly["scratch"]; ok {
		t.Error("local profile 'scratch' leaked into the shared project settings file")
	}
	if _, ok := projOnly["general"]; !ok {
		t.Error("project profile 'general' missing from project settings file")
	}
}

func TestRunReviewConfigureScriptedPreservesProjectDefault(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	ctx := context.Background()

	general := settings.ReviewProfileConfig{
		Task:   "General task.",
		Agents: map[string]settings.ReviewConfig{tAgentClaude: {Agent: tAgentClaude}},
	}
	if err := saveReviewProfile(ctx, DefaultProfileName, general, true, reviewScopeProject); err != nil {
		t.Fatalf("seed general profile: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	deps := configureTestDeps(tAgentClaude, tAgentCodex)
	if err := runReviewConfigure(ctx, cmd, "security", reviewConfigureOptions{Agents: []string{tAgentCodex}}, deps); err != nil {
		t.Fatalf("runReviewConfigure: %v", err)
	}

	s, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ReviewDefaultProfile != DefaultProfileName {
		t.Fatalf("default profile = %q, want %s", s.ReviewDefaultProfile, DefaultProfileName)
	}
	if _, ok := s.ReviewProfiles["security"]; !ok {
		t.Fatalf("security profile was not saved: %#v", s.ReviewProfiles)
	}
}

func TestRunReviewConfigureScriptedLocalPreservesProjectDefault(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	ctx := context.Background()

	general := settings.ReviewProfileConfig{
		Task:   "General task.",
		Agents: map[string]settings.ReviewConfig{tAgentClaude: {Agent: tAgentClaude}},
	}
	if err := saveReviewProfile(ctx, DefaultProfileName, general, true, reviewScopeProject); err != nil {
		t.Fatalf("seed general profile: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	deps := configureTestDeps(tAgentClaude, tAgentCodex)
	if err := runReviewConfigure(ctx, cmd, "security", reviewConfigureOptions{Agents: []string{tAgentCodex}, Local: true}, deps); err != nil {
		t.Fatalf("runReviewConfigure: %v", err)
	}

	s, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ReviewDefaultProfile != DefaultProfileName {
		t.Fatalf("effective default profile = %q, want %s", s.ReviewDefaultProfile, DefaultProfileName)
	}
	_, localRaw, localExists, err := settings.LoadLocalRaw(ctx)
	if err != nil || !localExists {
		t.Fatalf("local raw: exists=%v err=%v", localExists, err)
	}
	if got := decodeRawReviewDefault(localRaw); got != "" {
		t.Fatalf("local review_default_profile = %q, want empty so project default remains effective", got)
	}
}

func TestSaveReviewProfileFirstProfileSetsDefault(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	ctx := context.Background()

	profile := settings.ReviewProfileConfig{
		Task:   "First task.",
		Agents: map[string]settings.ReviewConfig{tAgentClaude: {Agent: tAgentClaude}},
	}
	if err := saveReviewProfile(ctx, DefaultProfileName, profile, false, reviewScopeProject); err != nil {
		t.Fatalf("save first profile: %v", err)
	}
	s, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ReviewDefaultProfile != DefaultProfileName {
		t.Fatalf("default profile = %q, want %s", s.ReviewDefaultProfile, DefaultProfileName)
	}
}

func TestSaveReviewProfileConfigPreservesImpliedProjectDefault(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	ctx := context.Background()
	testutil.WriteFile(t, tmp, settings.EntireSettingsFile, `{
  "review_profiles": {
    "general": {"agents": {"claude-code": {"skills": ["/review"]}}},
    "security": {"agents": {"claude-code": {"skills": ["/old"]}}}
  }
}
`)

	if err := saveReviewProfileConfig(ctx, "security", map[string]settings.ReviewConfig{
		tAgentCodex: {Skills: []string{"/review"}},
	}, "", reviewScopeProject); err != nil {
		t.Fatalf("saveReviewProfileConfig: %v", err)
	}
	_, raw, exists, err := settings.LoadProjectRaw(ctx)
	if err != nil || !exists {
		t.Fatalf("project raw: exists=%v err=%v", exists, err)
	}
	if got := decodeRawReviewDefault(raw); got != "" {
		t.Fatalf("review_default_profile = %q, want empty so general remains implied default", got)
	}
	s, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	name, profile, err := selectReviewProfile(s, "")
	if err != nil {
		t.Fatalf("selectReviewProfile: %v", err)
	}
	if name != DefaultProfileName {
		t.Fatalf("selected profile = %q, want %s", name, DefaultProfileName)
	}
	if _, ok := profile.Agents[tAgentClaude]; !ok {
		t.Fatalf("general profile not selected: %#v", profile.Agents)
	}
}

func TestSaveReviewProfileConfigLocalPreservesProjectDefault(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	ctx := context.Background()

	general := settings.ReviewProfileConfig{
		Task:   "General task.",
		Agents: map[string]settings.ReviewConfig{tAgentClaude: {Agent: tAgentClaude}},
	}
	if err := saveReviewProfile(ctx, DefaultProfileName, general, true, reviewScopeProject); err != nil {
		t.Fatalf("seed project profile: %v", err)
	}
	if err := saveReviewProfileConfig(ctx, "security", map[string]settings.ReviewConfig{
		tAgentCodex: {Skills: []string{"/review"}},
	}, "", reviewScopeLocal); err != nil {
		t.Fatalf("saveReviewProfileConfig local: %v", err)
	}

	_, localRaw, localExists, err := settings.LoadLocalRaw(ctx)
	if err != nil || !localExists {
		t.Fatalf("local raw: exists=%v err=%v", localExists, err)
	}
	if got := decodeRawReviewDefault(localRaw); got != "" {
		t.Fatalf("local review_default_profile = %q, want empty so project default remains effective", got)
	}
	s, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ReviewDefaultProfile != DefaultProfileName {
		t.Fatalf("effective default profile = %q, want %s", s.ReviewDefaultProfile, DefaultProfileName)
	}
	if _, ok := s.ReviewProfiles["security"]; !ok {
		t.Fatalf("security profile was not saved locally: %#v", s.ReviewProfiles)
	}
}

func TestProfileJudge_ResolvesWorkerAlias(t *testing.T) {
	t.Parallel()
	// Judge names a worker alias; it must resolve to the underlying agent the
	// synthesis provider can launch, inheriting the worker's model.
	aliased := settings.ReviewProfileConfig{
		Agents: map[string]settings.ReviewConfig{
			"claude-opus": {Agent: tAgentClaude, Model: tModelOpus},
		},
		Judge: &settings.ReviewConfig{Agent: "claude-opus"},
	}
	if j, ok := profileJudge(aliased); !ok || j.agent != tAgentClaude || j.model != tModelOpus {
		t.Errorf("aliased judge = (%#v, %v), want claude-code/opus, true", j, ok)
	}

	// A standalone judge (not a worker id) is used as-is.
	standalone := settings.ReviewProfileConfig{
		Agents: map[string]settings.ReviewConfig{tAgentCodex: {Agent: tAgentCodex}},
		Judge:  &settings.ReviewConfig{Agent: tAgentClaude, Model: tModelOpus},
	}
	if j, ok := profileJudge(standalone); !ok || j.agent != tAgentClaude || j.model != tModelOpus {
		t.Errorf("standalone judge = (%#v, %v), want claude-code/opus, true", j, ok)
	}
}

func TestReviewWorkerLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		worker string
		cfg    settings.ReviewConfig
		want   string
	}{
		{"plain", tAgentClaude, settings.ReviewConfig{}, "claude-code"},
		{"model only", tAgentClaude, settings.ReviewConfig{Model: "opus"}, "claude-code  (model opus)"},
		{"alias only", "claude-opus", settings.ReviewConfig{Agent: tAgentClaude}, "claude-opus  (claude-code)"},
		{"alias and model", "claude-opus", settings.ReviewConfig{Agent: tAgentClaude, Model: "opus"}, "claude-opus  (claude-code, model opus)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := reviewWorkerLabel(c.worker, c.cfg); got != c.want {
				t.Errorf("reviewWorkerLabel(%q, %+v) = %q, want %q", c.worker, c.cfg, got, c.want)
			}
		})
	}
}

func TestDefaultReviewTasksRejectSlop(t *testing.T) {
	t.Parallel()
	for _, name := range []string{DefaultProfileName, "security", "accessibility"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			task := profileTask(name, settings.ReviewProfileConfig{})
			for _, want := range []string{"concrete", "code pointer", "No praise", "summaries", "speculation"} {
				if !strings.Contains(task, want) {
					t.Fatalf("task for %s missing %q:\n%s", name, want, task)
				}
			}
		})
	}
}

func TestReviewConfigureOptionsScripted_LocalOnlyDoesNotSkipInteractive(t *testing.T) {
	t.Parallel()
	if (reviewConfigureOptions{Local: true}).scripted() {
		t.Fatal("--local alone must not force scripted configure; it preselects local in the interactive scope picker")
	}
	if !(reviewConfigureOptions{Local: true, Agents: []string{tAgentClaude}}).scripted() {
		t.Fatal("--local with --set-* flags should still use scripted configure")
	}
}
