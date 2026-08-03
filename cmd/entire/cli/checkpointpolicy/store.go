package checkpointpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const PolicyFileName = "policy.json"

const maxPolicyFileBytes = 64 * 1024

const RefName = plumbing.ReferenceName("refs/entire/policies/checkpoint")

type Source string

const (
	SourceDefaults      Source = "defaults"
	SourceLocal         Source = "local"
	SourceRemote        Source = "remote"
	SourceLocalDiverged Source = "local-diverged"
)

type State struct {
	Policy     Policy
	Source     Source
	Hash       plumbing.Hash
	RemoteHash plumbing.Hash
}

func ReadLocal(ctx context.Context, repo *git.Repository) (State, error) {
	ref, err := repo.Reference(RefName, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return State{Source: SourceDefaults}, nil
		}
		return State{}, fmt.Errorf("read checkpoint policy ref: %w", err)
	}
	return readFromHash(ctx, repo, ref.Hash(), SourceLocal)
}

func ReadFromRef(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, source Source) (State, error) {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return State{}, fmt.Errorf("read checkpoint policy ref %s: %w", refName, err)
	}
	return readFromHash(ctx, repo, ref.Hash(), source)
}

func WriteLocal(ctx context.Context, repo *git.Repository, parent plumbing.Hash, policy Policy) (plumbing.Hash, error) {
	data, err := jsonutil.MarshalIndentWithNewline(policy, "", "  ")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("marshal checkpoint policy: %w", err)
	}
	blobHash, err := checkpoint.CreateBlobFromContent(repo, data)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("create checkpoint policy blob: %w", err)
	}
	treeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{
		PolicyFileName: {Name: PolicyFileName, Mode: filemode.Regular, Hash: blobHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("build checkpoint policy tree: %w", err)
	}
	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(repo)
	commitHash, err := checkpoint.CreateCommit(ctx, repo, treeHash, parent, "Update checkpoint policy", authorName, authorEmail)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("create checkpoint policy commit: %w", err)
	}
	if err := SetRef(repo, RefName, commitHash); err != nil {
		return plumbing.ZeroHash, err
	}
	return commitHash, nil
}

func SetRef(repo *git.Repository, ref plumbing.ReferenceName, hash plumbing.Hash) error {
	if err := repo.Storer.SetReference(plumbing.NewHashReference(ref, hash)); err != nil {
		return fmt.Errorf("set checkpoint policy ref %s: %w", ref, err)
	}
	return nil
}

func readFromHash(ctx context.Context, repo *git.Repository, hash plumbing.Hash, source Source) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, fmt.Errorf("read checkpoint policy: %w", err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return State{}, fmt.Errorf("read checkpoint policy commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return State{}, fmt.Errorf("read checkpoint policy tree: %w", err)
	}
	file, err := tree.File(PolicyFileName)
	if err != nil {
		return State{}, fmt.Errorf("read %s: %w", PolicyFileName, err)
	}
	if file.Size > maxPolicyFileBytes {
		return State{}, fmt.Errorf("parse %s: file exceeds %d bytes", PolicyFileName, maxPolicyFileBytes)
	}
	reader, err := file.Reader()
	if err != nil {
		return State{}, fmt.Errorf("read %s contents: %w", PolicyFileName, err)
	}
	defer reader.Close()

	var policy Policy
	decoder := json.NewDecoder(io.LimitReader(reader, maxPolicyFileBytes))
	if err := decoder.Decode(&policy); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", PolicyFileName, err)
	}
	var trailingValue json.RawMessage
	if err := decoder.Decode(&trailingValue); !errors.Is(err, io.EOF) {
		if err == nil {
			return State{}, fmt.Errorf("parse %s: multiple JSON values", PolicyFileName)
		}
		return State{}, fmt.Errorf("parse %s: %w", PolicyFileName, err)
	}
	return State{Policy: policy, Source: source, Hash: hash}, nil
}
