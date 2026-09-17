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
func announceContext(f *contexts.File, c *contexts.Context) {
	if f == nil || c == nil || len(f.Contexts) < 2 {
		return
	}
	if contextAnnounced.Swap(true) {
		return
	}
	fmt.Fprintf(contextNoticeW, "Using context '%s'.\n", c.Name)
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
