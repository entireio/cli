package gitproto

// Shared test fixtures. Extracted to package-level constants so repeated
// literals across the test files satisfy goconst.
const (
	testRefMain          = "refs/heads/main"
	testRefFeatureBranch = "refs/heads/feature-branch"
	testHeadSHA          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testFakePackData     = "PACK fake pack data here"
)
