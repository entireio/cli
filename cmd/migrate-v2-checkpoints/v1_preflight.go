package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

const (
	v1RefFetchTimeout = 2 * time.Minute
	v1FetchTmpRef     = strategy.FetchTmpRefPrefix + "migrate-v1"
)

func ensureLatestV1Ref(ctx context.Context, repoRoot string, repo *git.Repository) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	fetchTarget, err := remote.FetchURL(ctx, remote.FetchURLOptions{WorktreeRoot: repoRoot})
	if err != nil {
		if localV1RefExists(repo) {
			return nil
		}
		return fmt.Errorf("resolve v1 checkpoint fetch target: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, v1RefFetchTimeout)
	defer cancel()

	remoteHasV1, err := remoteRefExists(ctx, repoRoot, fetchTarget, refName.String())
	if err != nil {
		return err
	}
	if !remoteHasV1 {
		return fmt.Errorf("%s not found on remote %s", refName, remote.RedactURL(fetchTarget))
	}

	if err := fetchV1Ref(ctx, repoRoot, repo, fetchTarget); err != nil {
		return err
	}
	return nil
}

func localV1RefExists(repo *git.Repository) bool {
	_, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	return err == nil
}

func remoteRefExists(ctx context.Context, repoRoot, fetchTarget, refName string) (bool, error) {
	output, err := remote.LsRemoteInDir(ctx, repoRoot, fetchTarget, refName)
	if err != nil {
		return false, fmt.Errorf("list remote %s from %s: %w", refName, remote.RedactURL(fetchTarget), err)
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == refName {
			return true, nil
		}
	}
	return false, nil
}

func fetchV1Ref(ctx context.Context, repoRoot string, repo *git.Repository, fetchTarget string) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	tmpRefName := plumbing.ReferenceName(v1FetchTmpRef)
	refSpec := fmt.Sprintf("+%s:%s", refName, tmpRefName)
	output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		NoFilter: true,
		Dir:      repoRoot,
	})
	if err != nil {
		return fetchV1RefError("fetch v1 checkpoint ref", fetchTarget, output, err)
	}

	defer func() { _ = repo.Storer.RemoveReference(tmpRefName) }() //nolint:errcheck // cleanup is best-effort

	tmpRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		return fmt.Errorf("v1 checkpoint ref not found after fetch (tmp ref %s missing): %w", tmpRefName, err)
	}
	if err := strategy.SafelyAdvanceLocalRef(ctx, repo, refName, tmpRef.Hash()); err != nil {
		return fmt.Errorf("advance local %s: %w", refName, err)
	}
	return nil
}

func fetchV1RefError(action, fetchTarget string, output []byte, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s", action, v1RefFetchTimeout)
	}

	redactedTarget := remote.RedactURL(fetchTarget)
	msg := strings.TrimSpace(strings.ReplaceAll(string(output), fetchTarget, redactedTarget))
	if msg != "" {
		return fmt.Errorf("%s from %s failed: %s: %w", action, redactedTarget, msg, err)
	}
	return fmt.Errorf("%s from %s failed: %w", action, redactedTarget, err)
}
