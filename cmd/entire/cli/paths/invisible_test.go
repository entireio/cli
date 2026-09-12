package paths

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// TestMalformedSettingsStillFailsOpenForHooks is the property a future cleanup
// is most likely to break.
//
// A settings file that will not parse is presented by its own message, so the
// user is pointed at the JSON rather than at .entire. But it must STILL match
// ErrUnroutableRuntimePath, because the hook paths branch on that sentinel to
// warn and skip capture rather than failing the user's agent turn. Presenting
// it better must not turn a skipped capture into a broken turn.
func TestMalformedSettingsStillFailsOpenForHooks(t *testing.T) {
	t.Parallel()

	err := unroutableSettingsError{err: fmt.Errorf("%w: bad json", repopolicy.ErrRepoSettingsMalformed)}

	if !errors.Is(err, ErrUnroutableRuntimePath) {
		t.Error("hooks branch on ErrUnroutableRuntimePath to fail open; the malformed-settings error must still match it")
	}
	if !errors.Is(err, repopolicy.ErrRepoSettingsMalformed) {
		t.Error("presentation branches on ErrRepoSettingsMalformed to name the real cause")
	}
	if strings.Contains(err.Error(), "route cannot be verified") {
		t.Errorf("the message must name the settings file, not the route: %s", err)
	}
}
