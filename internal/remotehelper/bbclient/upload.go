package bbclient

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// UploadResult is the server's answer to one upload.
type UploadResult struct {
	Xorb xethash.Hash
	// State is StateStaged for a new upload, or StatePermanent when the
	// repo already held the xorb.
	State State
	// Chunks is the footer of the xorb in that state.
	Chunks []FooterChunk
}

type uploadResponse struct {
	Xorb   textHash    `json:"xorb"`
	State  State       `json:"state"`
	Chunks []wireChunk `json:"chunks"`
}

// Upload sends the network form of x to the repo. It retries a busy node and
// a transient failure. A refusal (400, 404, 413, 422) is a *StatusError at
// once.
func (c *Client) Upload(ctx context.Context, x *bbchunk.Xorb) (*UploadResult, error) {
	h := x.Hash
	if len(x.Network) == 0 || len(x.Network) > xorb.MaxNetworkSize {
		return nil, fmt.Errorf("bbclient: xorb %s is %d bytes, want 1 to %d", h, len(x.Network), xorb.MaxNetworkSize)
	}
	suffix := "/bb/xorbs/" + h.String()
	var wire uploadResponse
	if err := c.do(ctx, http.MethodPut, suffix, "application/octet-stream", x.Network, maxUploadResponse, &wire); err != nil {
		return nil, err
	}
	if xethash.Hash(wire.Xorb) != h {
		return nil, fmt.Errorf("bbclient: upload of xorb %s answered for xorb %s", h, xethash.Hash(wire.Xorb))
	}
	if wire.State != StateStaged && wire.State != StatePermanent {
		return nil, fmt.Errorf("bbclient: upload of xorb %s answered state %q", h, wire.State)
	}
	chunks, err := parseFooter(h, wire.Chunks)
	if err != nil {
		return nil, err
	}
	return &UploadResult{Xorb: h, State: wire.State, Chunks: chunks}, nil
}

// Uploader uploads xorbs in the background, at most MaxInFlight at once.
// The first failure cancels the other uploads.
type Uploader struct {
	c   *Client
	ctx context.Context
	g   *errgroup.Group

	mu      sync.Mutex
	results map[xethash.Hash]*UploadResult
}

// NewUploader returns an Uploader whose uploads stop when ctx ends.
func (c *Client) NewUploader(ctx context.Context) *Uploader {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(MaxInFlight)
	return &Uploader{c: c, ctx: gctx, g: g, results: map[xethash.Hash]*UploadResult{}}
}

// Add starts the upload of x and takes ownership of it. It blocks while
// MaxInFlight uploads run, so a producer cannot hold more than that many
// xorbs in flight. Add skips a xorb that it already took. After an upload
// fails, Add returns an error and starts nothing; Wait returns the cause.
func (u *Uploader) Add(x *bbchunk.Xorb) error {
	if err := u.ctx.Err(); err != nil {
		return fmt.Errorf("bbclient: uploads stopped: %w", err)
	}
	u.mu.Lock()
	_, seen := u.results[x.Hash]
	if !seen {
		u.results[x.Hash] = nil
	}
	u.mu.Unlock()
	if seen {
		return nil
	}
	u.g.Go(func() error {
		r, err := u.c.Upload(u.ctx, x)
		if err != nil {
			return err
		}
		u.mu.Lock()
		u.results[x.Hash] = r
		u.mu.Unlock()
		return nil
	})
	return nil
}

// Wait waits for every upload and returns the result of each xorb that the
// server took, and the first error. After an error the map holds only the
// uploads that finished, so a caller can still record them for the next
// push.
func (u *Uploader) Wait() (map[xethash.Hash]*UploadResult, error) {
	err := u.g.Wait()
	u.mu.Lock()
	defer u.mu.Unlock()
	done := make(map[xethash.Hash]*UploadResult, len(u.results))
	for h, r := range u.results {
		if r != nil {
			done[h] = r
		}
	}
	return done, err //nolint:wrapcheck // Upload names the xorb
}
