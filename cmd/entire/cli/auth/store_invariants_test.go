package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductionAuthStoreIsKeyringOnly enforces that the file-backed auth
// store backend stays fully gated behind //go:build authfilestore. The
// production CLI binary (no build tag) must not contain code that reads
// the test env var or persists tokens to disk — otherwise an end user
// can flip the variable in their shell and silently bypass the OS
// keyring.
//
// This runs as part of the regular (untagged) test suite, so any new
// production-compiled .go file in this package that mentions the
// forbidden symbols will fail CI immediately. The remediation is to
// move the offending code into a //go:build authfilestore file
// alongside store_filebackend.go.
func TestProductionAuthStoreIsKeyringOnly(t *testing.T) {
	t.Parallel()

	// Symbols that may only appear in authfilestore-tagged files.
	// Keep this list tight — these are the load-bearing markers of
	// "we are persisting auth tokens to a file from production code".
	forbidden := []string{
		"ENTIRE_TEST_AUTH_STORE_FILE", // the test env-var hook
		"os.WriteFile",                // token-on-disk write
		"os.ReadFile",                 // token-on-disk read
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir auth pkg: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// _test.go files are never in the production binary, so they
		// cannot reintroduce the file backend regardless of contents.
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(data)

		if hasAuthFileStoreBuildTag(src) {
			continue
		}

		for _, sym := range forbidden {
			if strings.Contains(src, sym) {
				t.Errorf(
					"%s references %q outside a //go:build authfilestore file. "+
						"File-backed auth storage must stay gated so production "+
						"builds cannot opt into it. Move this code into a tagged "+
						"file (see store_filebackend.go).",
					name, sym,
				)
			}
		}
	}
}

// hasAuthFileStoreBuildTag reports whether the file's build constraint
// requires the authfilestore tag. Build constraints must appear before
// the package clause, so we only scan up to that point.
func hasAuthFileStoreBuildTag(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			return false
		}
		if strings.HasPrefix(trimmed, "//go:build ") &&
			strings.Contains(trimmed, "authfilestore") {
			return true
		}
	}
	return false
}
