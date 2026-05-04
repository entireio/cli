package strategy

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

const (
	repairAuthorName  = "Entire Migration"
	repairAuthorEmail = "migration@entire.dev"
)

// RepairV2GenerationMetadataResult describes archived v2 generation metadata
// repair work performed by RepairV2GenerationMetadata.
type RepairV2GenerationMetadataResult struct {
	Repaired []string
	Skipped  []string
	Failed   []string
	Warnings []string
}

// RepairV2GenerationMetadata rewrites generation.json for archived v2 /full/*
// generation refs using the timestamp envelope from raw transcripts. Remote
// archived refs are repaired with force-with-lease when they exist on the
// checkpoint remote.
func RepairV2GenerationMetadata(ctx context.Context) (*RepairV2GenerationMetadataResult, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	store := checkpoint.NewV2GitStore(repo, "origin")
	return repairV2GenerationMetadata(ctx, repo, store)
}

func repairV2GenerationMetadata(ctx context.Context, repo *git.Repository, store *checkpoint.V2GitStore) (*RepairV2GenerationMetadataResult, error) {
	candidates, tempRefs, warnings, err := listArchivedV2GenerationCandidates(ctx, repo, store)
	if err != nil {
		return nil, fmt.Errorf("failed to list archived generations: %w", err)
	}
	defer removeTempRefs(repo, tempRefs)

	result := &RepairV2GenerationMetadataResult{
		Warnings: warnings,
	}

	pushTarget := &repairPushTarget{}

	for _, candidate := range candidates {
		repaired, repairErr := repairOneV2GenerationMetadata(ctx, repo, store, candidate, pushTarget)
		if repairErr != nil {
			result.Failed = append(result.Failed, candidate.Name)
			result.Warnings = append(result.Warnings, fmt.Sprintf("generation %s: %v", candidate.Name, repairErr))
			continue
		}
		if repaired {
			result.Repaired = append(result.Repaired, candidate.Name)
		} else {
			result.Skipped = append(result.Skipped, candidate.Name)
		}
	}

	return result, nil
}

func repairOneV2GenerationMetadata(
	ctx context.Context,
	repo *git.Repository,
	store *checkpoint.V2GitStore,
	candidate archivedV2GenerationCandidate,
	pushTarget *repairPushTarget,
) (bool, error) {
	oldCommitHash, treeHash, refErr := store.GetRefState(candidate.RefName)
	if refErr != nil {
		return false, fmt.Errorf("cannot read ref: %w", refErr)
	}

	gen, found, timestampErr := store.ComputeGenerationTimestampsFromTrees(treeHash, nil)
	if timestampErr != nil {
		return false, fmt.Errorf("failed to compute raw transcript timestamps: %w", timestampErr)
	}
	if !found {
		gen, found, timestampErr = store.ComputeGenerationCheckpointTimestamps(treeHash)
		if timestampErr != nil {
			return false, fmt.Errorf("failed to compute checkpoint timestamps: %w", timestampErr)
		}
		if !found {
			return false, nil
		}
	}

	current, genErr := store.ReadGeneration(treeHash)
	if genErr != nil {
		return false, fmt.Errorf("failed to read generation.json: %w", genErr)
	}
	if generationMetadataEqual(current, gen) {
		return false, nil
	}

	newTreeHash, addErr := store.AddGenerationJSONToTree(treeHash, gen)
	if addErr != nil {
		return false, fmt.Errorf("failed to rewrite generation.json: %w", addErr)
	}
	if newTreeHash == treeHash {
		return false, nil
	}

	newCommitHash, commitErr := checkpoint.CreateCommit(ctx, repo, newTreeHash, oldCommitHash,
		fmt.Sprintf("Repair generation metadata: %s\n", candidate.Name),
		repairAuthorName, repairAuthorEmail)
	if commitErr != nil {
		return false, fmt.Errorf("failed to create repair commit: %w", commitErr)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(candidate.RefName, newCommitHash)); err != nil {
		return false, fmt.Errorf("failed to update ref %s: %w", candidate.RefName, err)
	}

	if !candidate.HasRemote {
		return true, nil
	}

	target, err := pushTarget.resolve(ctx)
	if err != nil {
		rollbackRepairLocalRef(repo, candidate.RefName, oldCommitHash)
		return false, fmt.Errorf("failed to resolve remote for generation metadata repair push: %w", err)
	}
	if target == "" {
		rollbackRepairLocalRef(repo, candidate.RefName, oldCommitHash)
		return false, errors.New("no push target available for remote generation metadata repair")
	}

	remoteRefName := paths.V2FullRefPrefix + candidate.Name
	if err := pushRepairedV2Generation(ctx, target, candidate.RefName.String(), remoteRefName, candidate.RefOID); err != nil {
		rollbackRepairLocalRef(repo, candidate.RefName, oldCommitHash)
		return false, err
	}
	return true, nil
}

// rollbackRepairLocalRef restores the local ref to its pre-repair commit when
// the remote push fails, so local does not silently diverge from origin.
func rollbackRepairLocalRef(repo *git.Repository, refName plumbing.ReferenceName, oldCommitHash plumbing.Hash) {
	_ = repo.Storer.SetReference(plumbing.NewHashReference(refName, oldCommitHash)) //nolint:errcheck // best-effort rollback; the original push error is what we report
}

// repairPushTarget memoizes the push URL lookup so the remote is resolved at
// most once, only when a candidate actually needs to push.
type repairPushTarget struct {
	resolved bool
	target   string
	err      error
}

func (r *repairPushTarget) resolve(ctx context.Context) (string, error) {
	if r.resolved {
		return r.target, r.err
	}
	r.resolved = true
	target, _, err := remote.PushURL(ctx, "origin")
	if err != nil {
		r.err = fmt.Errorf("push URL: %w", err)
		return "", r.err
	}
	r.target = target
	return target, nil
}

func pushRepairedV2Generation(ctx context.Context, target, sourceRef, remoteRef, expectedOID string) error {
	return pushWithLease(ctx, target, sourceRef+":"+remoteRef, remoteRef, expectedOID,
		"push repaired generation ref "+remoteRef)
}

func generationMetadataEqual(left, right checkpoint.GenerationMetadata) bool {
	return left.OldestCheckpointAt.Equal(right.OldestCheckpointAt) &&
		left.NewestCheckpointAt.Equal(right.NewestCheckpointAt)
}
