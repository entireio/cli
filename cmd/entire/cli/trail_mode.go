package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// projectTrailsEnv opts into project-scoped trails. Read it only through
// projectTrailsEnabled, at the two places that choose a model: building the
// trail command tree and rendering the first-turn context injection.
const projectTrailsEnv = "ENTIRE_PROJECT_TRAILS"

// projectTrailsAnnotation marks a project-model trail tree so agent-help
// describes the tree that was actually built, not the current environment.
const projectTrailsAnnotation = "entire_project_trails"

func projectTrailsEnabled() bool {
	return os.Getenv(projectTrailsEnv) == "1"
}

// trailMode is the only seam between the repository-scoped (legacy) and the
// project-scoped trail models. Commands whose meaning differs between the two
// are separate trees: trail_*.go for legacy, project_trail_*.go for project.
// Commands that act on one repository branch in both models (checkout, resume,
// finding, watch, approve, request-changes, approvals) are shared and take a
// trailMode, which decides how a selector finds that branch and how help
// describes the selector. Retiring the legacy model means deleting
// legacyTrailMode, its tree, and the legacy argument of each help call.
type trailMode struct {
	project bool
	// workingContext resolves a selector and/or --branch to one repository
	// branch. localOnly callers (checkout, resume) act on the local clone.
	workingContext func(cmd *cobra.Command, selector, branch string, localOnly bool) (*trailWorkingContext, error)
	// reviewTarget resolves the finding and watch target. localOnly callers
	// (finding apply) patch this clone, so the target must be its repository.
	reviewTarget func(cmd *cobra.Command, selector string, localOnly bool) (*api.Client, trailReviewTarget, error)
}

var (
	legacyTrailMode = &trailMode{
		workingContext: resolveLegacyTrailContext,
		reviewTarget:   authenticatedLegacyTrailReviewTarget,
	}
	projectTrailMode = &trailMode{
		project:        true,
		workingContext: resolveProjectTrailWorkingContext,
		reviewTarget:   projectTrailReviewTarget,
	}
)

func trailModeFor(project bool) *trailMode {
	if project {
		return projectTrailMode
	}
	return legacyTrailMode
}

// help picks the help text for this mode's selector semantics.
func (m *trailMode) help(legacy, project string) string {
	if m.project {
		return project
	}
	return legacy
}

// usesProjectTrails reports the model of the tree cmd belongs to. Only
// agent-help needs it; commands receive their trailMode at construction.
func usesProjectTrails(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[projectTrailsAnnotation] == agentHelpAnnotationEnabled {
			return true
		}
	}
	return false
}
