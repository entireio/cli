package logging_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// LogsDir and LogsName spell the same directory in two coordinates and are
// declared separately (a const block cannot call MustName). Nothing but this
// test stops them drifting, and a mismatch would have the writer create one
// directory while doctor, trace, and the bundle read another.
func TestLogsNameMatchesLogsDir(t *testing.T) {
	t.Parallel()

	if got := entiredir.MustName(logging.LogsDir); got != logging.LogsName {
		t.Errorf("LogsName = %q, but %q resolves to %q", logging.LogsName, logging.LogsDir, got)
	}
}
