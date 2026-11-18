package githelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/bbpush"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// helperChunkingEnv turns the client-chunked push and the chunked fetch off
// when it holds a false value ("0", "false"). The push then always runs git
// send-pack, and the server chunks the large blobs. A fetch does not ask for
// recipes and gets whole blobs. entiredb's ENTIRE_BB_CLIENT_CHUNKING is the
// server's switch; the helper never reads it.
const helperChunkingEnv = "ENTIRE_HELPER_CHUNKING"

// helperChunkingOff reports whether helperChunkingEnv turns the
// client-chunked push and the chunked fetch off.
func helperChunkingOff() bool {
	v, ok := os.LookupEnv(helperChunkingEnv)
	if !ok {
		return false
	}
	on, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && !on
}

// helperPush runs a client-chunked push when the server advertises
// entire-bb and the push holds a large blob. It reports true when send-pack
// must not run: the push request went out, or the dry run finished. It
// reports false when it sent nothing, and the caller then pushes with
// send-pack. An unexpected reason for that gets one warning line on stderr,
// because the large blobs then go to the server whole.
func helperPush(ctx context.Context, t Transport, advertisement []byte, refspecs []string, opts *Options, stdout, stderr io.Writer) (bool, error) {
	if _, ok := bbpush.Threshold(advertisement); !ok {
		return false, nil
	}
	if helperChunkingOff() {
		debuglog.Printf("bbpush: %s turns client chunking off; pushing with send-pack", helperChunkingEnv)
		return false, nil
	}
	pushOpts, err := opts.bbPushOptions()
	if err != nil {
		debuglog.Printf("bbpush: %v; pushing with send-pack", err)
		return false, nil
	}
	var store *bbcache.Store
	err = bbpush.Push(ctx, bbpush.Config{
		Advertisement: advertisement,
		Refspecs:      refspecs,
		Options:       pushOpts,
		OpenStore: func() (*bbcache.Store, error) {
			s, err := openXorbStore(ctx, t)
			store = s
			return s, err
		},
		Client: bbclient.New(t),
		Agent:  Agent,
		Post: func(ctx context.Context, body *gitproto.RequestBody) (io.ReadCloser, error) {
			return t.ServiceRPC(ctx, serviceReceivePack, body.Reader(), receivePackHeaders(body))
		},
		Stdout: stdout,
		Stderr: stderr,
	})
	if store != nil {
		store.Trim()
	}
	var fallback *bbpush.FallbackError
	if !errors.As(err, &fallback) {
		return true, err //nolint:wrapcheck // bbpush names the stage
	}
	debuglog.Printf("bbpush: %v; pushing with send-pack", err)
	if !errors.Is(err, bbpush.ErrNoLargeBlobs) && !errors.Is(err, bbpush.ErrNeedsSendPack) {
		msg := strings.ReplaceAll(bbpush.Sanitize(err.Error()), "\n", " ")
		fmt.Fprintf(stderr, "warning: %s; sending large blobs whole\n", msg)
	}
	return false, nil
}

func openXorbStore(ctx context.Context, t Transport) (*bbcache.Store, error) {
	entryURL, repoPath := t.RepoLocation()
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // CommonDir names the stage
	}
	return bbcache.Open(commonDir, entryURL, repoPath) //nolint:wrapcheck // Open names the stage
}
