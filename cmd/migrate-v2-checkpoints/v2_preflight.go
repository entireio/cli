package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

const (
	v2RefFetchTimeout = 2 * time.Minute
	v2MainFetchTmpRef = strategy.FetchTmpRefPrefix + "migrate-v2-main"
)

func ensureLatestV2Refs(ctx context.Context, repoRoot string, repo *git.Repository) error {
	fetchTarget, err := remote.FetchURL(ctx, remote.FetchURLOptions{WorktreeRoot: repoRoot})
	if err != nil {
		if localV2MainRefExists(repo) {
			return nil
		}
		return fmt.Errorf("resolve v2 checkpoint fetch target: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, v2RefFetchTimeout)
	defer cancel()

	remoteRefs, err := listRemoteV2Refs(ctx, repoRoot, fetchTarget)
	if err != nil {
		return err
	}
	if _, ok := remoteRefs[paths.V2MainRefName]; !ok {
		return fmt.Errorf("%s not found on remote %s", paths.V2MainRefName, remote.RedactURL(fetchTarget))
	}

	if err := fetchV2MainRef(ctx, repoRoot, repo, fetchTarget); err != nil {
		return err
	}
	if err := fetchV2FullRefs(ctx, repoRoot, fetchTarget, remoteRefs); err != nil {
		return err
	}
	return nil
}

func localV2MainRefExists(repo *git.Repository) bool {
	_, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	return err == nil
}

func listRemoteV2Refs(ctx context.Context, repoRoot, fetchTarget string) (map[string]struct{}, error) {
	output, err := remote.LsRemoteInDir(ctx, repoRoot, fetchTarget, "refs/entire/checkpoints/v2/*")
	if err != nil {
		return nil, fmt.Errorf("list remote v2 checkpoint refs from %s: %w", remote.RedactURL(fetchTarget), err)
	}

	refs := make(map[string]struct{})
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		refs[fields[1]] = struct{}{}
	}
	return refs, nil
}

func fetchV2MainRef(ctx context.Context, repoRoot string, repo *git.Repository, fetchTarget string) error {
	refSpec := fmt.Sprintf("+%s:%s", paths.V2MainRefName, v2MainFetchTmpRef)
	output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		NoFilter: true,
		Dir:      repoRoot,
	})
	if err != nil {
		return fetchV2RefsError("fetch v2 /main", fetchTarget, output, err)
	}

	tmpRefName := plumbing.ReferenceName(v2MainFetchTmpRef)
	defer func() { _ = repo.Storer.RemoveReference(tmpRefName) }() //nolint:errcheck // cleanup is best-effort

	tmpRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		return fmt.Errorf("v2 /main not found after fetch (tmp ref %s missing): %w", tmpRefName, err)
	}
	if err := strategy.SafelyAdvanceLocalRef(ctx, repo, plumbing.ReferenceName(paths.V2MainRefName), tmpRef.Hash()); err != nil {
		return fmt.Errorf("advance local %s: %w", paths.V2MainRefName, err)
	}
	return nil
}

func fetchV2FullRefs(ctx context.Context, repoRoot, fetchTarget string, remoteRefs map[string]struct{}) error {
	refSpecs := v2FullRefSpecs(remoteRefs)
	if len(refSpecs) == 0 {
		return nil
	}

	output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: refSpecs,
		NoTags:   true,
		NoFilter: true,
		Dir:      repoRoot,
	})
	if err != nil {
		return fetchV2RefsError("fetch v2 /full refs", fetchTarget, output, err)
	}
	return nil
}

func v2FullRefSpecs(remoteRefs map[string]struct{}) []string {
	refSpecs := make([]string, 0, len(remoteRefs))
	for refName := range remoteRefs {
		if !isV2FullRefName(refName) {
			continue
		}
		refSpec := refName + ":" + refName
		if refName == paths.V2FullCurrentRefName {
			refSpec = "+" + refSpec
		}
		refSpecs = append(refSpecs, refSpec)
	}
	sort.Strings(refSpecs)
	return refSpecs
}

func isV2FullRefName(refName string) bool {
	prefix := strings.TrimSuffix(paths.V2FullCurrentRefName, "current")
	if !strings.HasPrefix(refName, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(refName, prefix)
	if suffix == "current" {
		return true
	}
	if len(suffix) != 13 {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func fetchV2RefsError(action, fetchTarget string, output []byte, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s", action, v2RefFetchTimeout)
	}

	redactedTarget := remote.RedactURL(fetchTarget)
	msg := strings.TrimSpace(strings.ReplaceAll(string(output), fetchTarget, redactedTarget))
	if msg != "" {
		return fmt.Errorf("%s from %s failed: %s: %w", action, redactedTarget, msg, err)
	}
	return fmt.Errorf("%s from %s failed: %w", action, redactedTarget, err)
}
