package strategy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
)

const checkpointAuthHelpURL = "https://docs.entire.io/troubleshooting/checkpoint-auth"

func isHTTPCheckpointAuthFailure(err error) bool {
	return errors.Is(err, remote.ErrHTTPAuthUnavailable) || errors.Is(err, remote.ErrHTTPAuthRejected)
}

// Shared by policy refresh and both push backends. Printing the hint must not
// determine whether a caller skips recovery: later failures still return true.
var checkpointHTTPAuthHintOnce sync.Once

func reportHTTPCheckpointAuthFailure(err error) bool {
	if !isHTTPCheckpointAuthFailure(err) {
		return false
	}
	checkpointHTTPAuthHintOnce.Do(func() { writeCheckpointHTTPAuthHint(os.Stderr, err) })
	return true
}

func writeCheckpointHTTPAuthHint(w io.Writer, err error) {
	fmt.Fprintln(w, "[entire] Checkpoint HTTPS authentication failed.")
	if errors.Is(err, remote.ErrHTTPAuthUnavailable) {
		fmt.Fprintln(w, "[entire] Credentials entered for git push aren't automatically available to Entire's hook; configure a Git credential helper or ssh-agent.")
	} else {
		fmt.Fprintln(w, "[entire] The server rejected authentication; check your credentials and repository access.")
	}
	fmt.Fprintln(w, "[entire] Help: "+checkpointAuthHelpURL)
	fmt.Fprintln(w, "[entire] Checkpoints remain local for a later push. This authentication failure does not block your code push.")
}
