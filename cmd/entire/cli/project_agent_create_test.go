package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// projectAgentCreateArgs is a valid create of a custom agent; extra flags
// follow it.
func projectAgentCreateArgs(extra ...string) []string {
	return append([]string{"create", "--name", "Migration Risk", "--harness", "claude", "--signal", "percent",
		"--instructions", "Score the migration risk of {{branch}}."}, extra...)
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_SendsACustomAgent(t *testing.T) {
	fake := &fakeAgentsAPI{envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)

	out, errOut, err := executeProjectAgent(t, projectAgentCreateArgs(
		"--description", "Migrations that could lock or lose data",
		"--polarity", "lower_is_better", "--model", "claude-opus-5-5", "--reasoning-effort", "HIGH",
		"--timeout", "15m", "--when", "always", "--branch", "main", "--branch", " release/* ", "--path", "db/**")...)
	require.NoError(t, err)
	assert.Contains(t, out, "created Migration Risk (")
	assert.Contains(t, out, "reviewing gh/acme/widget")
	assert.Contains(t, errOut, "enrolled in billing")

	posts := fake.writesTo(http.MethodPost)
	require.Len(t, posts, 1, "the project's one environment is used, not created")
	require.Equal(t, agentTestBase+"/agents", posts[0].Path)
	assert.NotEmpty(t, posts[0].IdempotencyKey)
	got, err := json.Marshal(posts[0].Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"name": "Migration Risk",
		"description": "Migrations that could lock or lose data",
		"is_enabled": true,
		"harness": "claude",
		"model": "claude-opus-5-5",
		"reasoning_effort": "high",
		"timeout_ms": 900000,
		"instructions": "Score the migration risk of {{branch}}.",
		"signal": {"value_type": "percent", "polarity": "lower_is_better"},
		"repository_ids": ["`+agentTestRepoID+`"],
		"triggers": [{"kind": "push", "when": "always", "branches": ["main", "release/*"], "paths": ["db/**"]}],
		"environment_id": "01JENV0000000000000000000"
	}`, string(got), "a custom agent sends its definition and no preset")
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_DefaultsLeaveTheServerToChoose(t *testing.T) {
	fake := &fakeAgentsAPI{envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)

	_, _, err := executeProjectAgent(t, projectAgentCreateArgs()...)
	require.NoError(t, err)
	body := fake.writesTo(http.MethodPost)[0].Body
	for _, field := range []string{"preset", "model", "reasoning_effort", "description"} {
		assert.NotContains(t, body, field, "omitted: the harness's default or none")
	}
	assert.Equal(t, map[string]any{"value_type": "percent"}, body["signal"], "no polarity: the server stores neutral")
	assert.InDelta(t, 600000, body["timeout_ms"], 0, "ten minutes by default")
	assert.Equal(t, []any{map[string]any{"kind": "push", "when": "ready"}}, body["triggers"])
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_SavesANewDefaultEnvironmentWithTheAgent(t *testing.T) {
	fake := &fakeAgentsAPI{}
	stubProjectAgentAPI(t, fake)

	_, errOut, err := executeProjectAgent(t, projectAgentCreateArgs()...)
	require.NoError(t, err)
	assert.Contains(t, errOut, "created environment Default (01JENV0000000000000000000)")
	// One request: an environment saved apart from the agent would outlive a
	// refused create.
	posts := fake.writesTo(http.MethodPost)
	require.Len(t, posts, 1)
	assert.Equal(t, agentTestBase+"/agents", posts[0].Path)
	assert.Equal(t, map[string]any{"name": "Default", "description": "Created by the Entire CLI"}, posts[0].Body["environment"])
	assert.NotContains(t, posts[0].Body, "environment_id")
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_RunsInTheNamedEnvironment(t *testing.T) {
	envs := []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}, {ID: "01JENV0000000000000000001", Name: "Staging"}}
	for _, want := range []string{"staging", "01JENV0000000000000000001"} {
		fake := &fakeAgentsAPI{envs: envs}
		stubProjectAgentAPI(t, fake)
		_, _, err := executeProjectAgent(t, projectAgentCreateArgs("--environment", want)...)
		require.NoError(t, err, want)
		assert.Equal(t, "01JENV0000000000000000001", fake.writesTo(http.MethodPost)[0].Body["environment_id"], want)
	}

	fake := &fakeAgentsAPI{envs: envs}
	stubProjectAgentAPI(t, fake)
	_, _, err := executeProjectAgent(t, projectAgentCreateArgs("--environment", "prod")...)
	require.ErrorContains(t, err, `no environment "prod"`)
	require.ErrorContains(t, err, "Default, Staging")
	// Several environments and none named, without a terminal to ask.
	_, _, err = executeProjectAgent(t, projectAgentCreateArgs()...)
	require.ErrorContains(t, err, "pass --environment with one of: Default, Staging")
	assert.Empty(t, fake.writesTo(http.MethodPost), "no agent without an environment")
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_ReportsARefusedCreate(t *testing.T) {
	fake := &fakeAgentsAPI{envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}, refuseCreate: http.StatusConflict}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, projectAgentCreateArgs()...)
	require.ErrorContains(t, err, `create agent "Migration Risk" in gh/acme`)
	require.ErrorContains(t, err, "already exists")
	assert.NotContains(t, out, "created")
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_StopsWhenTheServerSavesNoEnvironment(t *testing.T) {
	fake := &fakeAgentsAPI{ignoreInlineEnv: true}
	stubProjectAgentAPI(t, fake)

	_, errOut, err := executeProjectAgent(t, projectAgentCreateArgs()...)
	require.ErrorContains(t, err, "created Migration Risk (")
	require.ErrorContains(t, err, "saved no Default environment")
	assert.NotContains(t, errOut, "created environment", "an empty id is never reported as an environment")
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_ManualAndDisabled(t *testing.T) {
	fake := &fakeAgentsAPI{envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, projectAgentCreateArgs("--when", "manual", "--disabled")...)
	require.NoError(t, err)
	assert.Contains(t, out, ", disabled")
	body := fake.writesTo(http.MethodPost)[0].Body
	assert.NotContains(t, body, "triggers", "a manual agent has no trigger")
	assert.Equal(t, false, body["is_enabled"], "sent explicitly")
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_ReadsInstructionsFromAFileOrStdin(t *testing.T) {
	fake := &fakeAgentsAPI{envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)
	path := filepath.Join(t.TempDir(), "review.md")
	require.NoError(t, os.WriteFile(path, []byte("\nReview {{branch}} against {{base_branch}}.\n"), 0o600))

	base := []string{"create", "--name", "From file", "--harness", "codex", "--signal", "boolean"}
	_, _, err := executeProjectAgent(t, append(base, "--instructions-file", path)...)
	require.NoError(t, err)

	cmd := newProjectAgentCmd()
	cmd.PersistentFlags().Bool("insecure-http-auth", false, "")
	cmd.SetIn(strings.NewReader("Review from stdin."))
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{"create", "--name", "From stdin", "--harness", "codex", "--signal", "boolean", "--instructions-file", "-"})
	cmd.SetContext(t.Context())
	require.NoError(t, cmd.Execute())

	posts := fake.writesTo(http.MethodPost)
	require.Len(t, posts, 2)
	assert.Equal(t, "Review {{branch}} against {{base_branch}}.", posts[0].Body["instructions"], "surrounding whitespace trimmed")
	assert.Equal(t, "Review from stdin.", posts[1].Body["instructions"])
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentCreate_JSONIsTheSavedAgent(t *testing.T) {
	fake := &fakeAgentsAPI{envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, projectAgentCreateArgs("--json")...)
	require.NoError(t, err)
	var got api.ProjectAgent
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "Migration Risk", got.Name)
	assert.NotEmpty(t, got.ID)
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentDurationLabel(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m", 90 * time.Minute: "1h30m", 30 * time.Second: "30s", 90 * time.Second: "1m30s"} {
		assert.Equal(t, want, projectAgentDurationLabel(d), d)
	}
}

func TestProjectAgentCreate_RefusesBadFlagsBeforeAnyRequest(t *testing.T) {
	old := resolveProjectAgentTarget
	t.Cleanup(func() { resolveProjectAgentTarget = old })
	resolveProjectAgentTarget = func(*cobra.Command) (*projectAgentTarget, error) {
		t.Error("the project was resolved before the flags were checked")
		return nil, assert.AnError
	}
	missing := filepath.Join(t.TempDir(), "missing.md")
	long := strings.Repeat("x", projectAgentMaxInstructions+1)
	// The API counts bytes: these are under the limits in characters but
	// over them in bytes.
	wideInstructions := strings.Repeat("é", projectAgentMaxInstructions/2+1)
	wideFile := filepath.Join(t.TempDir(), "wide.md")
	require.NoError(t, os.WriteFile(wideFile, []byte(wideInstructions), 0o600))

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "--harness", "claude", "--signal", "percent", "--instructions", "x"}, "--name is required"},
		{[]string{"create", "--name", " ", "--harness", "claude", "--signal", "percent", "--instructions", "x"}, "--name is required"},
		{[]string{"create", "--name", "A", "--signal", "percent", "--instructions", "x"}, "--harness is required: claude, codex"},
		{[]string{"create", "--name", "A", "--harness", "gemini", "--signal", "percent", "--instructions", "x"}, `--harness must be one of claude, codex, not "gemini"`},
		{[]string{"create", "--name", "A", "--harness", "claude", "--instructions", "x"}, "--signal is required"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "score", "--instructions", "x"}, "--signal must be one of"},
		{projectAgentCreateArgs("--polarity", "up"), "--polarity must be one of"},
		{projectAgentCreateArgs("--when", "never"), "--when must be one of"},
		{projectAgentCreateArgs("--when", "manual", "--path", "db/**"), "--when manual has none"},
		{projectAgentCreateArgs("--branch", ""), "--branch needs a non-empty pattern"},
		{projectAgentCreateArgs("--path", "  "), "--path needs a non-empty pattern"},
		{projectAgentCreateArgs("--path", "/db/**"), "must not start or end with /"},
		{projectAgentCreateArgs("--branch", "release/"), "must not start or end with /"},
		{projectAgentCreateArgs("--path", strings.Repeat("a", 257)), "limited to 256 bytes"},
		{projectAgentCreateArgs("--timeout", "31m"), "--timeout must be between 1s and 30m"},
		{projectAgentCreateArgs("--timeout", "0s"), "--timeout must be between"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "percent"}, "pass one of --instructions or --instructions-file"},
		{projectAgentCreateArgs("--instructions-file", "x.md"), "pass one of --instructions or --instructions-file"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "percent", "--instructions", "  "}, "the instructions are empty"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "percent", "--instructions", long}, "limited to 16384 bytes"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "percent", "--instructions", wideInstructions}, "the instructions are limited to 16384 bytes"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "percent", "--instructions-file", wideFile}, "the instructions are limited to 16384 bytes"},
		{projectAgentCreateArgs("--name", strings.Repeat("é", projectAgentMaxName/2+1)), "--name is limited to 128 bytes"},
		{projectAgentCreateArgs("--description", strings.Repeat("é", projectAgentMaxDescription/2+1)), "--description is limited to 1024 bytes"},
		{[]string{"create", "--name", "A", "--harness", "claude", "--signal", "percent", "--instructions-file", missing}, "read --instructions-file"},
	} {
		_, _, err := executeProjectAgent(t, tc.args...)
		require.ErrorContains(t, err, tc.want, "args %q", tc.args)
	}
	_, _, err := executeProjectAgent(t, projectAgentCreateArgs("--timeout", "31m")...)
	require.NotContains(t, err.Error(), "30m0s", "the bound reads as the help writes it")
}
