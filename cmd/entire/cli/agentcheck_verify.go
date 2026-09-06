package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const (
	agentCheckEcosystemGo      = "go"
	agentCheckEcosystemNode    = "node"
	agentCheckEcosystemPython  = "python"
	agentCheckEcosystemUnknown = "unknown"

	agentCheckVerificationSuccess     = "success"
	agentCheckVerificationFailed      = "failed"
	agentCheckVerificationUnableToRun = "unable_to_run"

	defaultAgentCheckVerificationTimeout    = 10 * time.Minute
	defaultAgentCheckVerificationOutputSize = 64 * 1024
)

// agentCheckVerificationEvidence is Owner C's internal verification contract.
// It records what the verification command did, without deciding correctness.
type agentCheckVerificationEvidence struct {
	Command         string        `json:"command"`
	Ecosystem       string        `json:"ecosystem"`
	Status          string        `json:"status"`
	ExitCode        int           `json:"exit_code"`
	Duration        time.Duration `json:"duration"`
	Stdout          string        `json:"stdout"`
	StdoutTruncated bool          `json:"stdout_truncated"`
	Stderr          string        `json:"stderr"`
	StderrTruncated bool          `json:"stderr_truncated"`
}

type agentCheckVerificationOptions struct {
	RepoRoot       string
	Timeout        time.Duration
	MaxOutputBytes int
	Runner         agentCheckVerificationRunner
}

type agentCheckVerificationRunner interface {
	Run(ctx context.Context, repoRoot string, command agentCheckVerificationCommand, stdout, stderr io.Writer) (int, error)
}

type agentCheckVerificationCommand struct {
	Name string
	Args []string
}

func (c agentCheckVerificationCommand) String() string {
	if c.Name == "" {
		return ""
	}
	parts := make([]string, 0, 1+len(c.Args))
	parts = append(parts, c.Name)
	parts = append(parts, c.Args...)
	return strings.Join(parts, " ")
}

type agentCheckExecRunner struct{}

func (agentCheckExecRunner) Run(ctx context.Context, repoRoot string, command agentCheckVerificationCommand, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = repoRoot
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	return -1, err
}

func runAgentCheckVerification(ctx context.Context, opts agentCheckVerificationOptions) agentCheckVerificationEvidence {
	start := time.Now()
	evidence := agentCheckVerificationEvidence{
		Ecosystem: agentCheckEcosystemUnknown,
		Status:    agentCheckVerificationUnableToRun,
		ExitCode:  -1,
	}

	repoRoot := opts.RepoRoot
	if repoRoot == "" {
		evidence.Stderr = "repository root is empty"
		return evidence
	}

	ecosystem := detectAgentCheckEcosystem(repoRoot)
	evidence.Ecosystem = ecosystem

	command, ok := selectAgentCheckVerificationCommand(repoRoot, ecosystem)
	if !ok {
		evidence.Stderr = fmt.Sprintf("no verification command selected for %s ecosystem", ecosystem)
		return evidence
	}
	evidence.Command = command.String()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultAgentCheckVerificationTimeout
	}
	maxOutputBytes := opts.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultAgentCheckVerificationOutputSize
	}
	runner := opts.Runner
	if runner == nil {
		runner = agentCheckExecRunner{}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr boundedAgentCheckBuffer
	stdout.limit = maxOutputBytes
	stderr.limit = maxOutputBytes

	exitCode, err := runner.Run(runCtx, repoRoot, command, &stdout, &stderr)
	evidence.Duration = time.Since(start)
	evidence.ExitCode = exitCode
	evidence.Stdout = stdout.String()
	evidence.StdoutTruncated = stdout.Truncated()
	evidence.Stderr = stderr.String()
	evidence.StderrTruncated = stderr.Truncated()

	switch {
	case err == nil:
		evidence.Status = agentCheckVerificationSuccess
		evidence.ExitCode = 0
	case exitCode >= 0:
		evidence.Status = agentCheckVerificationFailed
	default:
		evidence.Status = agentCheckVerificationUnableToRun
		if evidence.Stderr == "" {
			evidence.Stderr = err.Error()
		}
	}

	return evidence
}

func runAgentCheckVerificationForCurrentWorktree(ctx context.Context, opts agentCheckVerificationOptions) agentCheckVerificationEvidence {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return agentCheckVerificationEvidence{
			Ecosystem: agentCheckEcosystemUnknown,
			Status:    agentCheckVerificationUnableToRun,
			ExitCode:  -1,
			Stderr:    fmt.Sprintf("resolve worktree root: %v", err),
		}
	}
	opts.RepoRoot = repoRoot
	return runAgentCheckVerification(ctx, opts)
}

func detectAgentCheckEcosystem(repoRoot string) string {
	switch {
	case agentCheckRegularFileExists(repoRoot, "go.mod"):
		return agentCheckEcosystemGo
	case agentCheckRegularFileExists(repoRoot, "package.json"):
		return agentCheckEcosystemNode
	case agentCheckRegularFileExists(repoRoot, "pytest.ini"),
		agentCheckRegularFileExists(repoRoot, ".pytest.ini"),
		agentCheckRegularFileExists(repoRoot, "pyproject.toml"),
		agentCheckRegularFileExists(repoRoot, "tox.ini"):
		return agentCheckEcosystemPython
	default:
		return agentCheckEcosystemUnknown
	}
}

func selectAgentCheckVerificationCommand(repoRoot, ecosystem string) (agentCheckVerificationCommand, bool) {
	switch ecosystem {
	case agentCheckEcosystemGo:
		if agentCheckRegularFileExists(repoRoot, "go.mod") {
			return agentCheckVerificationCommand{Name: "go", Args: []string{"test", "./..."}}, true
		}
	case agentCheckEcosystemNode:
		if script, ok := agentCheckPackageJSONScript(repoRoot, "test"); ok && !agentCheckIsPlaceholderNodeTest(script) {
			return agentCheckVerificationCommand{Name: "npm", Args: []string{"test"}}, true
		}
	case agentCheckEcosystemPython:
		if agentCheckRegularFileExists(repoRoot, "pytest.ini") || agentCheckRegularFileExists(repoRoot, ".pytest.ini") {
			return agentCheckVerificationCommand{Name: "python", Args: []string{"-m", "pytest"}}, true
		}
	}
	return agentCheckVerificationCommand{}, false
}

func agentCheckPackageJSONScript(repoRoot, name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "package.json")) //nolint:gosec // repoRoot is the selected worktree root.
	if err != nil {
		return "", false
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", false
	}
	script := strings.TrimSpace(pkg.Scripts[name])
	return script, script != ""
}

func agentCheckIsPlaceholderNodeTest(script string) bool {
	lower := strings.ToLower(script)
	return strings.Contains(lower, "no test specified") && strings.Contains(lower, "exit 1")
}

func agentCheckRegularFileExists(repoRoot, relPath string) bool {
	info, err := os.Lstat(filepath.Join(repoRoot, relPath))
	return err == nil && info.Mode().IsRegular()
}

type boundedAgentCheckBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedAgentCheckBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *boundedAgentCheckBuffer) String() string {
	return b.buf.String()
}

func (b *boundedAgentCheckBuffer) Truncated() bool {
	return b.truncated
}
