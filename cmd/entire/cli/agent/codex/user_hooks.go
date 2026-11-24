package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// readUserHooksFile reads the user-level hooks.json with the same size bound
// as the project file. A missing file is reported as !exists.
func readUserHooksFile(file agent.UserConfigFile) ([]byte, bool, error) {
	data, err := file.Read()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", file.Path, err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, false, fmt.Errorf("codex hooks file %q exceeds %d bytes", file.Path, maxHooksFileBytes)
	}
	return data, true, nil
}

func userHookCommand(ctx context.Context) func(managedHook) string {
	windows := agent.UseWindowsProductionHooks(ctx)
	prefix := userHookCommandPrefix()
	return func(h managedHook) string { return h.wrap(prefix+h.verb, windows) }
}

// EnsureUserHooks implements agent.UserHookInstaller. Codex runs these hooks
// only once the user approves them in /hooks.
func (c *CodexAgent) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
	path, err := userHooksPath()
	if err != nil {
		return 0, "", err
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		return 0, path, agent.ErrUserHooksNotInUse
	}
	file := agent.UserConfigFile{Path: path}
	commandFor := userHookCommand(ctx)
	n, err := agent.EnsureUserHookFile(ctx, path+".lock", wanted, func(write bool) (int, error) {
		data, exists, err := readUserHooksFile(file)
		if err != nil {
			return 0, err
		}
		output, count, err := mergeEntireHooks(data, exists, false, commandFor)
		if err != nil || count == 0 || !write {
			return count, err
		}
		return count, file.Write(output, 0o600) //nolint:wrapcheck // names the file
	})
	return n, path, err
}

// RemoveUserHooks implements agent.UserHookInstaller.
func (c *CodexAgent) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	path, err := userHooksPath()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		return false, path, nil
	}
	release, err := agent.LockUserHookFile(ctx, path+".lock")
	if err != nil {
		return false, path, err //nolint:wrapcheck // names the lock file
	}
	defer release()
	file := agent.UserConfigFile{Path: path}
	data, exists, err := readUserHooksFile(file)
	if err != nil || !exists {
		return false, path, err
	}
	output, changed, err := stripEntireHooks(data)
	if err != nil || !changed {
		return false, path, err
	}
	if err := file.Write(output, 0o600); err != nil {
		return false, path, err //nolint:wrapcheck // names the file
	}
	return true, path, nil
}

// InspectUserHooks implements agent.UserHookInstaller, including whether
// Codex has approval records for the user-level hooks.
func (c *CodexAgent) InspectUserHooks(ctx context.Context) agent.UserHookReport {
	path, err := userHooksPath()
	if err != nil {
		return agent.UserHookReport{State: agent.UserHooksUnavailable, Err: err}
	}
	report := agent.UserHookReport{Path: path}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		report.State = agent.UserHooksNotInUse
		return report
	}
	data, exists, err := readUserHooksFile(agent.UserConfigFile{Path: path})
	if err != nil {
		report.State, report.Err = agent.UserHooksUnavailable, err
		return report
	}
	document := &hooksDocument{rawHooks: map[string]json.RawMessage{}}
	if exists {
		document, err = decodeHooksDocument(path, data)
		if err != nil {
			report.State, report.Err = agent.UserHooksUnavailable, err
			return report
		}
	}
	installed := entireHookLabels(document)
	if len(installed) == 0 {
		report.State = agent.UserHooksAbsent
		return report
	}
	report.Version = userHooksVersionIn(document)
	_, count, err := mergeEntireHooks(data, exists, false, userHookCommand(ctx))
	switch {
	case err != nil:
		report.State, report.Err = agent.UserHooksUnavailable, err
	case count > 0:
		report.State = agent.UserHooksOutdated
	default:
		report.State = agent.UserHooksCurrent
	}
	trust := inspectHookTrustForDeclared(path, installed)
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
	var labels []string
	for _, h := range managedHooks {
		var groups []MatcherGroup
		if parseHookType(document.rawHooks, h.event, &groups) != nil {
			continue
		}
		if hasEntireHook(groups) {
			labels = append(labels, h.label)
		}
	}
	return labels
}

// userHooksVersionIn returns the version stamped in the document's Entire
// hook commands (0 when none carries one).
func userHooksVersionIn(document *hooksDocument) int {
	marker := "--scope=" + UserScopePrefix
	for _, h := range managedHooks {
		var groups []MatcherGroup
		if parseHookType(document.rawHooks, h.event, &groups) != nil {
			continue
		}
		for _, g := range groups {
			for _, hook := range g.Hooks {
				_, rest, ok := strings.Cut(hook.Command, marker)
				if !ok || !isEntireHook(hook.Command) {
					continue
				}
				digits, _, _ := strings.Cut(rest, " ")
				if v, err := strconv.Atoi(digits); err == nil {
					return v
				}
			}
		}
	}
	return 0
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
