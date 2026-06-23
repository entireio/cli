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

const defaultBaseRemote = "origin"

const fetchRefName = plumbing.ReferenceName("refs/entire/policies/checkpoint-fetch")

var errStopTraversal = errors.New("stop traversal")

type Target struct {
	Remote string
	Label  string
	Dir    string
}

type RemoteState struct {
	Exists bool
	Hash   plumbing.Hash
}

func ResolveTarget(ctx context.Context, baseRemote string) (Target, error) {
	if baseRemote == "" {
		baseRemote = defaultBaseRemote
	}
	dir, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return Target{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	if target, dedicated, err := remote.ConfiguredURL(ctx, baseRemote, dir); err != nil {
		return Target{}, fmt.Errorf("resolve checkpoint remote URL: %w", err)
	} else if dedicated {
		return Target{Remote: target, Label: "checkpoint remote", Dir: dir}, nil
	}
	target, dedicated, err := remote.PushURL(ctx, baseRemote)
	if err != nil {
		return Target{}, fmt.Errorf("resolve checkpoint push URL: %w", err)
	}
	label := baseRemote
	if dedicated {
		label = "checkpoint remote"
	}
	return Target{Remote: target, Label: label, Dir: dir}, nil
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
	if len(fields[0]) != 40 {
		return RemoteState{}, fmt.Errorf("invalid remote checkpoint policy hash %q", fields[0])
	}
	return RemoteState{Exists: true, Hash: plumbing.NewHash(fields[0])}, nil
}

func Sync(ctx context.Context, repo *git.Repository, target Target) (State, error) {
	local, err := ReadLocal(ctx, repo)
	if err != nil {
		return State{}, err
	}

	remoteState, err := CheckRemote(ctx, target)
	if err != nil {
		return State{}, err
	}
	if !remoteState.Exists {
		return local, nil
	}
	if local.Hash == remoteState.Hash {
		local.Source = SourceRemote
		local.RemoteHash = remoteState.Hash
		return local, nil
	}

	fetched, err := fetchRemotePolicy(ctx, repo, target)
	if err != nil {
		return State{}, err
	}
	fetched.RemoteHash = remoteState.Hash
	defer removeFetchRef(repo)

	if local.Hash.IsZero() || isAncestorOf(ctx, repo, local.Hash, fetched.Hash) {
		if err := SetRef(repo, RefName, fetched.Hash); err != nil {
			return State{}, err
		}
		fetched.Source = SourceRemote
		return fetched, nil
	}

	local.Source = SourceLocalDiverged
	local.RemoteHash = remoteState.Hash
	local.Warning = fmt.Sprintf("local checkpoint policy %s diverges from remote %s", local.Hash, remoteState.Hash)
	return local, nil
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
	return ReadFromRef(ctx, repo, fetchRefName, SourceRemote)
}

func removeFetchRef(repo *git.Repository) {
	if err := repo.Storer.RemoveReference(fetchRefName); err != nil {
		return
	}
}

func isAncestorOf(ctx context.Context, repo *git.Repository, ancestor, target plumbing.Hash) bool {
	if ancestor == target {
		return true
	}

	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false
	}
	defer iter.Close()

	found := false
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("traverse checkpoint policy ancestry: %w", err)
		}
		if commit.Hash == ancestor {
			found = true
			return errStopTraversal
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopTraversal) {
		return false
	}
	return found
}
