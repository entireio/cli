package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"golang.org/x/mod/semver"
)

// Claude Code added StopFailure in 2.1.78. Older versions ignore the project
// hook configuration when it contains that event, including the capture hooks.
// https://github.com/anthropics/claude-code/releases/tag/v2.1.78
const minimumClaudeHookVersion = "v2.1.78"

// Isolate the installed executable in unit tests. Do not cache the result:
// callers can update Claude or change PATH between setup and diagnosis.
var installedClaudeHookVersion = probeClaudeHookVersion

func probeClaudeHookVersion(ctx context.Context) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := execx.NonInteractive(probeCtx, "claude", "--version")
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("check Claude Code version: %w", ctx.Err())
	}
	if errors.Is(err, exec.ErrNotFound) {
		// Allow preparing a repository before Claude is installed. A missing
		// executable cannot capture sessions; it is not an incompatible one.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot check Claude Code version; run `claude --version`: %w", err)
	}
	return parseClaudeHookVersion(string(out))
}

func parseClaudeHookVersion(out string) (string, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", errors.New("cannot read Claude Code version; run `claude --version`")
	}
	version := "v" + strings.TrimPrefix(fields[0], "v")
	if !semver.IsValid(version) {
		return "", errors.New("cannot read Claude Code version; run `claude --version`")
	}
	return version, nil
}

func checkClaudeHookVersion(ctx context.Context, name types.AgentName) error {
	if name != agent.AgentNameClaudeCode {
		return nil
	}
	version, err := installedClaudeHookVersion(ctx)
	if err != nil {
		return err
	}
	if version == "" || semver.Compare(version, minimumClaudeHookVersion) >= 0 {
		return nil
	}
	return fmt.Errorf("installed Claude Code %s cannot load Entire's StopFailure hook. Update Claude Code to %s or later, then run `entire agent add claude-code`",
		strings.TrimPrefix(version, "v"), strings.TrimPrefix(minimumClaudeHookVersion, "v"))
}
