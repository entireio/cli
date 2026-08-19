package checkpointpolicy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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

	// SkipLocalUpdate marks a read-only (legacy-tier) candidate: a policy
	// baseline found on it may be read, compared, and reported, but never
	// advances the local policy ref via SetRef. Push targets and the elected
	// checkpoint sync remote leave this false. The zero value preserves the
	// historical single-target Sync behavior (pre-push hook path).
	SkipLocalUpdate bool
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
	return SyncFrom(ctx, repo, []Target{target})
}

// SyncFrom reconciles the local checkpoint policy ref against the first
// target that has a policy ref. Targets are tried in order; a target lacking
// the policy ref and a target failing at the transport level both advance to
// the next target (failures logged at debug); when no target yields a
// baseline and at least one failed, the FIRST failure is surfaced. A winning
// target marked SkipLocalUpdate contributes a read-only baseline: it is
// compared and reported but never advances the local policy ref.
func SyncFrom(ctx context.Context, repo *git.Repository, targets []Target) (State, error) {
	local, err := ReadLocal(ctx, repo)
	if err != nil {
		return State{}, err
	}

	winner, baseline, remoteFound, err := findRemoteBaseline(ctx, repo, targets, local)
	if err != nil {
		return State{}, err
	}
	if !remoteFound {
		return local, nil
	}
	if local.Hash == baseline.Hash {
		return baseline, nil
	}

	// Legacy-tier baseline (SkipLocalUpdate): read/compare/report only — the
	// local policy ref advances only from the elected remote (or a dedicated
	// checkpoint_remote). Because the local ref did not move, the ENFORCED
	// policy is still the local one, and that is what gets reported:
	// returning the baseline as Source=remote here would print a policy the
	// hooks never enforce. The unadopted baseline stays visible via
	// RemoteHash so divergence can be surfaced.
	if local.Hash.IsZero() {
		if winner.SkipLocalUpdate {
			local.RemoteHash = baseline.RemoteHash
			return local, nil
		}
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
		if winner.SkipLocalUpdate {
			local.RemoteHash = baseline.RemoteHash
			return local, nil
		}
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

// remoteBaseline resolves the policy baseline from a single target, for the
// write flow (Update), which always operates against the push target.
func remoteBaseline(ctx context.Context, repo *git.Repository, target Target, local State) (State, bool, error) {
	_, baseline, found, err := findRemoteBaseline(ctx, repo, []Target{target}, local)
	if err != nil {
		return State{}, false, err
	}
	if !found {
		return local, false, nil
	}
	return baseline, true, nil
}

// findRemoteBaseline iterates targets in order and returns the first target
// that has a policy ref together with its resolved baseline. A target whose
// probe or fetch fails advances to the next target; the first such error is
// surfaced only when no target yields a baseline. found is false (with a nil
// error) when every target was reachable and none has the policy ref.
func findRemoteBaseline(ctx context.Context, repo *git.Repository, targets []Target, local State) (Target, State, bool, error) {
	var firstErr error
	for _, target := range targets {
		remoteState, err := CheckRemote(ctx, target)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			logging.Debug(ctx, "checkpoint policy: read candidate probe failed; trying next candidate",
				slog.String("candidate", remote.RedactURLOrPath(target.Remote)),
				slog.String("error", err.Error()))
			continue
		}
		if !remoteState.Exists {
			continue
		}
		if local.Hash == remoteState.Hash {
			baseline := local
			baseline.Source = SourceRemote
			baseline.RemoteHash = remoteState.Hash
			return target, baseline, true, nil
		}
		fetched, err := fetchRemotePolicy(ctx, repo, target)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			logging.Debug(ctx, "checkpoint policy: read candidate fetch failed; trying next candidate",
				slog.String("candidate", remote.RedactURLOrPath(target.Remote)),
				slog.String("error", err.Error()))
			continue
		}
		fetched.RemoteHash = remoteState.Hash
		return target, fetched, true, nil
	}
	if firstErr != nil {
		return Target{}, State{}, false, firstErr
	}
	return Target{}, State{}, false, nil
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
