package githelper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"

	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/bbfetch"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// featureBBFetch is the fetch feature that entiredb advertises, and the
// fetch argument that asks it for chunked recipes instead of large blobs.
// The contract is entiredb's docs/specs/client-chunked-fetch.md.
const featureBBFetch = "entire-bb-fetch"

// errLargeFileFetch is the error of a fetch that failed in the recipe path.
// The helper has already sent the details to git on band 3, so the error
// that it returns does not repeat them.
var errLargeFileFetch = errors.New("large file download failed; see the message above")

// chunkedFetch is the client-chunked fetch of one stateless-connect session.
// It adds featureBBFetch to each fetch command, and rebuilds the large blobs
// of each pack before git reads them.
type chunkedFetch struct {
	t      Transport
	store  *bbcache.Store
	client *bbclient.Client
	record *bbcache.FallbackRecord
	// off is set when the fetch must not opt in: the cache is not writable,
	// a fallback record of the repo is valid or cannot be read, the server
	// refused the argument, or a fetch failed in the recipe path. The rest
	// of the session fetches whole blobs.
	off bool
}

// newChunkedFetch returns the chunked fetch of the session, or nil when the
// server does not advertise featureBBFetch or the kill switch is set.
func newChunkedFetch(t Transport, advertisement []byte) *chunkedFetch {
	if helperChunkingOff() || !gitproto.HasV2FetchFeature(advertisement, featureBBFetch) {
		return nil
	}
	return &chunkedFetch{t: t}
}

// handles reports whether the chunked fetch takes the request with the
// command command. It opens the cache at the first fetch command, so a
// session without one, such as ls-remote, never touches it.
func (c *chunkedFetch) handles(ctx context.Context, command string) bool {
	if c == nil || c.off || command != "fetch" {
		return false
	}
	if c.store == nil {
		c.off = !c.open(ctx)
	}
	return !c.off
}

// open opens the cache, and reports whether the fetch may opt in.
func (c *chunkedFetch) open(ctx context.Context) bool {
	store, err := openXorbStore(ctx, c.t)
	if err == nil {
		err = store.CheckWritable()
	}
	if err != nil {
		debuglog.Printf("bbfetch: %v; fetching whole blobs", err)
		return false
	}
	entryURL, repoPath := c.t.RepoLocation()
	record, err := bbcache.OpenFallbackRecord(entryURL, repoPath)
	if err != nil {
		// Without the record, a failure would repeat on every fetch.
		debuglog.Printf("bbfetch: %v; fetching whole blobs", err)
		return false
	}
	c.store, c.client, c.record = store, bbclient.New(c.t), record
	if fb, ok := record.Get(); ok {
		debuglog.Printf("bbfetch: fallback record until %s (%s); fetching whole blobs", fb.Until.Format(time.DateTime), fb.Reason)
		return false
	}
	return true
}

// close trims the cache once the session ends. It keeps the xorbs that the
// session pinned.
func (c *chunkedFetch) close() {
	if c != nil && c.store != nil {
		c.store.Trim()
	}
}

// fallback records that the next fetches of the repo get whole blobs, and
// stops the opt-in for the rest of the session.
func (c *chunkedFetch) fallback(reason string) {
	c.off = true
	if err := c.record.Set(reason); err != nil {
		debuglog.Printf("bbfetch: %v", err)
	}
}

// fetch sends the fetch request reqBody with featureBBFetch and copies the
// response to stdout, with each recipe replaced by its blob. When the server
// refuses the argument before it sends anything else, fetch sends the
// request again without it.
func (c *chunkedFetch) fetch(ctx context.Context, reqBody []byte, stdout io.Writer) error {
	req, err := gitproto.ParseV2Request(reqBody)
	if err != nil {
		return fmt.Errorf("stateless-connect: parse fetch request: %w", err)
	}
	objectFormat, ok := req.Capability("object-format")
	if !ok {
		objectFormat = "sha1"
	}
	withArg, err := gitproto.AddV2Argument(reqBody, featureBBFetch)
	if err != nil {
		return fmt.Errorf("stateless-connect: add %s: %w", featureBBFetch, err)
	}
	resp, err := c.t.UploadPackChunked(ctx, bytes.NewReader(withArg), protocolV2Header)
	var status *transport.HTTPStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusInternalServerError {
		// A node from before the fetch spec, or a node that failed. The
		// session stops the opt-in; the next one tries again.
		debuglog.Printf("bbfetch: the server answered HTTP 500 to %s: %v; fetching whole blobs in this session", featureBBFetch, err)
		c.off = true
		return c.resend(ctx, reqBody, stdout)
	}
	if err != nil {
		return fmt.Errorf("stateless-connect: POST: %w", err)
	}
	body := bufio.NewReaderSize(resp, pktline.MaxSize)
	if refusal, ok := argumentRefusal(body); ok {
		_ = resp.Close()
		debuglog.Printf("bbfetch: the server refused %s: %s", featureBBFetch, refusal)
		c.fallback("the server refused " + featureBBFetch + ": " + refusal)
		return c.resend(ctx, reqBody, stdout)
	}
	defer resp.Close()

	var stats *bbfetch.Stats
	reported := false
	err = gitproto.CopyFetchResponse(stdout, body, req.HasArgument("sideband-all"), func(data io.Reader, out *gitproto.SidebandWriter) error {
		var err error
		stats, err = bbfetch.RewritePack(ctx, bbfetch.Config{
			Store: c.store, Client: c.client, ObjectFormat: objectFormat, Quiet: req.HasArgument("no-progress"),
		}, data, out)
		var re *bbfetch.RecipeError
		if errors.As(err, &re) && ctx.Err() == nil {
			c.fallback(re.Error())
			reported = c.report(out, re)
		}
		return err //nolint:wrapcheck // the caller names the stage
	})
	if stats != nil && stats.Recipes > 0 {
		debuglog.Printf("bbfetch: %d recipes, %d bytes; %d xorbs: %d cached, %d from pushes, %d downloaded (%d bytes), %d corrupt",
			stats.Recipes, stats.LogicalBytes, stats.Xorbs, stats.Cached, stats.Converted, stats.Downloaded, stats.DownloadedBytes, stats.Corrupt)
	}
	if err != nil && reported {
		debuglog.Printf("bbfetch: %v", err)
		return errLargeFileFetch
	}
	if err != nil {
		return fmt.Errorf("stateless-connect: fetch response: %w", err)
	}
	return nil
}

// report sends the failure re to git on band 3, and reports whether the
// write worked. Git shows the text and stops the fetch or the clone.
func (c *chunkedFetch) report(out *gitproto.SidebandWriter, re *bbfetch.RecipeError) bool {
	msg := "entire: large file download failed: " + strings.ReplaceAll(re.Error(), "\n", " ") +
		". Clones and fetches of this repository get whole files for the next 24 hours; run the command again." +
		" Set " + helperChunkingEnv + "=0 to turn large file downloads off."
	if err := out.Error(gitproto.AppendSanitized(nil, []byte(msg))); err != nil {
		debuglog.Printf("bbfetch: report the failure to git: %v", err)
		return false
	}
	return true
}

// resend sends reqBody, the request without featureBBFetch, as a plain
// fetch. Nothing reached git yet.
func (c *chunkedFetch) resend(ctx context.Context, reqBody []byte, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // the cancellation is the whole story
	}
	resp, err := c.t.ServiceRPC(ctx, serviceUploadPack, bytes.NewReader(reqBody), protocolV2Header)
	if err != nil {
		return fmt.Errorf("stateless-connect: POST: %w", err)
	}
	return copyResponse(stdout, resp)
}

// argumentRefusal reports whether the response in body starts with the
// in-band refusal of featureBBFetch, and returns its text. It does not
// consume the response.
func argumentRefusal(body *bufio.Reader) (string, bool) {
	head, err := body.Peek(pktline.LenSize)
	if err != nil {
		return "", false
	}
	n, err := pktline.ParseLength(head)
	if err != nil || n <= pktline.LenSize {
		return "", false
	}
	line, err := body.Peek(n)
	if err != nil {
		return "", false
	}
	msg, ok := bytes.CutPrefix(line[pktline.LenSize:], []byte("ERR "))
	if !ok || !bytes.Contains(msg, []byte(featureBBFetch)) {
		return "", false
	}
	return string(gitproto.AppendSanitized(nil, bytes.TrimRight(msg, "\n"))), true
}

func protocolV2Header(req *http.Request) {
	req.Header.Set("Git-Protocol", "version=2")
}

// copyResponse copies a stateless response to stdout and closes it.
func copyResponse(stdout io.Writer, resp io.ReadCloser) error {
	if _, err := io.Copy(stdout, resp); err != nil {
		_ = resp.Close()
		return fmt.Errorf("streaming stateless response: %w", err)
	}
	if err := resp.Close(); err != nil {
		return fmt.Errorf("closing stateless response: %w", err)
	}
	return nil
}
