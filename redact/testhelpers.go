package redact

// Cross-package test helpers. Lives in a regular .go file (not
// export_test.go) so tests in cmd/entire/cli/strategy can call it.
// The "ForTest" suffix is the production-code-must-not-call signal.

// ResetOPFConfigForTest clears OPF configuration and the circuit
// breaker. Test-only.
func ResetOPFConfigForTest() {
	resetOPFConfig()
}

// SetScannerDegradedForTest flips the scanner degradation flag. Runtime
// scan errors are engineered out (see detectGoredact), so tests cannot
// reach this state organically. Exported for redact's own sentinel tests
// and for dependent packages' tests of the checkpoint write paths that
// consume ErrScannerDegraded.
// Test-only.
func SetScannerDegradedForTest(v bool) {
	scannerDegraded.Store(v)
}

// testingTB is the subset of testing.TB these helpers need, declared
// locally so package redact does not pull testing (and flag, runtime/pprof,
// runtime/trace) into every binary that imports it. *testing.T satisfies it.
type testingTB interface {
	Helper()
	Cleanup(f func())
	Fatalf(format string, args ...any)
}

// WithScannerDegradedSole configures a goredact-only scanner set and flips
// the degradation flag so JSONLBytes/JSONLBytesWithPrivacyFilter return
// ErrScannerDegraded, registering a cleanup that restores the betterleaks
// default. Named With (not Set) per Go convention because it registers
// cleanup. Callers must NOT use t.Parallel — the scanner state is
// process-global. Test-only.
func WithScannerDegradedSole(t testingTB) {
	t.Helper()
	t.Cleanup(func() {
		// ConfigureScanners resets the degradation flag itself; no explicit
		// clear, so that reset behavior stays load-bearing here.
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore scanners: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners: %v", err)
	}
	SetScannerDegradedForTest(true)
}
