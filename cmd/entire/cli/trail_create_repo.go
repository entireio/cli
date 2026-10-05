package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/spf13/cobra"
)

// trailBranchPresence is what a remote says about a trail's branch.
type trailBranchPresence int

const (
	trailBranchUnknown trailBranchPresence = iota
	trailBranchPresent
	trailBranchMissing
)

// trailBranchCheckTimeout bounds the whole remote branch check (resolving the
// repo's URL and asking it), like remoteHasBranch on the local create path, so
// an unreachable control plane or remote fails instead of hanging.
// A variable so tests can shorten it.
var trailBranchCheckTimeout = 30 * time.Second

// trailBranchCheckWaitDelay bounds how long the check waits for pipes after
// the deadline kills git (a transport child can hold them open). A variable so
// tests can shorten it.
var trailBranchCheckWaitDelay = execx.KillWaitDelay

// trailRemoteBranchState reports whether branch exists on the repository
// named by forge/owner/repo. It is a seam for tests.
var trailRemoteBranchState = remoteTrailBranchState

// runTrailCreateForRepo creates a trail on the repository --repo names,
// through the API alone. Unlike the local path it never opens a clone, never
// creates or pushes a branch, and never prompts: the branch must already exist
// on that repository, because the server would otherwise start one at the
// base tip and bind the trail to the wrong code.
func runTrailCreateForRepo(cmd *cobra.Command, repoArg, title, body, base, branch, statusStr, typeStr, priorityStr string, assignees []string, checkout, noBranch bool) error {
	ctx := cmd.Context()
	if checkout {
		return errors.New("cannot combine --repo with --checkout: --repo creates the trail remotely and leaves the local clone alone")
	}
	if err := validateTrailCreateEnums(cmd, typeStr, priorityStr); err != nil {
		return err
	}
	title, base, branch = strings.TrimSpace(title), strings.TrimSpace(base), strings.TrimSpace(branch)
	switch {
	case title == "":
		return errors.New("--repo requires --title")
	case base == "":
		return errors.New("--repo requires --base")
	case branch == "" && !noBranch:
		return errors.New("--repo requires --branch or --no-branch")
	}
	if statusStr == "" {
		statusStr = string(trail.StatusOpen)
	}
	if err := validateTrailCreateFields(ctx, title, branch, statusStr, noBranch); err != nil {
		return err
	}
	forge, owner, repoName, err := parseTrailRepoArg(repoArg)
	if err != nil {
		return err
	}
	ref := forge + "/" + owner + "/" + repoName
	if !noBranch {
		presence, err := trailRemoteBranchState(ctx, forge, owner, repoName, branch)
		switch {
		case presence == trailBranchMissing:
			return fmt.Errorf("branch %s does not exist on %s; push it first", branch, ref)
		case presence != trailBranchPresent:
			return fmt.Errorf("could not verify branch %s on %s: %w; push it first or check your access", branch, ref, err)
		}
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), forge, owner, repoName)
	if err != nil {
		return renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repoName, err)
	}
	basePath, err := trailRepoBasePath(forge, owner, repoName, repoID)
	if err != nil {
		return err
	}
	createResp, err := postTrailCreate(ctx, client, basePath, forge, owner, repoName, title, body, branch, base, statusStr, strings.TrimSpace(typeStr), strings.TrimSpace(priorityStr), assignees, false)
	if err != nil {
		return err
	}
	printCreatedTrail(cmd.OutOrStdout(), createResp.Trail, forge, owner, repoName)
	return nil
}

// remoteTrailBranchState asks the repository's git remote whether branch
// exists. An error of any kind (auth, network, unresolvable repo) is Unknown,
// never Missing: a private GitHub repo without credentials must not read as
// an absent branch.
func remoteTrailBranchState(ctx context.Context, forge, owner, repo, branch string) (trailBranchPresence, error) {
	ctx, cancel := context.WithTimeout(ctx, trailBranchCheckTimeout)
	defer cancel()
	url, err := resolveTrailRepoCloneURL(ctx, forge, owner, repo)
	if err != nil {
		if ctx.Err() != nil {
			return trailBranchUnknown, fmt.Errorf("resolve %s/%s/%s: no answer within %s: %w", forge, owner, repo, trailBranchCheckTimeout, ctx.Err())
		}
		return trailBranchUnknown, err
	}
	presence, err := trailLsRemote(ctx, url, branch)
	if presence != trailBranchUnknown || forge != gitremote.ForgeGitHub || ctx.Err() != nil {
		return presence, err
	}
	// The local path inherits the user's protocol from their remote; here the
	// URL is synthesized, so a user whose GitHub access is SSH-only cannot
	// answer over HTTPS. Ask over SSH before giving up.
	sshURL := "git@github.com:" + owner + "/" + repo + ".git"
	sshPresence, sshErr := trailLsRemote(ctx, sshURL, branch)
	if sshPresence != trailBranchUnknown {
		return sshPresence, sshErr
	}
	return trailBranchUnknown, fmt.Errorf("over HTTPS: %w; over SSH: %w", err, sshErr)
}

// resolveTrailRepoCloneURL and trailLsRemote are seams for tests.
var (
	resolveTrailRepoCloneURL = trailRepoCloneURL
	trailLsRemote            = lsRemoteBranch
)

// lsRemoteBranch asks url whether refs/heads/<branch> exists. It runs under
// the caller's context: remoteTrailBranchState owns the one deadline for the
// whole check, so the SSH fallback shares the same 30s rather than adding to it.
func lsRemoteBranch(ctx context.Context, url, branch string) (trailBranchPresence, error) {
	cmd := execx.NonInteractive(ctx, "git", "ls-remote", "--heads", url, "refs/heads/"+branch)
	// ls-remote runs the transport as a child that can outlive a killed git and
	// hold the pipes open; WaitDelay bounds that. (TerminateOnCancel's process
	// group cannot be combined with NonInteractive's new session.)
	cmd.WaitDelay = trailBranchCheckWaitDelay
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return trailBranchUnknown, fmt.Errorf("git ls-remote: no answer within %s: %w", trailBranchCheckTimeout, ctx.Err())
		}
		// git echoes the remote URL in its errors, and a URL can carry
		// credentials, so stderr is redacted per URL before it is surfaced.
		if msg := redactGitStderr(stderr.String()); msg != "" {
			return trailBranchUnknown, fmt.Errorf("git ls-remote: %s", msg)
		}
		return trailBranchUnknown, fmt.Errorf("git ls-remote: %w", err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return trailBranchMissing, nil
	}
	return trailBranchPresent, nil
}

// trailRepoCloneURL is the git URL for a --repo triple: the native repo's own
// entire:// coordinates from the control plane, or the GitHub HTTPS URL, which
// uses the caller's git credentials.
func trailRepoCloneURL(ctx context.Context, forge, owner, repo string) (string, error) {
	switch forge {
	case gitremote.ForgeNative:
		c, err := activeCoreClient(ctx)
		if err != nil {
			return "", fmt.Errorf("connect to Entire control plane: %w", err)
		}
		r, err := resolveNativeRepo(ctx, c, owner, repo)
		if err != nil {
			return "", err
		}
		u := repoRemoteURL(*r)
		if u == "" {
			return "", fmt.Errorf("%s/%s/%s has no clone coordinates yet", forge, owner, repo)
		}
		return u, nil
	case gitremote.ForgeGitHub:
		return "https://github.com/" + owner + "/" + repo + ".git", nil
	default:
		return "", fmt.Errorf("unsupported forge %q", forge)
	}
}
