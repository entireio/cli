package strategy

// ResetRedactionConfiguredForTest reinitializes the EnsureRedactionConfigured
// sync.Once so a test in another package can assert that a command's entry
// point configures redaction. Without it the Once may already be consumed by
// an earlier test in the same process, and the assertion passes or fails on
// test order rather than on the wiring under test.
func ResetRedactionConfiguredForTest() {
	resetRedactionConfiguredForTest()
}
