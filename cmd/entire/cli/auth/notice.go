package auth

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// contextNoticeW receives the acting-login notice.
var contextNoticeW io.Writer = os.Stderr

var contextAnnounced atomic.Bool

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
// itself (`auth token`): name is the acting context, saved the number stored.
func AnnounceContext(saved int, name string) {
	if name == "" || saved < 2 {
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
	t.Cleanup(func() {
		contextNoticeW = prev
		contextAnnounced.Store(false)
	})
}
