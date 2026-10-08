package redact

import (
	"io"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// OPF progress UX (→ scanning / ✓ done / × unavailable) defaults to
	// os.Stderr in production. Silence it process-wide for tests so it
	// neither bleeds into `go test -v` output nor needs a per-test
	// override. No test captures opfStderr to assert on its content; the
	// strategy package suppresses its own pre-push progress writer the
	// same way (see strategy/global_test.go).
	opfStderr = io.Discard
	os.Exit(m.Run())
}
