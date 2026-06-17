package githelper

// Shared test fixtures. Extracted to package-level constants so repeated
// literals across the test files satisfy goconst.
const (
	testRefMain          = "refs/heads/main"
	testRefFeatureBranch = "refs/heads/feature-branch"
	testHeadSHA          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)
