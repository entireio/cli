package auth

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// contextNoticeW receives the acting-login notice. Package-level for the same
// reason clusterdiscovery.autoSelectNoticeW is: the notice is written deep
// inside resolution, far from the cobra command that owns the streams.
var contextNoticeW io.Writer = os.Stderr

var (
	contextAnnounced atomic.Bool
	contextSilenced  atomic.Bool
)

// SilenceContextNotice suppresses the acting-login notice for the rest of this
// process.
//
// `entire agent-help` uses it. Its output is read by an agent, and the login it
// resolves there authenticates a background trail-enablement probe rather than
// work the user asked to have done as a particular identity — so naming the
// login is noise in a place that is expensive to be noisy in.
func SilenceContextNotice() { contextSilenced.Store(true) }

// announceContext names the acting login once per process.
//
// Only when several logins are saved: with one, the answer is obvious and the
// line would be noise on every command.
func announceContext(saved int, c *contexts.Context) {
	if c == nil {
		return
	}
	AnnounceContext(saved, c.Name)
}

// AnnounceContext is announceContext for a caller that resolved the login
// itself (`auth token`, `api --to core`): name is the acting context, saved the
// number stored.
//
// Nothing is said when the user named the identity for this invocation with
// `--context`/$ENTIRE_CONTEXT — echoing back what they just typed is noise, and
// clusterdiscovery.selectLoginContext draws the same line: it announces only a
// login the user did NOT select. An explicit selection is also the only one the
// resolvers may act as, so silence here can never hide a different identity.
func AnnounceContext(saved int, name string) {
	if name == "" || saved < 2 {
		return
	}
	if contextSilenced.Load() || contexts.Requested() {
		return
	}
	if contextAnnounced.Swap(true) {
		return
	}
	fmt.Fprintf(contextNoticeW, "Using context '%s'.\n", name)
}

// announceLogin is announceContext for a login picked by host discovery.
func announceLogin(c *contexts.Context) {
	all, _, err := StoredContexts()
	if err != nil {
		return
	}
	announceContext(len(all), c)
}

// CaptureContextNoticeForTest redirects the notice into w and re-arms it.
func CaptureContextNoticeForTest(t interface {
	Helper()
	Cleanup(restore func())
}, w io.Writer,
) {
	t.Helper()
	prev := contextNoticeW
	contextNoticeW = w
	contextAnnounced.Store(false)
	contextSilenced.Store(false)
	t.Cleanup(func() {
		contextNoticeW = prev
		contextAnnounced.Store(false)
		contextSilenced.Store(false)
	})
}
