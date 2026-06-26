package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const DefaultProfileName = "general"

// Review output destinations. ReviewOutputLocal prints the verdict and writes
// the local review manifest; ReviewOutputTrail additionally posts the verdict
// to the branch's trail as a finding (`entire trail finding`).
const (
	ReviewOutputLocal = "local"
	ReviewOutputTrail = "trail"
)

// profileOutput resolves the configured output destination, defaulting to
// local. Unknown values fall back to local.
func profileOutput(profile settings.ReviewProfileConfig) string {
	if strings.EqualFold(strings.TrimSpace(profile.Output), ReviewOutputTrail) {
		return ReviewOutputTrail
	}
	return ReviewOutputLocal
}

// normalizeReviewOutput validates a user-supplied output value, returning the
// canonical form. Empty is allowed (means local).
func normalizeReviewOutput(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", ReviewOutputLocal:
		return ReviewOutputLocal, nil
	case ReviewOutputTrail:
		return ReviewOutputTrail, nil
	default:
		return "", fmt.Errorf("invalid output %q; valid values are %s, %s", raw, ReviewOutputLocal, ReviewOutputTrail)
	}
}

const (
	defaultGeneralTask       = "Review this change for correctness, regressions, API design, missing tests, maintainability, and user-facing behavior changes. Return only real, actionable defects with concrete evidence and an exact code pointer. No praise, summaries, speculation, style preferences, or nice-to-have refactors."
	defaultSecurityTask      = "Review this change for security vulnerabilities: authentication and authorization bugs, injection risks, secrets exposure, unsafe dependency or deserialization behavior, privilege-boundary mistakes, insecure defaults, and data leakage. Return only exploitable or clearly risky defects with concrete evidence and an exact code pointer. No praise, summaries, speculation, or hardening wishlists."
	defaultAccessibilityTask = "Review this change for accessibility regressions: keyboard navigation, focus management, semantic markup, labels, ARIA correctness, color contrast, reduced-motion behavior, screen-reader behavior, and inclusive error states. Return only concrete user-impacting defects with an exact code pointer. No praise, summaries, speculation, or generic best-practice advice."
)

// profileTask returns the configured task, or a built-in task for conventional
// profile names when the config leaves task empty.
func profileTask(name string, cfg settings.ReviewProfileConfig) string {
	if strings.TrimSpace(cfg.Task) != "" {
		return strings.TrimSpace(cfg.Task)
	}
	switch strings.ToLower(name) {
	case "", DefaultProfileName:
		return defaultGeneralTask
	case "security":
		return defaultSecurityTask
	case "accessibility", "a11y":
		return defaultAccessibilityTask
	default:
		return defaultGeneralTask
	}
}

// selectReviewProfile resolves the profile to run. When no review_profiles are
// configured, a legacy top-level review map is exposed as the general profile so
// upgrades keep honoring existing review setups until the user saves profiles.
func selectReviewProfile(s *settings.EntireSettings, override string) (string, settings.ReviewProfileConfig, error) {
	applyLegacyReviewProfileFallback(s)
	if s == nil || len(s.ReviewProfiles) == 0 {
		return "", settings.ReviewProfileConfig{}, errors.New("no review profiles configured; run `entire review --configure` or add review_profiles to Entire preferences")
	}
	profiles := nonZeroProfiles(s.ReviewProfiles)
	if len(profiles) == 0 {
		return "", settings.ReviewProfileConfig{}, errors.New("no review profiles configured; every profile is empty")
	}

	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(s.ReviewDefaultProfile)
	}
	if name == "" {
		if _, ok := profiles[DefaultProfileName]; ok {
			name = DefaultProfileName
		} else if len(profiles) == 1 {
			for only := range profiles {
				name = only
			}
		} else {
			return "", settings.ReviewProfileConfig{}, fmt.Errorf(
				"multiple review profiles configured (%s); pass a profile name or set review_default_profile",
				strings.Join(sortedMapKeys(profiles), ", "))
		}
	}

	cfg, ok := profiles[name]
	if !ok {
		return "", settings.ReviewProfileConfig{}, fmt.Errorf(
			"review profile %q is not configured; configured profiles: %s",
			name, strings.Join(sortedMapKeys(profiles), ", "))
	}
	if len(nonZeroAgentConfigs(cfg.Agents)) == 0 {
		return "", settings.ReviewProfileConfig{}, fmt.Errorf("review profile %q has no configured agents", name)
	}
	return name, cfg, nil
}

func applyLegacyReviewProfileFallback(s *settings.EntireSettings) {
	if s == nil || len(nonZeroProfiles(s.ReviewProfiles)) > 0 {
		return
	}
	legacyAgents := nonZeroAgentConfigs(s.Review) //nolint:staticcheck // intentional compatibility fallback for deprecated review config
	if len(legacyAgents) == 0 {
		return
	}
	s.ReviewProfiles = map[string]settings.ReviewProfileConfig{
		DefaultProfileName: {
			Agents: legacyAgents,
		},
	}
	if strings.TrimSpace(s.ReviewDefaultProfile) == "" {
		s.ReviewDefaultProfile = DefaultProfileName
	}
}

func nonZeroProfiles(in map[string]settings.ReviewProfileConfig) map[string]settings.ReviewProfileConfig {
	out := make(map[string]settings.ReviewProfileConfig, len(in))
	for name, cfg := range in {
		name = strings.TrimSpace(name)
		if name == "" || cfg.IsZero() {
			continue
		}
		out[name] = cfg
	}
	return out
}

func nonZeroAgentConfigs(in map[string]settings.ReviewConfig) map[string]settings.ReviewConfig {
	out := make(map[string]settings.ReviewConfig, len(in))
	for name, cfg := range in {
		name = strings.TrimSpace(name)
		if name == "" || cfg.IsZero() {
			continue
		}
		out[name] = cfg
	}
	return out
}

func reviewAgentName(workerName string, cfg settings.ReviewConfig) string {
	if strings.TrimSpace(cfg.Agent) != "" {
		return strings.TrimSpace(cfg.Agent)
	}
	return strings.TrimSpace(workerName)
}

func reviewWorkerLabel(workerName string, cfg settings.ReviewConfig) string {
	agentName := reviewAgentName(workerName, cfg)
	parts := []string{workerName}
	var details []string
	if agentName != "" && agentName != workerName {
		details = append(details, agentName)
	}
	if strings.TrimSpace(cfg.Model) != "" {
		details = append(details, "model "+strings.TrimSpace(cfg.Model))
	}
	if len(details) > 0 {
		parts = append(parts, "  ("+strings.Join(details, ", ")+")")
	}
	return strings.Join(parts, "")
}

// judgeSpec is the resolved consolidating judge: the agent that renders the
// final verdict plus its optional model.
type judgeSpec struct {
	agent string
	model string
}

// profileJudge resolves the configured consolidating judge. ok is false when
// the profile has no judge set (a single-reviewer profile, or one left to the
// runtime default); callers fall back to resolveJudge for the default pick.
func profileJudge(profile settings.ReviewProfileConfig) (judgeSpec, bool) {
	if profile.Judge == nil {
		return judgeSpec{}, false
	}
	name := strings.TrimSpace(profile.Judge.Agent)
	if name == "" {
		return judgeSpec{}, false
	}
	model := strings.TrimSpace(profile.Judge.Model)
	// If the judge names one of the profile's worker ids (possibly an alias such
	// as "claude-opus" for {agent: claude-code, model: opus}), resolve it to the
	// underlying agent the synthesis provider can actually launch, inheriting the
	// worker's model when the judge didn't specify one. Otherwise the judge is a
	// standalone agent name and is used as-is.
	if cfg, ok := profile.Agents[name]; ok && !cfg.IsZero() {
		if model == "" {
			model = strings.TrimSpace(cfg.Model)
		}
		name = reviewAgentName(name, cfg)
	}
	return judgeSpec{agent: name, model: model}, true
}

// resolveJudge returns the judge to use for a fan-out run: the explicitly
// configured judge, or an auto-selected text-gen reviewer when none is set.
func resolveJudge(ctx context.Context, profile settings.ReviewProfileConfig) (judgeSpec, bool) {
	if j, ok := profileJudge(profile); ok {
		return j, true
	}
	return defaultJudge(ctx, profile.Agents)
}

// judgeLabel renders a judge for UI output: "agent" or "agent · model".
func judgeLabel(j judgeSpec) string {
	if strings.TrimSpace(j.model) != "" {
		return labelForSimpleAgent(j.agent) + " · " + j.model
	}
	return labelForSimpleAgent(j.agent)
}

func selectProfileWorker(profile settings.ReviewProfileConfig, selector string) (string, settings.ReviewConfig, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", settings.ReviewConfig{}, errors.New("empty review reviewer selector")
	}
	if cfg, ok := profile.Agents[selector]; ok && !cfg.IsZero() {
		return selector, cfg, nil
	}
	var matches []string
	for workerName, cfg := range profile.Agents {
		if cfg.IsZero() {
			continue
		}
		if reviewAgentName(workerName, cfg) == selector {
			matches = append(matches, workerName)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], profile.Agents[matches[0]], nil
	case 0:
		configured := sortedMapKeys(profile.Agents)
		if len(configured) == 0 {
			return "", settings.ReviewConfig{}, fmt.Errorf("review reviewer or agent %q is not configured", selector)
		}
		return "", settings.ReviewConfig{}, fmt.Errorf("review reviewer or agent %q is not configured; configured reviewers: %s", selector, strings.Join(configured, ", "))
	default:
		return "", settings.ReviewConfig{}, fmt.Errorf("agent %q has multiple review reviewers (%s); choose one by reviewer name", selector, strings.Join(matches, ", "))
	}
}

func workerIDForAgentModel(agentName, model string, existing map[string]settings.ReviewConfig) string {
	base := strings.TrimSpace(agentName)
	if strings.TrimSpace(model) != "" {
		base += ":" + sanitizeWorkerIDPart(model)
	}
	if base == "" {
		base = "worker"
	}
	candidate := base
	for i := 2; ; i++ {
		if _, exists := existing[candidate]; !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func sanitizeWorkerIDPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		keep := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if keep {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "model"
	}
	return out
}

func defaultReviewProfileForInstalledAgents(
	ctx context.Context,
	profileName string,
	installed []types.AgentName,
	reviewerFor func(string) reviewtypes.AgentReviewer,
) (settings.ReviewProfileConfig, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		profileName = DefaultProfileName
	}
	installedNames := make([]string, 0, len(installed))
	for _, name := range installed {
		installedNames = append(installedNames, string(name))
	}
	sort.Strings(installedNames)

	agents := make(map[string]settings.ReviewConfig, len(installedNames))
	for _, name := range installedNames {
		if reviewerFor != nil && reviewerFor(name) == nil {
			continue
		}
		cfg := defaultReviewAgentConfig(profileName, name)
		if cfg.IsZero() {
			continue
		}
		agents[name] = cfg
	}
	if len(agents) == 0 {
		return settings.ReviewProfileConfig{}, errors.New("no agents with review runner adapters and hooks installed; run `entire configure --agent claude-code`, `entire configure --agent codex`, or `entire configure --agent gemini`")
	}
	profile := settings.ReviewProfileConfig{
		Task:   profileTask(profileName, settings.ReviewProfileConfig{}),
		Agents: agents,
	}
	if j, ok := defaultJudge(ctx, agents); ok {
		profile.Judge = &settings.ReviewConfig{Agent: j.agent, Model: j.model}
	}
	return profile, nil
}

func defaultReviewAgentConfig(profileName, agentName string) settings.ReviewConfig {
	focus := defaultProfileFocus(profileName)
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		if strings.EqualFold(profileName, "security") {
			return settings.ReviewConfig{Skills: []string{"/security-review"}}
		}
		return settings.ReviewConfig{Skills: []string{"/review"}, Prompt: focus}
	case string(agent.AgentNameCodex):
		return settings.ReviewConfig{Skills: []string{"/review"}, Prompt: focus}
	case string(agent.AgentNameGemini):
		prompt := "Review the change according to the profile task."
		if focus != "" {
			prompt += " " + focus
		}
		return settings.ReviewConfig{Prompt: prompt}
	default:
		return settings.ReviewConfig{}
	}
}

func defaultProfileFocus(profileName string) string {
	switch strings.ToLower(strings.TrimSpace(profileName)) {
	case "security":
		return "Focus specifically on security issues."
	case "accessibility", "a11y":
		return "Focus specifically on accessibility issues."
	default:
		return ""
	}
}

// defaultJudge auto-selects a consolidating judge from the configured
// reviewers: it prefers claude-code, then codex, then gemini, and otherwise
// takes the first reviewer that can write a verdict (text generation). ok is
// false when no reviewer can.
func defaultJudge(ctx context.Context, configured map[string]settings.ReviewConfig) (judgeSpec, bool) {
	for _, preferred := range []string{string(agent.AgentNameClaudeCode), string(agent.AgentNameCodex), string(agent.AgentNameGemini)} {
		for _, workerName := range sortedMapKeys(configured) {
			cfg := configured[workerName]
			if reviewAgentName(workerName, cfg) == preferred && agentSupportsTextGeneration(ctx, preferred) {
				return judgeSpec{agent: preferred, model: strings.TrimSpace(cfg.Model)}, true
			}
		}
	}
	for _, workerName := range sortedMapKeys(configured) {
		cfg := configured[workerName]
		if name := reviewAgentName(workerName, cfg); agentSupportsTextGeneration(ctx, name) {
			return judgeSpec{agent: name, model: strings.TrimSpace(cfg.Model)}, true
		}
	}
	return judgeSpec{}, false
}

func sortedMapKeys[V any](in map[string]V) []string {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func agentSupportsTextGeneration(_ context.Context, name string) bool {
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		return false
	}
	_, ok := agent.AsTextGenerator(ag)
	return ok
}

// reviewSettingsScope selects which settings file a review profile is written
// to. Both files are read and merged by settings.Load; the scope only decides
// where new profiles are persisted.
type reviewSettingsScope int

const (
	// reviewScopeProject writes to .entire/settings.json (shared, committed).
	reviewScopeProject reviewSettingsScope = iota
	// reviewScopeLocal writes to .entire/settings.local.json (per-developer).
	reviewScopeLocal
)

// file returns the settings filename this scope writes to.
func (s reviewSettingsScope) file() string {
	if s == reviewScopeLocal {
		return settings.EntireSettingsLocalFile
	}
	return settings.EntireSettingsFile
}

// saveReviewProfile persists one profile into the chosen settings file via a
// raw read-modify-write so unrelated keys (and other profiles) are preserved.
func saveReviewProfile(ctx context.Context, profileName string, profile settings.ReviewProfileConfig, makeDefault bool, scope reviewSettingsScope) error {
	path, raw, err := loadReviewSettingsRaw(ctx, scope)
	if err != nil {
		return err
	}
	profiles, err := decodeRawReviewProfiles(raw)
	if err != nil {
		return err
	}
	hadProfiles := len(profiles) > 0
	profiles[profileName] = profile
	defaultName := decodeRawReviewDefault(raw)
	switch {
	case makeDefault:
		defaultName = profileName
	case strings.TrimSpace(defaultName) == "" && !hadProfiles:
		hasLower, err := lowerReviewDefaultOrProfiles(ctx, scope)
		if err != nil {
			return err
		}
		if !hasLower {
			defaultName = profileName
		}
	}
	return writeRawReviewProfiles(path, raw, profiles, defaultName)
}

func lowerReviewDefaultOrProfiles(ctx context.Context, scope reviewSettingsScope) (bool, error) {
	if scope != reviewScopeLocal {
		return false, nil
	}
	_, raw, exists, err := settings.LoadProjectRaw(ctx)
	if err != nil {
		return false, fmt.Errorf("load project settings before local default check: %w", err)
	}
	if !exists || raw == nil {
		return false, nil
	}
	return rawHasReviewDefaultOrProfiles(raw)
}

func rawHasReviewDefaultOrProfiles(raw map[string]json.RawMessage) (bool, error) {
	if strings.TrimSpace(decodeRawReviewDefault(raw)) != "" {
		return true, nil
	}
	profiles, err := decodeRawReviewProfiles(raw)
	if err != nil {
		return false, err
	}
	return len(profiles) > 0, nil
}

// loadReviewSettingsRaw reads the raw JSON object for the chosen settings file.
func loadReviewSettingsRaw(ctx context.Context, scope reviewSettingsScope) (string, map[string]json.RawMessage, error) {
	var (
		path string
		raw  map[string]json.RawMessage
		err  error
	)
	if scope == reviewScopeLocal {
		path, raw, _, err = settings.LoadLocalRaw(ctx)
	} else {
		path, raw, _, err = settings.LoadProjectRaw(ctx)
	}
	if err != nil {
		return "", nil, fmt.Errorf("load %s before save: %w", scope.file(), err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	return path, raw, nil
}

func decodeRawReviewProfiles(raw map[string]json.RawMessage) (map[string]settings.ReviewProfileConfig, error) {
	profiles := map[string]settings.ReviewProfileConfig{}
	if msg, ok := raw["review_profiles"]; ok && len(msg) > 0 {
		if err := json.Unmarshal(msg, &profiles); err != nil {
			return nil, fmt.Errorf("parse existing review_profiles: %w", err)
		}
		if profiles == nil {
			profiles = map[string]settings.ReviewProfileConfig{}
		}
	}
	return profiles, nil
}

func decodeRawReviewDefault(raw map[string]json.RawMessage) string {
	if msg, ok := raw["review_default_profile"]; ok && len(msg) > 0 {
		var s string
		if err := json.Unmarshal(msg, &s); err == nil {
			return s
		}
	}
	return ""
}

func writeRawReviewProfiles(path string, raw map[string]json.RawMessage, profiles map[string]settings.ReviewProfileConfig, defaultName string) error {
	profilesJSON, err := json.Marshal(profiles)
	if err != nil {
		return fmt.Errorf("encode review_profiles: %w", err)
	}
	raw["review_profiles"] = profilesJSON
	if strings.TrimSpace(defaultName) != "" {
		defJSON, err := json.Marshal(defaultName)
		if err != nil {
			return fmt.Errorf("encode review_default_profile: %w", err)
		}
		raw["review_default_profile"] = defJSON
	}
	// SaveProjectRaw writes the given path atomically (temp file + rename in the
	// same dir) but does not create the directory, so ensure .entire/ exists
	// for repos that haven't been enabled yet.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create settings dir %s: %w", dir, err)
		}
	}
	// SaveProjectRaw is path-generic despite the name, so it also serves the
	// local settings file.
	if err := settings.SaveProjectRaw(path, raw); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
