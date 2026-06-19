package redact

// Cross-package test helpers. Lives in a regular .go file (not
// export_test.go) so tests in cmd/entire/cli/strategy can call it.
// The "ForTest" suffix is the production-code-must-not-call signal.

// ResetOPFConfigForTest clears OPF configuration and the circuit
// breaker. Test-only.
func ResetOPFConfigForTest() {
	resetOPFConfig()
}
