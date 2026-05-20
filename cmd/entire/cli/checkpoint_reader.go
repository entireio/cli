package cli

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	git "github.com/go-git/go-git/v6"
)

type committedCheckpointReaderStores struct {
	v1Store  *checkpoint.GitStore
	v2Store  *checkpoint.V2GitStore
	reader   checkpoint.CommittedListReader
	readMode checkpoint.CommittedReadMode
}

type committedCheckpointReaderOptions struct {
	blobFetcher    checkpoint.BlobFetchFunc
	fetchRemoteLog string
}

func committedCheckpointReadMode(ctx context.Context) checkpoint.CommittedReadMode {
	return checkpoint.CommittedReadModeForOptions(
		settings.IsCheckpointsV2Enabled(ctx),
		settings.CheckpointsVersion(ctx),
	)
}

func newCommittedCheckpointReader(ctx context.Context, repo *git.Repository, opts committedCheckpointReaderOptions) (*committedCheckpointReaderStores, error) {
	v1Store := checkpoint.NewGitStore(repo)
	if opts.blobFetcher != nil {
		v1Store.SetBlobFetcher(opts.blobFetcher)
	}

	readMode := committedCheckpointReadMode(ctx)
	var v2Store *checkpoint.V2GitStore
	if committedCheckpointReadUsesV2(readMode) {
		v2URL, err := remote.FetchURL(ctx)
		if err != nil {
			message := opts.fetchRemoteLog
			if message == "" {
				message = "checkpoint reader: using origin for v2 store fetch remote"
			}
			logging.Debug(ctx, message, slog.String("error", err.Error()))
			v2URL = ""
		}
		v2Store = checkpoint.NewV2GitStore(repo, v2URL)
		if opts.blobFetcher != nil {
			v2Store.SetBlobFetcher(opts.blobFetcher)
		}
	}

	reader, err := checkpoint.NewCommittedReader(v1Store, v2Store, readMode)
	if err != nil {
		return nil, err //nolint:wrapcheck // Caller adds command-specific context.
	}

	return &committedCheckpointReaderStores{
		v1Store:  v1Store,
		v2Store:  v2Store,
		reader:   reader,
		readMode: readMode,
	}, nil
}

func committedCheckpointReadUsesV2(mode checkpoint.CommittedReadMode) bool {
	return mode != checkpoint.CommittedReadV1
}
