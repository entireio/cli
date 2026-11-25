package review

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// terminalProbeCallers lists the review files allowed to call
// mdrender.RenderForWriter, which queries the terminal for its background
// colour. The query's reply arrives on the terminal's input, so making it while
// the dashboard is up races the dashboard's input reader: the query can block
// until the next keypress (the judge "consolidating" for minutes) and then
// restore the dashboard's raw mode after the dashboard has exited.
//
// This is a tripwire, not a proof: it matches by file, and it cannot see a
// query made through another package. Code that renders while the dashboard
// may be up uses TUISink.DarkBackground instead.
var terminalProbeCallers = map[string]string{
	"cmd/entire/cli/review/fix.go":            "`entire review --findings` prints without a dashboard",
	"cmd/entire/cli/review/synthesis_sink.go": "fallback when Render is unset; only non-TTY runs leave it unset, and their writer is not a terminal",
}

func TestReviewTerminalProbeCallers(t *testing.T) {
	t.Parallel()
	root, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	out := testutil.GitGrepGuard(t, root, "-l", "-F", "mdrender.RenderForWriter(", "--", "cmd/entire/cli/review/*.go", ":!*_test.go")
	for _, file := range strings.Fields(out) {
		if _, allowed := terminalProbeCallers[file]; !allowed {
			t.Errorf("%s calls mdrender.RenderForWriter, which queries the terminal; while the dashboard may be up, render with TUISink.DarkBackground via mdrender.RenderForWriterWithBackground (or add it to terminalProbeCallers with the reason no dashboard can be up)", file)
		}
	}

	// termenv's terminal queries are the same hazard; review reaches them only
	// through mdrender. The pathspec is wider than review so the guard fails
	// loudly if the detection pattern goes stale instead of passing on zero matches.
	out = testutil.GitGrepGuard(t, root, "-l", "-F", "github.com/muesli/termenv", "--", "cmd/entire/cli/*.go", ":!*_test.go")
	for _, file := range strings.Fields(out) {
		if strings.HasPrefix(file, "cmd/entire/cli/review/") {
			t.Errorf("%s imports termenv; review must not query the terminal directly (see terminalProbeCallers)", file)
		}
	}
}
