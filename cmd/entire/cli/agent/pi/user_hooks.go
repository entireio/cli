package pi

import (
	"context"
	"path/filepath"
	"regexp"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UserHookInstaller = (*PiAgent)(nil)

// userHooksVersion is the Entire hook version stamped into the user-level
// extension. Bump it when the user-level render changes.
const userHooksVersion = 1

var userVersionPattern = regexp.MustCompile(`Entire user-level extension, hook version (\d+)`)

// userExtension is $PI_CODING_AGENT_DIR/extensions/entire/index.ts (default
// ~/.pi/agent/...), which Pi loads in every project.
func userExtension() (agent.GeneratedUserFile, error) {
	home, err := resolvePiHome()
	if err != nil {
		return agent.GeneratedUserFile{}, err
	}
	return agent.GeneratedUserFile{
		Home:    home,
		Path:    filepath.Join(home, "extensions", "entire", extensionFileName),
		Marker:  entireMarker,
		Content: renderExtensionFor(true),
		Version: userVersionPattern,
		// Pi discovers extensions by directory; an empty one left behind is
		// a half-removed extension.
		RemoveDir: true,
	}, nil
}

// EnsureUserHooks implements agent.UserHookInstaller.
func (a *PiAgent) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
	file, err := userExtension()
	if err != nil {
		return 0, "", err
	}
	n, err := file.Ensure(ctx, wanted)
	return n, file.Path, err //nolint:wrapcheck // names the file
}

// RemoveUserHooks implements agent.UserHookInstaller.
func (a *PiAgent) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	file, err := userExtension()
	if err != nil {
		return false, "", err
	}
	removed, err := file.Remove(ctx)
	return removed, file.Path, err //nolint:wrapcheck // names the file
}

// InspectUserHooks implements agent.UserHookInstaller.
func (a *PiAgent) InspectUserHooks(context.Context) agent.UserHookReport {
	file, err := userExtension()
	if err != nil {
		return agent.UserHookReport{State: agent.UserHooksUnavailable, Err: err}
	}
	return file.Inspect()
}

// ProjectHooksSupersede implements agent.UserHookInstaller. Pi's user-level
// extension steps aside itself when it loads (see userLevelStepAside), so no
// hook command needs to ask.
func (a *PiAgent) ProjectHooksSupersede(context.Context) bool { return false }
