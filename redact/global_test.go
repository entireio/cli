package redact

import (
	"flag"
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
	// Benchmarks keep the shipped shard size.
	flag.Parse()
	if !benchmarking() {
		jsonlShardTargetBytes = 16 << 10
	}
	os.Exit(m.Run())
}

// Reports whether -test.bench was set.
func benchmarking() bool {
	f := flag.Lookup("test.bench")
	return f != nil && f.Value.String() != ""
}

// The default, captured before TestMain runs.
var shippedShardTargetBytes = jsonlShardTargetBytes

// Pins the shipped shard size.
func TestJSONLShardTargetBytes_ProductionDefault(t *testing.T) {
	t.Parallel()
	if shippedShardTargetBytes != 1<<20 {
		t.Fatalf("production shard target = %d, want 1MiB", shippedShardTargetBytes)
	}
}
