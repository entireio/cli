// Package review — see env.go for package-level rationale.
//
// fanout.go implements skill fan-out: a worker configured with N skills is
// exploded into N single-skill workers before planning, so the skills run
// concurrently as ordinary worker slots. Previously all N skills were joined
// into one child's prompt and executed sequentially (or blended) — selecting
// more skills made the user wait for the SUM of their durations; after
// explosion the wait is the slowest skill.
package review

import (
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// explodeSkillWorkers returns a copy of profile whose multi-skill workers are
// split into one worker per skill. Single-skill and skill-less workers pass
// through unchanged under their original keys. Exploded workers keep the
// source worker's model and prompt, carry an explicit Agent so the derived
// key still resolves to the real agent, and get deterministic keys
// (<worker>:<skill-slug>, deduped against existing keys).
func explodeSkillWorkers(profile settings.ReviewProfileConfig, hasAdapter func(agentName string) bool) settings.ReviewProfileConfig {
	out := profile
	agents := make(map[string]settings.ReviewConfig, len(profile.Agents))

	// Pass-through workers claim their keys first so exploded keys can never
	// clobber an existing worker that happens to match a derived name.
	// Workers whose agent has no review-runner adapter also pass through:
	// exploding them forces the multi-agent branch, which hard-fails on
	// adapter-less agents, while unexploded they keep the working
	// single-agent RunMarkerFallback path.
	multiSkill := make([]string, 0, len(profile.Agents))
	for _, name := range sortedMapKeys(profile.Agents) {
		cfg := profile.Agents[name]
		if len(cfg.Skills) <= 1 || !hasAdapter(reviewAgentName(name, cfg)) {
			agents[name] = cfg
			continue
		}
		multiSkill = append(multiSkill, name)
	}

	for _, name := range multiSkill {
		cfg := profile.Agents[name]
		agentName := reviewAgentName(name, cfg)
		for _, skill := range cfg.Skills {
			worker := cfg
			worker.Skills = []string{skill}
			worker.Agent = agentName
			agents[workerIDForSkill(name, skill, agents)] = worker
		}
	}

	out.Agents = agents
	return out
}

// workerIDForSkill derives a stable worker key for one exploded skill run,
// following the workerIDForAgentModel convention (<base>:<slug>, numeric
// suffix on collision).
func workerIDForSkill(base, skill string, existing map[string]settings.ReviewConfig) string {
	candidate := base + ":" + sanitizeWorkerIDPart(skill)
	for i := 2; ; i++ {
		if _, exists := existing[candidate]; !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s:%s-%d", base, sanitizeWorkerIDPart(skill), i)
	}
}

// applyAgentOverride narrows profile.Agents to the workers matching the
// --agent selector, applying an optional model override to each. Exactly one
// match returns (workerName, cfg, true) for the single-agent path; multiple
// matches — the agent's exploded skill workers — narrow the profile in place
// and run as a filtered crew through the normal fan-out flow.
func applyAgentOverride(profile *settings.ReviewProfileConfig, agentOverride, modelOverride string) (string, settings.ReviewConfig, bool, error) {
	matched, err := selectProfileWorkers(*profile, agentOverride)
	if err != nil {
		return "", settings.ReviewConfig{}, false, err
	}
	if modelOverride != "" {
		for workerName, cfg := range matched {
			cfg.Model = modelOverride
			matched[workerName] = cfg
		}
	}
	if len(matched) == 1 {
		for workerName, cfg := range matched {
			return workerName, cfg, true, nil
		}
	}
	profile.Agents = matched
	return "", settings.ReviewConfig{}, false, nil
}
