//go:build integration

package integration

// Literals reused across test files in this package, extracted because goconst
// flags a string repeated three or more times.
const (
	windowsGOOS = "windows"

	contentV1 = "version 1"
	contentV2 = "version 2"
	contentV3 = "version 3"

	preservedSetting = "should-be-preserved"

	pkgFuncA = "package main\n\nfunc A() {}\n"
	pkgFuncB = "package main\n\nfunc B() {}\n"

	pathDeviceAuthorization = "/device_authorization"
	pathOAuthToken          = "/oauth/token"

	// agentClaudeCode mirrors agent.AgentNameClaudeCode, which is a
	// types.AgentName and so cannot be used where a plain string is wanted.
	agentClaudeCode = "claude-code"

	testSessionID = "test-session"
)
