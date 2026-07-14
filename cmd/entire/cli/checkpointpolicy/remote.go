package checkpointpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const (
	sha1HexSize   = 40
	sha256HexSize = 64
)

const fetchRefName = plumbing.ReferenceName("refs/entire/policies/checkpoint-fetch")

var errStopTraversal = errors.New("stop traversal")

type Target struct {
	Remote string
	Dir    string
}

type RemoteState struct {
	Exists bool
	Hash   plumbing.Hash
}

func ResolveTarget(ctx context.Context) (Target, error) {
	dir, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return Target{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	target, err := remote.FetchURL(ctx, remote.FetchURLOptions{WorktreeRoot: dir})
	if err != nil {
		return Target{}, fmt.Errorf("resolve checkpoint remote URL: %w", err)
	}
	return Target{Remote: target, Dir: dir}, nil
}

func CheckRemote(ctx context.Context, target Target) (RemoteState, error) {
	output, err := remote.LsRemoteInDir(ctx, target.Dir, target.Remote, RefName.String())
	if err != nil {
		return RemoteState{}, fmt.Errorf("check remote checkpoint policy ref: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return RemoteState{}, nil
	}
	hash, err := parseRemotePolicyHash(fields[0])
	if err != nil {
		return RemoteState{}, err
	}
	return RemoteState{Exists: true, Hash: hash}, nil
}

func Sync(ctx context.Context, repo *git.Repository, target Target) (State, error) {
	local, err := ReadLocal(ctx, repo)
	if err != nil {
		return State{}, err
	}

	baseline, remoteFound, err := remoteBaseline(ctx, repo, target, local)
	if err != nil {
		return State{}, err
	}
	if !remoteFound || local.Hash == baseline.Hash {
		return baseline, nil
	}

	if local.Hash.IsZero() {
		if err := SetRef(repo, RefName, baseline.Hash); err != nil {
			return State{}, err
		}
		baseline.Source = SourceRemote
		return baseline, nil
	}
	localAncestor, err := isAncestorOf(ctx, repo, local.Hash, baseline.Hash)
	if err != nil {
		return State{}, err
	}
	if localAncestor {
		if err := SetRef(repo, RefName, baseline.Hash); err != nil {
			return State{}, err
		}
		baseline.Source = SourceRemote
		return baseline, nil
	}

	baselineAncestor, err := isAncestorOf(ctx, repo, baseline.Hash, local.Hash)
	if err != nil {
		return State{}, err
	}
	if baselineAncestor {
		local.RemoteHash = baseline.RemoteHash
		return local, nil
	}

	local.Source = SourceLocalDiverged
	local.RemoteHash = baseline.RemoteHash
	return local, nil
}

func remoteBaseline(ctx context.Context, repo *git.Repository, target Target, local State) (State, bool, error) {
	remoteState, err := CheckRemote(ctx, target)
	if err != nil {
		return State{}, false, err
	}
	if !remoteState.Exists {
		return local, false, nil
	}
	if local.Hash == remoteState.Hash {
		local.Source = SourceRemote
		local.RemoteHash = remoteState.Hash
		return local, true, nil
	}

	fetched, err := fetchRemotePolicy(ctx, repo, target)
	if err != nil {
		return State{}, false, err
	}
	fetched.RemoteHash = remoteState.Hash
	return fetched, true, nil
}

func parseRemotePolicyHash(raw string) (plumbing.Hash, error) {
	if !isSupportedRemotePolicyHashLength(raw) {
		return plumbing.ZeroHash, fmt.Errorf("invalid remote checkpoint policy hash %q", raw)
	}
	hash, ok := plumbing.FromHex(raw)
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("invalid remote checkpoint policy hash %q", raw)
	}
	return hash, nil
}

func isSupportedRemotePolicyHashLength(raw string) bool {
	return len(raw) == sha1HexSize || len(raw) == sha256HexSize
}

func Push(ctx context.Context, target Target) error {
	refspec := RefName.String() + ":" + RefName.String()
	result, err := remote.PushWithOptions(ctx, remote.PushOptions{
		Remote:   target.Remote,
		RefSpecs: []string{refspec},
		Dir:      target.Dir,
	})
	if err != nil {
		output := strings.TrimSpace(result.Output)
		if output == "" {
			return fmt.Errorf("push checkpoint policy: %w", err)
		}
		return fmt.Errorf("push checkpoint policy: %s: %w", output, err)
	}
	return nil
}

func fetchRemotePolicy(ctx context.Context, repo *git.Repository, target Target) (State, error) {
	refspec := fmt.Sprintf("+%s:%s", RefName, fetchRefName)
	if _, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   target.Remote,
		RefSpecs: []string{refspec},
		NoTags:   true,
		NoFilter: true,
		Dir:      target.Dir,
	}); err != nil {
		return State{}, fmt.Errorf("fetch checkpoint policy ref: %w", err)
	}
	defer removeFetchRef(repo)
	return ReadFromRef(ctx, repo, fetchRefName, SourceRemote)
}

func removeFetchRef(repo *git.Repository) {
	if err := repo.Storer.RemoveReference(fetchRefName); err != nil {
		return
	}
}

func isAncestorOf(ctx context.Context, repo *git.Repository, ancestor, target plumbing.Hash) (bool, error) {
	if ancestor == target {
		return true, nil
	}

	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false, fmt.Errorf("open checkpoint policy ancestry: %w", err)
	}
	defer iter.Close()

	found := false
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("checkpoint policy ancestry context: %w", err)
		}
		if commit.Hash == ancestor {
			found = true
			return errStopTraversal
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopTraversal) {
		return false, fmt.Errorf("traverse checkpoint policy ancestry: %w", err)
	}
	return found, nil
}
