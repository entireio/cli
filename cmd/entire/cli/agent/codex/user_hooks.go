package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UserHookInstaller = (*CodexAgent)(nil)

// userHooksVersion is the Entire hook version of the user-level hooks,
// stamped into their --scope value. Bump it when what the user-level install
// writes changes; Codex asks the user to approve changed hooks in /hooks
// either way.
const userHooksVersion = 1

// UserScopePrefix starts the --scope value of every user-level hook command:
// `entire hooks codex --scope=user/v<N> <verb>`. The hooks command reads it to
// step aside where a project install takes over.
const UserScopePrefix = "user/v"

// userHookCommandPrefix starts every hook command in the user-level
// hooks.json.
func userHookCommandPrefix() string {
	return "entire hooks codex --scope=" + UserScopePrefix + strconv.Itoa(userHooksVersion) + " "
}

// userHooksPath is $CODEX_HOME/hooks.json (default ~/.codex/hooks.json),
// which Codex loads in every project. Resolved on its own rather than through
// ResolveHookDiscovery, which describes the project file and refuses a hook
// root that is the user's.
func userHooksPath() (string, error) {
	home, err := resolveCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, HooksFileName), nil
}

func userHookCommand(ctx context.Context) func(managedHook) string {
	windows := agent.UseWindowsProductionHooks(ctx)
	prefix := userHookCommandPrefix()
	return func(h managedHook) string { return h.wrap(prefix+h.verb, windows) }
}

// userHooksFile is the user-level hooks.json, merged with the project
// install's merge under user-scoped commands.
func userHooksFile(ctx context.Context) (agent.MergedUserFile, error) {
	path, err := userHooksPath()
	if err != nil {
		return agent.MergedUserFile{}, err
	}
	commandFor := userHookCommand(ctx)
	return agent.MergedUserFile{
		Path:     path,
		MaxBytes: maxHooksFileBytes,
		Perm:     0o600,
		HasEntire: func(data []byte) bool {
			document, err := decodeHooksDocument(path, data)
			// A file that does not parse is left to the merge to report.
			return err != nil || len(entireHookLabels(document)) > 0
		},
		Merge: func(data []byte, exists bool) ([]byte, int, error) {
			return mergeEntireHooks(data, exists, false, commandFor)
		},
		Strip: stripEntireHooks,
	}, nil
}

// EnsureUserHooks implements agent.UserHookInstaller. Codex runs these hooks
// only once the user approves them in /hooks.
func (c *CodexAgent) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (agent.UserHookInstall, error) {
	file, err := userHooksFile(ctx)
	if err != nil {
		return agent.UserHookInstall{}, err
	}
	return file.Ensure(ctx, wanted) //nolint:wrapcheck // names the file or its lock
}

// RemoveUserHooks implements agent.UserHookInstaller.
func (c *CodexAgent) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	file, err := userHooksFile(ctx)
	if err != nil {
		return false, "", err
	}
	removed, err := file.Remove(ctx)
	return removed, file.Path, err //nolint:wrapcheck // names the file or its lock
}

// InspectUserHooks implements agent.UserHookInstaller, including whether
// Codex has approval records for the user-level hooks.
func (c *CodexAgent) InspectUserHooks(ctx context.Context) agent.UserHookReport {
	file, err := userHooksFile(ctx)
	if err != nil {
		return agent.UserHookReport{State: agent.UserHooksUnavailable, Err: err}
	}
	var installed []string
	file.Version = func(data []byte) int {
		document, err := decodeHooksDocument(file.Path, data)
		if err != nil {
			return 0
		}
		var version int
		installed, version = userHookLabelsAndVersion(document)
		return version
	}
	report := file.Inspect()
	if report.State != agent.UserHooksCurrent && report.State != agent.UserHooksOutdated {
		return report
	}
	trust := inspectHookTrustForDeclared(file.Path, installed)
	switch {
	case !trust.Known:
		report.Notes = append(report.Notes, "Codex runs these hooks only after you approve them: open /hooks in Codex.")
	case len(trust.Gaps) > 0:
		report.Notes = append(report.Notes, fmt.Sprintf("%d of these Codex hooks await approval (%s): open /hooks in Codex to approve them.",
			len(trust.Gaps), strings.Join(trust.Gaps, ", ")))
	}
	return report
}

// entireHookLabels lists the trust labels of the events where the document
// has an Entire hook.
func entireHookLabels(document *hooksDocument) []string {
	labels, _ := userHookLabelsAndVersion(document)
	return labels
}

// userHookLabelsAndVersion walks the document's Entire hooks once: the trust
// labels of the events that have one, and the version stamped in the first
// user-scoped command (0 when none carries one).
func userHookLabelsAndVersion(document *hooksDocument) ([]string, int) {
	marker := "--scope=" + UserScopePrefix
	var labels []string
	version := 0
	for _, h := range managedHooks {
		var groups []MatcherGroup
		if parseHookType(document.rawHooks, h.event, &groups) != nil {
			continue
		}
		if hasEntireHook(groups) {
			labels = append(labels, h.label)
		}
		for _, g := range groups {
			for _, hook := range g.Hooks {
				_, rest, ok := strings.Cut(hook.Command, marker)
				if version != 0 || !ok || !isEntireHook(hook.Command) {
					continue
				}
				digits, _, _ := strings.Cut(rest, " ")
				if v, err := strconv.Atoi(digits); err == nil {
					version = v
				}
			}
		}
	}
	return labels, version
}

// ProjectHooksSupersede implements agent.UserHookInstaller. Codex loads both
// the user-level and the project hooks.json, so the user-level hooks step
// aside where the hooks file Codex discovers for this checkout is Entire's,
// the project layer exists, and Codex has approval records for every Entire
// hook in it. Untrusted project hooks do not run, so then the user-level ones
// must. A discovered file that is the user-level one (a home directory that
// is itself a repository) never supersedes it.
func (c *CodexAgent) ProjectHooksSupersede(ctx context.Context) bool {
	discovery := ResolveHookDiscovery(ctx)
	if discovery.State != HookDiscoveryResolved || !discovery.ProjectLayerExists() {
		return false
	}
	projectPath := discovery.DiscoveredHooks.Path()
	if userPath, err := userHooksPath(); err == nil {
		canonicalUser, userErr := canonicalPath(userPath)
		canonicalProject, projectErr := canonicalPath(projectPath)
		if userErr == nil && projectErr == nil && canonicalUser == canonicalProject {
			return false
		}
	}
	inspection := inspectDiscoveredHookConfigLightweight(discovery.DiscoveredHooks)
	if inspection.State != HookFileEntire || !inspection.CoreInstalled {
		return false
	}
	document, err := readHooksDocument(projectPath)
	if err != nil {
		return false
	}
	entireLabels := entireHookLabels(document)
	trust := inspectHookTrustForDeclared(projectPath, entireLabels)
	return trust.Known && len(trust.Gaps) == 0 && len(entireLabels) > 0
}
