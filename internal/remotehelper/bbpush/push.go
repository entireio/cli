// Package bbpush builds a client-chunked push for git-remote-entire, as
// entiredb's docs/specs/client-chunked-push.md defines it. When the server
// advertises entire-bb, the helper chunks each large blob, uploads the xorbs
// that the repo lacks, and sends a pack in which each large blob is a meta
// 0x03 entry (its recipe) instead of its bytes.
//
// The package does what git send-pack does for such a push: it plans the ref
// updates, writes the request, posts it and prints send-pack's helper status
// lines. Every failure before the post is a *FallbackError: the caller then
// pushes with send-pack, so a push never fails because this path could not
// run. After the post, failures are final.
package bbpush

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jmwoliver/xet-go/xethash"
	"golang.org/x/term"

	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// ErrNoLargeBlobs reports a push that holds no blob at or over the
// threshold. send-pack sends it as it is.
var ErrNoLargeBlobs = errors.New("no large blobs in the push")

// ErrNeedsSendPack reports a push that asks for a feature that only
// send-pack implements, such as a signed push.
var ErrNeedsSendPack = errors.New("the push needs send-pack")

// maxPushXorbs is the largest number of distinct xorbs that the entries of
// one push may name. entiredb refuses a push that names more.
const maxPushXorbs = 4096

// FallbackError is a failure before the push request went out. The caller
// pushes with send-pack instead.
type FallbackError struct {
	Err error
}

func (e *FallbackError) Error() string { return "client-chunked push not used: " + e.Err.Error() }

func (e *FallbackError) Unwrap() error { return e.Err }

// Config is one push.
type Config struct {
	// Advertisement is the receive-pack ref advertisement that git's "list
	// for-push" saw, without the service announcement.
	Advertisement []byte
	// Refspecs are the "push" lines of the batch, without "push ".
	Refspecs []string
	Options  Options
	// OpenStore opens the local xorb store of the repo. Push calls it only
	// for a push that holds a large blob.
	OpenStore func() (*bbcache.Store, error)
	Client    *bbclient.Client
	// Agent is the helper's agent, which the request appends to git's.
	Agent string
	// Post sends a receive-pack request and returns the response body. The
	// body is replayable.
	Post func(context.Context, *gitproto.RequestBody) (io.ReadCloser, error)
	// Stdout takes the helper status lines and the blank line after them.
	Stdout io.Writer
	Stderr io.Writer
}

// Push runs one push. It returns a *FallbackError when it sent no push
// request, unless ctx ended. After it printed the status lines, it returns
// an error when a ref failed, as send-pack exits non-zero then. When the
// report-status does not arrive, it prints "expecting report" for each
// command and returns the error, as send-pack does.
func Push(ctx context.Context, cfg Config) error {
	p, err := prepare(ctx, &cfg)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return &FallbackError{Err: err}
	}
	if cfg.Options.DryRun {
		return finish(&cfg, p, nil)
	}
	rep, err := p.post(ctx, &cfg)
	if err != nil {
		return p.failedReport(&cfg, err)
	}
	if rep.missingXorb() {
		debuglog.Printf("bbpush: server reported a missing xorb (%s); uploading again", Sanitize(rep.unpack))
		if err := p.repairMissing(ctx, &cfg); err != nil {
			debuglog.Printf("bbpush: upload missing xorbs: %v", err)
		} else if rep, err = p.post(ctx, &cfg); err != nil {
			return p.failedReport(&cfg, err)
		}
	}
	return finish(&cfg, p, rep)
}

// failedReport handles a post whose report-status did not arrive. When the
// request went out, the server may have updated refs, so it writes the
// status lines that send-pack writes then: "expecting report" for each
// command.
func (p *prepared) failedReport(cfg *Config, err error) error {
	var noReport *reportError
	if !errors.As(err, &noReport) {
		return err
	}
	if werr := writeStatusLines(cfg, p, &report{refs: map[string]refResult{}}); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

func finish(cfg *Config, p *prepared, rep *report) error {
	if rep != nil && rep.unpack != "ok" {
		fmt.Fprintf(cfg.Stderr, "error: remote unpack failed: %s\n", Sanitize(rep.unpack))
	}
	if err := writeStatusLines(cfg, p, rep); err != nil {
		return err
	}
	if rep != nil && rep.failed(p.cmds) {
		return errors.New("the remote refused some refs")
	}
	return nil
}

// writeStatusLines writes the helper status lines and the blank line after
// them.
func writeStatusLines(cfg *Config, p *prepared, rep *report) error {
	if err := writeStatus(cfg.Stdout, p.adv, p.cmds, rep); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cfg.Stdout); err != nil {
		return fmt.Errorf("write push terminator: %w", err)
	}
	return nil
}

// isTerminal reports whether w is a terminal. Only an *os.File can be one.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// prepared is a push that is ready to post.
type prepared struct {
	adv      *advertisement
	cmds     []command
	sideband bool
	store    *bbcache.Store
	body     *gitproto.RequestBody
	// xorbs lists every xorb that the entries name.
	xorbs []xethash.Hash
	// summary is the line that tells the user what the chunking and the
	// uploads did.
	summary string
}

func prepare(ctx context.Context, cfg *Config) (*prepared, error) {
	adv, err := parseAdvertisement(cfg.Advertisement)
	if err != nil {
		return nil, err
	}
	threshold, ok := adv.threshold()
	if !ok {
		return nil, errors.New("the remote does not advertise entire-bb")
	}
	repo, err := openLocalRepo(ctx)
	if err != nil {
		return nil, err
	}
	cmds, err := planCommands(ctx, repo, adv, cfg.Refspecs, cfg.Options)
	if err != nil {
		return nil, err
	}
	interactive := isTerminal(cfg.Stderr)
	caps, err := requestCaps(adv, repo, cfg.Options, !interactive)
	if err != nil {
		return nil, err
	}
	negatives, err := localNegatives(ctx, adv)
	if err != nil {
		return nil, err
	}
	revs := revisions(cmds, repo.zeroOID(), negatives)
	blobs, err := findLargeBlobs(ctx, revs, threshold, cfg.Stderr)
	if err != nil {
		return nil, err
	}
	if len(blobs) == 0 {
		return nil, ErrNoLargeBlobs
	}
	p := &prepared{adv: adv, cmds: cmds, sideband: adv.supports("side-band-64k")}
	if cfg.Options.DryRun {
		return p, nil
	}
	entries, err := p.chunkAndUpload(ctx, cfg, blobs)
	if err != nil {
		return nil, err
	}
	// cat-file has exited, so no git process writes to stderr now.
	if !cfg.Options.Quiet {
		fmt.Fprintln(cfg.Stderr, p.summary)
	}
	section, err := commandSection(cmds, caps, cfg.Options.PushOptions)
	if err != nil {
		return nil, err
	}
	p.body = &gitproto.RequestBody{}
	p.body.Append(section)
	if err := gitproto.AppendAgentToReceivePackBody(p.body, cfg.Agent); err != nil {
		return nil, fmt.Errorf("append agent: %w", err)
	}
	err = appendPack(ctx, p.body, packSpec{
		revs:      revs,
		threshold: threshold,
		ofsDelta:  adv.supports("ofs-delta"),
		progress:  interactive,
		stderr:    cfg.Stderr,
		entries:   entries,
		newHash:   newPackHash(repo.objectFormat),
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// chunkAndUpload reads each large blob twice: once for its chunk hashes,
// which the dedup query needs, and once to build its recipe and new xorbs.
// It uploads the new xorbs and returns the pack entries.
func (p *prepared) chunkAndUpload(ctx context.Context, cfg *Config, blobs []largeBlob) ([][]byte, error) {
	store, err := cfg.OpenStore()
	if err != nil {
		return nil, fmt.Errorf("open the xorb store: %w", err)
	}
	p.store = store
	src, err := openBlobSource(ctx, cfg.Stderr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := src.Close(); err != nil {
			debuglog.Printf("bbpush: %v", err)
		}
	}()
	chunks := make([]*blobChunks, len(blobs))
	for i, b := range blobs {
		if chunks[i], err = scanChunks(ctx, src, b); err != nil {
			return nil, err
		}
	}
	ix := newChunkIndex()
	if err := loadKnownXorbs(ctx, p.store, cfg.Client, ix, chunks); err != nil {
		return nil, err
	}

	upCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	up := cfg.Client.NewUploader(upCtx)
	stats := newPushStats(blobs)
	sealed := func(x *bbchunk.Xorb) error {
		stats.addBuilt(x)
		if err := p.store.PutXorb(x); err != nil {
			debuglog.Printf("bbpush: keep xorb %s: %v", x.Hash, err)
		}
		return up.Add(x)
	}
	entries := make([][]byte, len(blobs))
	names := xorbNames{seen: map[xethash.Hash]bool{}}
	var buildErr error
	for i, b := range blobs {
		var r *bbchunk.Recipe
		if r, buildErr = buildRecipe(ctx, src, ix, b, chunks[i], sealed); buildErr != nil {
			break
		}
		if entries[i], buildErr = appendEntry(b.oid, r); buildErr != nil {
			break
		}
		if buildErr = names.add(r.Xorbs); buildErr != nil {
			break
		}
	}
	p.xorbs = names.list
	if buildErr != nil {
		// The push falls back to send-pack, so the uploads that have not
		// finished are of no use.
		cancel()
	}
	results, err := up.Wait()
	recordUploads(p.store, results)
	if err := errors.Join(buildErr, err); err != nil {
		return nil, err
	}
	p.summary = stats.line(p.xorbs, results)
	return entries, nil
}

// xorbNames lists the distinct xorbs that the entries of a push name, in
// the order of first use.
type xorbNames struct {
	seen map[xethash.Hash]bool
	list []xethash.Hash
}

// add adds the xorbs of one recipe. It fails when the push would name more
// than maxPushXorbs xorbs, which the server refuses.
func (n *xorbNames) add(hs []xethash.Hash) error {
	for _, h := range hs {
		if !n.seen[h] {
			n.seen[h] = true
			n.list = append(n.list, h)
		}
	}
	if len(n.list) > maxPushXorbs {
		return fmt.Errorf("the entries name %d xorbs, over the %d-xorb limit", len(n.list), maxPushXorbs)
	}
	return nil
}

// appendEntry returns the meta 0x03 entry of blob oidHex. The server stores
// the entry as meta 0x02, which adds a byte range of two uvarints to each
// term, under the same payload limit. appendEntry refuses an entry whose
// stored form could pass that limit.
func appendEntry(oidHex string, r *bbchunk.Recipe) ([]byte, error) {
	oid, err := decodeOID(oidHex)
	if err != nil {
		return nil, err
	}
	e, err := bbchunk.AppendEntry(nil, oid, r)
	if err != nil {
		return nil, fmt.Errorf("entry of blob %s: %w", oidHex, err)
	}
	if stored := len(e) + len(r.Terms)*2*binary.MaxVarintLen64; stored > bbchunk.MaxPayloadSize {
		return nil, fmt.Errorf("entry of blob %s: the stored entry can take %d bytes, over the %d-byte limit", oidHex, stored, bbchunk.MaxPayloadSize)
	}
	return e, nil
}

func recordUploads(store *bbcache.Store, results map[xethash.Hash]*bbclient.UploadResult) {
	for _, r := range results {
		if err := store.RecordUpload(r); err != nil {
			debuglog.Printf("bbpush: record upload of xorb %s: %v", r.Xorb, err)
		}
	}
}

// reportError is a report-status that did not arrive after the request
// went out.
type reportError struct{ err error }

func (e *reportError) Error() string { return "reading receive-pack report: " + e.err.Error() }

func (e *reportError) Unwrap() error { return e.err }

// post sends the request and reads the report-status.
func (p *prepared) post(ctx context.Context, cfg *Config) (*report, error) {
	resp, err := cfg.Post(ctx, p.body)
	if err != nil {
		return nil, fmt.Errorf("posting receive-pack: %w", err)
	}
	defer resp.Close()
	rep, err := readReport(resp, p.sideband, cfg.Stderr)
	if err != nil {
		return nil, &reportError{err: err}
	}
	return rep, nil
}

// repairMissing asks for the state of every xorb that the entries name,
// forgets the records of the missing ones and uploads them again from the
// local store.
func (p *prepared) repairMissing(ctx context.Context, cfg *Config) error {
	res, err := cfg.Client.Dedup(ctx, nil, p.xorbs)
	if err != nil {
		return fmt.Errorf("query xorbs: %w", err)
	}
	if err := p.store.RecordDedup(res); err != nil {
		debuglog.Printf("bbpush: record dedup answer: %v", err)
	}
	up := cfg.Client.NewUploader(ctx)
	var errs []error
	for _, s := range res.Xorbs {
		if s.State != bbclient.StateMissing {
			continue
		}
		x, ok := p.store.Xorb(s.Xorb)
		if !ok {
			errs = append(errs, fmt.Errorf("xorb %s is missing on the server and not stored locally", s.Xorb))
			break
		}
		if err := up.Add(x); err != nil {
			errs = append(errs, err)
			break
		}
	}
	results, err := up.Wait()
	recordUploads(p.store, results)
	return errors.Join(append(errs, err)...)
}
