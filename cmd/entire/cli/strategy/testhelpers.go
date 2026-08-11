package strategy

import "sync"

// Cross-package test helpers. Lives in a regular .go file (not
// export_test.go) so tests in cmd/entire/cli can call it. The "ForTest"
// suffix is the production-code-must-not-call signal (same convention as
// redact.ResetOPFConfigForTest).

// ResetRedactionConfiguredForTest re-arms the EnsureRedactionConfigured
// once-guard so a test can observe a fresh configuration pass regardless of
// which earlier test in the process fired it. Test-only.
func ResetRedactionConfiguredForTest() {
	initRedactionOnce = sync.Once{}
}
