package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExternalAgentBinary_DoesNotExecuteBinary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	script := "#!/bin/sh\n: > " + marker + "\n"

	WriteExternalAgentBinary(t, dir, "no-warmup", script)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("WriteExternalAgentBinary executed the fixture, stat error = %v", err)
	}
}
