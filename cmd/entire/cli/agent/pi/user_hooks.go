package pi

import (
	"path/filepath"
	"regexp"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UserHookInstaller = (*PiAgent)(nil)

// userHooksVersion is the Entire hook version stamped into the user-level
// extension. Bump it when the user-level render changes.
const userHooksVersion = 1

var userVersionPattern = regexp.MustCompile(`Entire user-level extension, hook version (\d+)`)

// userExtensionSource is $PI_CODING_AGENT_DIR/extensions/entire/index.ts
// (default ~/.pi/agent/...), which Pi loads in every project. The extension
// steps aside itself (see userLevelStepAside).
type userExtensionSource struct{}

// UserFile implements agent.UserFileSource.
func (userExtensionSource) UserFile() (agent.GeneratedUserFile, error) {
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
