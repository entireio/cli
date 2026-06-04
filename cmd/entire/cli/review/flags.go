// Package review — see env.go for package-level rationale.
//
// flags.go implements the `--reviewers` / `--fixer` one-off override
// resolution used by `entire review` and `entire review fix`. These
// flags are how a calling agent translates user intent ("you and Claude
// review, codex fixes") into a concrete CLI invocation without editing
// settings.
package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// resolveRolesFromFlags builds an EntireSettings.Review map from the
// --reviewers and --fixer flags. Returns (nil, nil) if no flags are
// set (caller should fall through to saved config / fallback).
//
// Validates that every named agent is installed; returns a clear
// error otherwise.
//
// An agent appearing in BOTH lists gets Role Both. Agents in only
// --reviewers get Role Reviewer. The --fixer agent (if not also a
// reviewer) gets Role Fixer.
//
//nolint:nilnil // (nil, nil) is the documented "no flags set" sentinel; caller checks override != nil.
func resolveRolesFromFlags(reviewers []string, fixer string, installed []types.AgentName) (map[string]settings.ReviewConfig, error) {
	if len(reviewers) == 0 && fixer == "" {
		return nil, nil
	}
	installedSet := make(map[string]struct{}, len(installed))
	for _, a := range installed {
		installedSet[string(a)] = struct{}{}
	}

	out := make(map[string]settings.ReviewConfig)
	reviewerSet := make(map[string]struct{}, len(reviewers))
	for _, name := range reviewers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := installedSet[name]; !ok {
			return nil, fmt.Errorf("--reviewers: agent not installed: %s", name)
		}
		reviewerSet[name] = struct{}{}
		out[name] = settings.ReviewConfig{Role: settings.RoleReviewer}
	}
	fixer = strings.TrimSpace(fixer)
	if fixer != "" {
		if _, ok := installedSet[fixer]; !ok {
			return nil, fmt.Errorf("--fixer: agent not installed: %s", fixer)
		}
		if _, alsoReviews := reviewerSet[fixer]; alsoReviews {
			out[fixer] = settings.ReviewConfig{Role: settings.RoleBoth}
		} else {
			out[fixer] = settings.ReviewConfig{Role: settings.RoleFixer}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("--reviewers / --fixer: at least one agent required")
	}
	return out, nil
}

// mergeFlagOverrideWithSavedSkills layers per-agent Skills/Prompt from
// `saved` onto the role-only entries in `override`. Agents named in the
// flags but not present in saved config fall back to seedDefaultSkills
// (curated built-ins, or name-matched on-disk discovery for agents like
// codex that ship no curated built-ins) so the run still has a review
// skill to invoke rather than a generic scope-only prompt.
//
// Note: the resulting map only contains agents named in the flags; it
// is a one-off override and never mixes in agents that were not
// explicitly requested.
func mergeFlagOverrideWithSavedSkills(ctx context.Context, override, saved map[string]settings.ReviewConfig) map[string]settings.ReviewConfig {
	out := make(map[string]settings.ReviewConfig, len(override))
	for name, cfg := range override {
		merged := cfg
		if prev, ok := saved[name]; ok {
			if len(prev.Skills) > 0 {
				merged.Skills = append([]string(nil), prev.Skills...)
			}
			if prev.Prompt != "" {
				merged.Prompt = prev.Prompt
			}
		}
		if len(merged.Skills) == 0 && merged.Prompt == "" {
			merged.Skills = seedDefaultSkills(ctx, name)
		}
		out[name] = merged
	}
	return out
}
