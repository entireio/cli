// Package review — see env.go for package-level rationale.
//
// picker.go implements the interactive review skills picker and agent selection
// helpers. pickConfig presents a huh multi-select per installed agent and saves
// the selection to clone-local review preferences.
package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// AgentChoice is one row in the spawn-time picker. Name is the agent
// registry key (used for marker/override); Label is the picker-visible
// string ("<name>   (N skills configured)" or "<name>   (prompt-only)").
type AgentChoice struct {
	Name  string
	Label string
}

// newAccessibleForm creates a huh form with Entire's standard theme,
// switching to accessibility mode when ACCESSIBLE is set. Thin wrapper
// around uiform.New preserved so existing call sites don't change.
func newAccessibleForm(groups ...*huh.Group) *huh.Form {
	return uiform.New(groups...)
}

// MergePickerResults combines the picker's output with existing review
// config entries that the picker did not surface. Agents in `offered` are
// fully controlled by the picker: if they appear in `selected` with a
// non-zero config the entry is set, otherwise the entry is removed.
// Agents not in `offered` keep their existing config untouched.
//
// Exported so tests can drive it directly — the picker itself
// can't run headless.
func MergePickerResults(existing map[string]settings.ReviewConfig, offered map[string]struct{}, selected map[string]settings.ReviewConfig) map[string]settings.ReviewConfig {
	merged := make(map[string]settings.ReviewConfig, len(existing)+len(selected))
	for name, cfg := range existing {
		if _, wasOffered := offered[name]; !wasOffered {
			merged[name] = cfg
		}
	}
	for name, cfg := range selected {
		merged[name] = cfg
	}
	return merged
}

// SaveReviewConfig persists the review map into clone-local preferences while
// preserving other review preferences. A load error means the preferences file
// exists but is malformed — we must NOT silently overwrite it with an empty
// struct, or every unrelated review preference would be wiped. Return the
// error so the caller can surface it instead.
func SaveReviewConfig(ctx context.Context, review map[string]settings.ReviewConfig) error {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return fmt.Errorf("load review preferences before save: %w", err)
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}
	prefs.Review = review
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		return fmt.Errorf("save review preferences: %w", err)
	}
	return nil
}

func SaveReviewFixAgent(ctx context.Context, agentName string) error {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return fmt.Errorf("load review preferences before save: %w", err)
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}
	prefs.ReviewFixAgent = agentName
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		return fmt.Errorf("save review preferences: %w", err)
	}
	return nil
}

// ComputeEligibleConfigured returns the sorted list of agents that are both
// configured (non-zero ReviewConfig entry) AND have hooks installed. Only
// eligible agents are valid picker targets — spawning a review for an agent
// without hooks would silently drop the review metadata.
func ComputeEligibleConfigured(s *settings.EntireSettings, installed []types.AgentName) []AgentChoice {
	if s == nil {
		return nil
	}
	installedSet := make(map[types.AgentName]struct{}, len(installed))
	for _, name := range installed {
		installedSet[name] = struct{}{}
	}
	out := make([]AgentChoice, 0, len(s.Review))
	for name, cfg := range s.Review {
		if cfg.IsZero() {
			continue
		}
		if _, ok := installedSet[types.AgentName(name)]; !ok {
			continue
		}
		out = append(out, AgentChoice{Name: name, Label: labelForAgentChoice(name, cfg)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// labelForAgentChoice builds the picker-visible label for an agent row.
func labelForAgentChoice(name string, cfg settings.ReviewConfig) string {
	switch {
	case len(cfg.Skills) > 0:
		return fmt.Sprintf("%s   (%d skills configured)", name, len(cfg.Skills))
	case cfg.Prompt != "":
		return name + "   (prompt-only)"
	default:
		return name
	}
}

// SelectReviewAgent picks an agent from the configured review map.
//
// If override is non-empty, returns the config for that agent or an error
// listing the configured alternatives. Otherwise returns the alphabetically
// first configured agent — deterministic but user-overridable via --agent.
func SelectReviewAgent(review map[string]settings.ReviewConfig, override string) (string, settings.ReviewConfig, error) {
	if len(review) == 0 {
		return "", settings.ReviewConfig{}, errors.New("no review config found")
	}
	var names []string
	for name, cfg := range review {
		if !cfg.IsZero() {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", settings.ReviewConfig{}, errors.New("no review config found")
	}
	sort.Strings(names)
	if override != "" {
		if cfg, ok := review[override]; ok && !cfg.IsZero() {
			return override, cfg, nil
		}
		return "", settings.ReviewConfig{}, fmt.Errorf(
			"agent %q is not configured for review; configured agents: %s",
			override, strings.Join(names, ", "),
		)
	}
	pick := names[0]
	return pick, review[pick], nil
}

// VerifyConfiguredSkillsInstalled is the spawn-time backstop for the
// silent-failure vector. For each skill in cfg.Skills, check it's either a
// curated built-in or returned by the agent's SkillDiscoverer; fail with a
// user-facing error if any skill is missing. Empty Skills (prompt-only
// config) short-circuits to nil — a freeform prompt has no skill list to
// validate against.
func VerifyConfiguredSkillsInstalled(ctx context.Context, ag agent.Agent, cfg settings.ReviewConfig) error {
	if len(cfg.Skills) == 0 {
		return nil
	}
	builtins := builtinNameSet(skilldiscovery.CuratedBuiltinsFor(string(ag.Name())))
	discoveredNames := map[string]struct{}{}
	if d, ok := ag.(agent.SkillDiscoverer); ok {
		if skills, err := d.DiscoverReviewSkills(ctx); err == nil {
			for _, s := range skills {
				discoveredNames[s.Name] = struct{}{}
			}
		} else {
			logging.Debug(ctx, "skill verification discovery failed",
				slog.String("agent", string(ag.Name())), slog.String("error", err.Error()))
		}
	}
	var missing []string
	for _, s := range cfg.Skills {
		if _, ok := builtins[s]; ok {
			continue
		}
		if _, ok := discoveredNames[s]; ok {
			continue
		}
		missing = append(missing, s)
	}
	if len(missing) == 0 {
		return nil
	}
	// Codex resolves skills by loose description match against the catalog it
	// injects into every session — not exact slash commands — and legacy saved
	// configs may still carry pre-$form invocations like "/review". On-disk
	// presence is therefore not authoritative for codex: log a hint but don't
	// block the spawn, since the named skill still loose-matches at runtime.
	if string(ag.Name()) == string(agent.AgentNameCodex) {
		logging.Debug(ctx, "codex review skill(s) not found on disk; relying on codex loose match",
			slog.String("skills", strings.Join(missing, ", ")))
		return nil
	}
	return fmt.Errorf(
		"configured review skill(s) not installed: %s\n"+
			"run `entire review --edit` to reconfigure, or install the plugin and retry",
		strings.Join(missing, ", "),
	)
}

// BuildReviewPickerFields composes the per-agent group fields for the
// review picker. Returns a slice of huh.Field in render order:
//
//	0: built-in commands (multiselect) OR note
//	1: installed plugin skills (multiselect) OR note
//	2: install hints (note with all active hint messages) — OMITTED if empty
//	3: additional instructions (text) — always present
func BuildReviewPickerFields(
	agentName string,
	builtins []skilldiscovery.CuratedSkill,
	discovered []agent.DiscoveredSkill,
	activeHints []skilldiscovery.InstallHint,
	previousPrompt string,
	builtinPicksOut, discoveredPicksOut *[]string,
	promptOut *string,
) []huh.Field {
	var fields []huh.Field

	if builtinPicksOut != nil && len(*builtinPicksOut) == 0 &&
		len(builtins) == 1 && strings.TrimSpace(previousPrompt) == "" {
		*builtinPicksOut = []string{builtins[0].Name}
	}

	builtinPreselected := preselectedSet(builtinPicksOut)
	discoveredPreselected := preselectedSet(discoveredPicksOut)

	if len(builtins) > 0 {
		opts := make([]huh.Option[string], 0, len(builtins))
		for _, b := range builtins {
			opt := huh.NewOption(b.Name, b.Name)
			if _, ok := builtinPreselected[b.Name]; ok {
				opt = opt.Selected(true)
			}
			opts = append(opts, opt)
		}
		ms := huh.NewMultiSelect[string]().
			Title("Built-in commands").
			Options(opts...).
			Height(len(opts) + 1)
		if builtinPicksOut != nil {
			ms = ms.Value(builtinPicksOut)
		}
		fields = append(fields, ms)
	} else {
		fields = append(fields, huh.NewNote().
			Title("Built-in commands").
			Description(fmt.Sprintf("No built-in review commands in %s.", agentName)))
	}

	if len(discovered) > 0 {
		opts := make([]huh.Option[string], 0, len(discovered))
		for _, d := range discovered {
			opt := huh.NewOption(d.Name, d.Name)
			if _, ok := discoveredPreselected[d.Name]; ok {
				opt = opt.Selected(true)
			}
			opts = append(opts, opt)
		}
		ms := huh.NewMultiSelect[string]().
			Title("Installed plugin skills").
			Options(opts...).
			Height(len(opts) + 1)
		if discoveredPicksOut != nil {
			ms = ms.Value(discoveredPicksOut)
		}
		fields = append(fields, ms)
	} else {
		fields = append(fields, huh.NewNote().
			Title("Installed plugin skills").
			Description("No plugin review skills detected on disk."))
	}

	if len(activeHints) > 0 {
		var sb strings.Builder
		for i, h := range activeHints {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("• ")
			sb.WriteString(h.Message)
		}
		fields = append(fields, huh.NewNote().
			Title("Install more").
			Description(sb.String()))
	}

	text := huh.NewText().
		Title("Additional instructions (optional)").
		Description("Added after selected skills. If no skills are selected, this becomes the full review prompt.")
	if promptOut != nil {
		*promptOut = previousPrompt
		text = text.Value(promptOut)
	}
	fields = append(fields, text)

	return fields
}

// SplitSavedPicks partitions a flat saved-skills list into the subset that
// matches built-in curated commands and the subset that matches discovered
// plugin skills. Skill names that match neither are dropped from both — they're
// preserved on the settings side via MergePickerResults when they belong to a
// picker-unaware agent entry.
func SplitSavedPicks(saved []string, builtins []skilldiscovery.CuratedSkill, discovered []agent.DiscoveredSkill) ([]string, []string) {
	builtinNames := make(map[string]struct{}, len(builtins))
	for _, b := range builtins {
		builtinNames[b.Name] = struct{}{}
	}
	discoveredNames := make(map[string]struct{}, len(discovered))
	for _, d := range discovered {
		discoveredNames[d.Name] = struct{}{}
	}
	var builtinPicks, discoveredPicks []string
	for _, s := range saved {
		if _, ok := builtinNames[s]; ok {
			builtinPicks = append(builtinPicks, s)
			continue
		}
		if _, ok := discoveredNames[s]; ok {
			discoveredPicks = append(discoveredPicks, s)
		}
	}
	return builtinPicks, discoveredPicks
}

// preselectedSet turns a slice pointer's current contents into a lookup
// set for the picker's "previously-saved" pre-selection.
func preselectedSet(slice *[]string) map[string]struct{} {
	if slice == nil || len(*slice) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(*slice))
	for _, s := range *slice {
		out[s] = struct{}{}
	}
	return out
}

func builtinNameSet(curated []skilldiscovery.CuratedSkill) map[string]struct{} {
	set := make(map[string]struct{}, len(curated))
	for _, c := range curated {
		set[c.Name] = struct{}{}
	}
	return set
}

// filterOutBuiltinCollisions drops any discovered skill whose name collides
// with a curated built-in. Built-in wins because it carries a richer,
// hand-authored description.
func filterOutBuiltinCollisions(discovered []agent.DiscoveredSkill, builtins map[string]struct{}) []agent.DiscoveredSkill {
	if len(discovered) == 0 || len(builtins) == 0 {
		return discovered
	}
	out := make([]agent.DiscoveredSkill, 0, len(discovered))
	for _, d := range discovered {
		if _, clash := builtins[d.Name]; clash {
			continue
		}
		out = append(out, d)
	}
	return out
}

func dedupeStrings(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
