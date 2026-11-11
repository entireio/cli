package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/google/uuid"
)

const agentOutputLimit = 8 * 1024 * 1024
const nativeCloneScheme = "entire"
const agentRestrictedFlag = "--restricted"

var agentCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type projectAgentExecutor struct {
	git, claude string
	run         func(context.Context, string, string, []string, []string, string) ([]byte, error)
}

func newProjectAgentExecutor(ctx context.Context) (*projectAgentExecutor, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("self-hosted execution currently requires macOS or Linux")
	}
	if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" && !filepath.IsAbs(config) {
		return nil, errors.New("CLAUDE_CONFIG_DIR must be absolute")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git is required: %w", err)
	}
	claude, err := exec.LookPath(activityAgentClaude)
	if err != nil {
		return nil, fmt.Errorf("claude Code is required: %w", err)
	}
	if !filepath.IsAbs(git) || !filepath.IsAbs(claude) {
		return nil, errors.New("git and claude must resolve to absolute executable paths")
	}
	e := &projectAgentExecutor{git: git, claude: claude, run: runProjectAgentProcess}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := e.run(probeCtx, claude, os.TempDir(), projectAgentHarnessEnv(), []string{agentRestrictedFlag, "--help"}, "")
	if err != nil {
		return nil, fmt.Errorf("check Claude capabilities: %w", err)
	}
	for _, flag := range []string{agentRestrictedFlag, "--strict-mcp-config", "--json-schema"} {
		if !bytes.Contains(out, []byte(flag)) {
			return nil, fmt.Errorf("claude Code must support %s; update Claude before serving jobs (no unrestricted fallback)", flag)
		}
	}
	return e, nil
}

// Only harness authentication and basic OS runtime variables cross this boundary.
// In particular no ENTIRE_*, git configuration, caller-session markers or arbitrary
// environment variables from the worker reach the model process.
func projectAgentHarnessEnv() []string {
	keys := []string{"HOME", "USERPROFILE", "SystemRoot", "WINDIR", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}
	env := []string{"PATH=" + strings.Join(execx.PathScanDirs(), string(os.PathListSeparator))}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func projectAgentGitEnv() []string {
	var env []string
	for _, entry := range gitrepo.EnvWithoutRepoOverrides() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "PATH" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "PATH="+strings.Join(execx.PathScanDirs(), string(os.PathListSeparator)), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
}

func validateProjectAgentClaim(c projectAgentClaim) error {
	if !agentCommitPattern.MatchString(c.HeadSHA) || !agentCommitPattern.MatchString(c.BaseSHA) {
		return errors.New("claim must contain full hexadecimal base and head commit IDs")
	}
	u, err := url.Parse(c.GitRemoteURL)
	if err != nil || u.Scheme != nativeCloneScheme || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("claim must contain a credential-free entire:// clone URL")
	}
	if c.Harness != activityAgentClaude || !json.Valid(c.OutputSchema) {
		return errors.New("unsupported harness or invalid result schema")
	}
	return nil
}

func (e *projectAgentExecutor) execute(ctx context.Context, c projectAgentClaim) (result json.RawMessage, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("review cancelled: %w", err)
	}
	if err := validateProjectAgentClaim(c); err != nil {
		return nil, err
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return nil, fmt.Errorf("open workspace cache: %w", err)
	}
	work := "agent-work/" + uuid.NewString()
	if err := osroot.MkdirAllNoSymlink(root, work+"/review", 0o700); err != nil {
		return nil, fmt.Errorf("create review directory: %w", err)
	}
	defer func() {
		if err := osroot.RemoveAllNoSymlinks(root, work); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove review workspace: %w", err))
		}
	}()
	if err := osroot.MkdirAllNoSymlink(root, work+"/empty", 0o700); err != nil {
		return nil, fmt.Errorf("create empty hooks directory: %w", err)
	}
	dir := filepath.Join(root.Name(), filepath.FromSlash(work))
	repo := filepath.Join(dir, "repo")
	gitEnv := projectAgentGitEnv()
	gitArgs := []string{"-c", "core.hooksPath=" + filepath.Join(dir, "empty"), "-c", "core.symlinks=false", "-c", "protocol.allow=never", "-c", "protocol.entire.allow=always", "-c", "protocol.https.allow=always"}
	clone := append(append([]string{}, gitArgs...), "clone", "--no-checkout", "--template="+filepath.Join(dir, "empty"), "--", c.GitRemoteURL, repo)
	if _, err := e.run(ctx, e.git, dir, gitEnv, clone, ""); err != nil {
		return nil, fmt.Errorf("clone failed: %w", err)
	}
	checkout := append(append([]string{}, gitArgs...), "checkout", "--detach", c.HeadSHA, "--")
	if _, err := e.run(ctx, e.git, repo, gitEnv, checkout, ""); err != nil {
		return nil, fmt.Errorf("checkout failed: %w", err)
	}
	diffArgs := append(append([]string{}, gitArgs...), "diff-tree", "--no-commit-id", "-r", "--no-ext-diff", "--no-textconv", "--stat", "--patch", c.BaseSHA, c.HeadSHA, "--")
	diff, err := e.run(ctx, e.git, repo, gitEnv, diffArgs, "")
	if err != nil {
		return nil, fmt.Errorf("diff failed: %w", err)
	}
	if err := osroot.WriteFile(root, work+"/review/change.diff", diff, 0o600); err != nil {
		return nil, fmt.Errorf("write review diff: %w", err)
	}
	reviewDir := filepath.Join(dir, "review")
	args := []string{"-p", "--output-format", "json", "--json-schema", string(c.OutputSchema), agentRestrictedFlag, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--settings", `{"disableAllHooks":true}`, "--no-session-persistence", "--tools", "Read,Grep,Glob", "--add-dir", reviewDir}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	prompt := c.Prompt + "\nThe diff is in " + filepath.Join(reviewDir, "change.diff") + ". You cannot run commands."
	output, err := e.run(ctx, e.claude, repo, projectAgentHarnessEnv(), args, prompt)
	if err != nil {
		return nil, fmt.Errorf("claude review failed: %w", err)
	}
	return projectAgentStructuredResult(output)
}

func projectAgentStructuredResult(output []byte) (json.RawMessage, error) {
	var result struct {
		IsError    bool              `json:"is_error"`
		Type       string            `json:"type"`
		Subtype    string            `json:"subtype"`
		Denials    []json.RawMessage `json:"permission_denials"`
		Structured json.RawMessage   `json:"structured_output"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, errors.New("claude returned invalid JSON")
	}
	if result.IsError || result.Type != "result" || result.Subtype != "success" || len(result.Denials) != 0 || len(result.Structured) == 0 || bytes.Equal(bytes.TrimSpace(result.Structured), []byte("null")) {
		return nil, errors.New("claude returned no successful structured result")
	}
	return result.Structured, nil
}

// Bound output in memory and never surface raw subprocess diagnostics: clone and
// harness errors can include secrets or source code. The stage identifies failure.
func runProjectAgentProcess(ctx context.Context, binary, dir string, env, args []string, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	execx.TerminateOnCancel(cmd)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	cmd.Stderr = io.Discard
	var out projectAgentLimitedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("review process cancelled: %w", ctx.Err())
		}
		return nil, errors.New("subprocess failed (output withheld)")
	}
	return out.Bytes(), nil
}

type projectAgentLimitedBuffer struct{ bytes.Buffer }

func (b *projectAgentLimitedBuffer) Write(p []byte) (int, error) {
	if len(p) > agentOutputLimit-b.Len() {
		return 0, errors.New("review output exceeds 8 MiB")
	}
	n, _ := b.Buffer.Write(p) // bytes.Buffer.Write always succeeds.
	return n, nil
}
