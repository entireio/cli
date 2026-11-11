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

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/google/uuid"
)

const agentOutputLimit = 8 * 1024 * 1024
const nativeCloneScheme = "entire"

var errProjectAgentOutputLimit = errors.New("output exceeds 8 MiB")

var agentCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type projectAgentExecutor struct {
	// workspace is the cache-relative parent of per-run directories.
	workspace string
	git       string
	run       func(context.Context, string, string, []string, []string, string) ([]byte, error)
	review    func(context.Context, claudecode.RestrictedReview) (json.RawMessage, error)
}

func newProjectAgentExecutor(ctx context.Context) (*projectAgentExecutor, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("self-hosted execution currently requires macOS or Linux")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git is required: %w", err)
	}
	if !filepath.IsAbs(git) {
		return nil, errors.New("git must resolve to an absolute executable path")
	}
	reviewer, err := claudecode.NewRestrictedReviewer(ctx, runProjectAgentProcess)
	if err != nil {
		return nil, fmt.Errorf("prepare restricted reviewer: %w", err)
	}
	return &projectAgentExecutor{git: git, run: runProjectAgentProcess, review: reviewer.Review}, nil
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

func (e *projectAgentExecutor) execute(ctx context.Context, c projectAgentClaim) (json.RawMessage, error) {
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
	parent := e.workspace
	if parent == "" {
		parent = "agent-work"
	}
	work := parent + "/" + uuid.NewString()
	if err := osroot.MkdirAllNoSymlink(root, work+"/review", 0o700); err != nil {
		return nil, fmt.Errorf("create review directory: %w", err)
	}
	defer func() {
		if err := osroot.RemoveAllNoSymlinks(root, work); err != nil {
			logging.Warn(ctx, "remove project agent review workspace", "workspace", work, "error", err)
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
	if err := e.ensureCommits(ctx, repo, gitEnv, gitArgs, c.BaseSHA, c.HeadSHA); err != nil {
		return nil, err
	}
	checkout := append(append([]string{}, gitArgs...), "checkout", "--detach", c.HeadSHA, "--")
	if _, err := e.run(ctx, e.git, repo, gitEnv, checkout, ""); err != nil {
		return nil, fmt.Errorf("checkout failed: %w", err)
	}
	diff, diffNote, err := e.diff(ctx, repo, gitEnv, gitArgs, c.BaseSHA, c.HeadSHA)
	if err != nil {
		return nil, err
	}
	if err := osroot.WriteFile(root, work+"/review/change.diff", diff, 0o600); err != nil {
		return nil, fmt.Errorf("write review diff: %w", err)
	}
	reviewDir := filepath.Join(dir, "review")
	prompt := c.Prompt + "\nThe diff is in " + filepath.Join(reviewDir, "change.diff") + "." + diffNote + " You cannot run commands."
	result, err := e.review(ctx, claudecode.RestrictedReview{
		Directory: repo, ReadDir: reviewDir, Prompt: prompt, Model: c.Model, OutputSchema: c.OutputSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("restricted review: %w", err)
	}
	return result, nil
}

// ensureCommits fetches pinned commits the clone refspec missed (e.g. fork heads).
func (e *projectAgentExecutor) ensureCommits(ctx context.Context, repo string, env, gitArgs []string, shas ...string) error {
	var missing []string
	for _, sha := range shas {
		probe := append(append([]string{}, gitArgs...), "cat-file", "-e", sha+"^{commit}")
		if _, err := e.run(ctx, e.git, repo, env, probe, ""); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("fetch pinned commits: %w", ctx.Err())
			}
			missing = append(missing, sha)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	fetch := append(append(append([]string{}, gitArgs...), "fetch", "--no-tags", "origin"), missing...)
	if _, err := e.run(ctx, e.git, repo, env, fetch, ""); err != nil {
		return fmt.Errorf("fetch pinned commits: %w", err)
	}
	return nil
}

// diff falls back to --stat when the patch exceeds the output bound.
func (e *projectAgentExecutor) diff(ctx context.Context, repo string, env, gitArgs []string, base, head string) ([]byte, string, error) {
	args := func(format ...string) []string {
		a := append(append([]string{}, gitArgs...), "diff-tree", "--no-commit-id", "-r", "--no-ext-diff", "--no-textconv")
		return append(append(a, format...), base, head, "--")
	}
	out, err := e.run(ctx, e.git, repo, env, args("--stat", "--patch"), "")
	if errors.Is(err, errProjectAgentOutputLimit) {
		out, err = e.run(ctx, e.git, repo, env, args("--stat"), "")
		if err == nil {
			return out, " The full patch exceeds 8 MiB, so the diff file only lists changed files; read them in the checkout.", nil
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("diff failed: %w", err)
	}
	return out, "", nil
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
		if out.exceeded {
			return nil, fmt.Errorf("subprocess failed: %w", errProjectAgentOutputLimit)
		}
		return nil, errors.New("subprocess failed (output withheld)")
	}
	return out.Bytes(), nil
}

// Not embedded: a promoted ReadFrom would let io.Copy bypass Write's bound.
type projectAgentLimitedBuffer struct {
	buf      bytes.Buffer
	exceeded bool
}

func (b *projectAgentLimitedBuffer) Write(p []byte) (int, error) {
	if len(p) > agentOutputLimit-b.buf.Len() {
		b.exceeded = true
		return 0, errProjectAgentOutputLimit
	}
	n, _ := b.buf.Write(p) // bytes.Buffer.Write always succeeds.
	return n, nil
}

func (b *projectAgentLimitedBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *projectAgentLimitedBuffer) Len() int { return b.buf.Len() }
