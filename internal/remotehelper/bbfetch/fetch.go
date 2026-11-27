package bbfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/jmwoliver/xet-go/xethash"

	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// minChunkedSize is the smallest logical size of a recipe in a fetch. A
// recipe of a smaller blob saves nothing, and entiredb takes no client
// recipe below it.
const minChunkedSize = 64 << 10

// RecipeError is a failure in the recipe path of a fetch: a complete pack
// that breaks the pack contract, a recipe that fails its checks, a download
// that fails, or a blob that does not rebuild. The fetch records a fallback
// after it, so the next fetch gets whole blobs. A failure of the server's
// stream or of the write to git is a *StreamError, not a RecipeError.
type RecipeError struct{ Err error }

func (e *RecipeError) Error() string { return e.Err.Error() }

func (e *RecipeError) Unwrap() error { return e.Err }

func recipeError(err error) error {
	var re *RecipeError
	if errors.As(err, &re) {
		return err
	}
	return &RecipeError{Err: err}
}

// Config is what RewritePack needs besides the pack.
type Config struct {
	// Store is the per-repo cache. It holds the complete xorbs that a fetch
	// rebuilds from, and the network form of the xorbs that a push built.
	Store *bbcache.Store
	// Client downloads the xorbs that Store lacks.
	Client *bbclient.Client
	// ObjectFormat is the repository's object format: "sha1" or "sha256".
	ObjectFormat string
	// Quiet turns progress off, for a fetch request with no-progress.
	Quiet bool
}

// Stats counts the work of one RewritePack.
type Stats struct {
	// Recipes is the number of type-5 entries, and LogicalBytes the size of
	// the blobs rebuilt in their place.
	Recipes      int
	LogicalBytes int64
	// Xorbs is the number of distinct xorbs that the recipes name. Each one
	// was in the cache (Cached), built by a push of this clone (Converted),
	// or downloaded.
	Xorbs     int
	Cached    int
	Converted int
	// Downloaded counts the downloaded xorbs and DownloadedBytes their size.
	// Corrupt counts the cached xorbs that failed a check and were
	// downloaded again.
	Downloaded      int
	DownloadedBytes int64
	Corrupt         int
}

// RewritePack reads a fetched pack from data and writes the pack that git
// reads to out. It forwards the ordinary entries as they arrive. Then it
// gets every xorb that the recipes name into the cache, rebuilds each blob,
// requires its object ID to match the recipe, and writes the trailer. It
// reports progress on band 2 of out.
//
// The cache policy of a fetch: it downloads every xorb that it needs, even
// past the cache's size cap, and pins them until the pack is done. The trim
// at the end of the session then applies the cap.
//
// An error wraps *RecipeError when the fetch must fall back, and is a
// *StreamError when the server's stream or the write to git failed.
// RewritePack writes no trailer after an error, so git refuses the pack.
func RewritePack(ctx context.Context, cfg Config, data io.Reader, out *gitproto.SidebandWriter) (*Stats, error) {
	rw, err := NewRewriter(out, cfg.ObjectFormat)
	if err != nil {
		return nil, err
	}
	var entries []ChunkedEntry
	err = rw.ReadPack(data, func(e ChunkedEntry) error {
		entries = append(entries, e)
		return nil
	})
	if errors.Is(err, ErrMalformedPack) || errors.Is(err, bbchunk.ErrMalformedChunked) {
		return nil, recipeError(err)
	}
	if err != nil {
		return nil, err
	}
	p := &packRebuild{cfg: cfg, rw: rw, out: out, stats: &Stats{Recipes: len(entries)}, redownloaded: map[xethash.Hash]bool{}}
	if len(entries) > 0 {
		err := p.rebuild(ctx, entries)
		cfg.Store.ReleaseComplete(p.xorbs)
		if err != nil {
			return p.stats, err
		}
	}
	if err := rw.Finish(); err != nil {
		return p.stats, err
	}
	return p.stats, nil
}

type packRebuild struct {
	cfg   Config
	rw    *Rewriter
	out   *gitproto.SidebandWriter
	stats *Stats
	// xorbs lists each xorb that the recipes name, once.
	xorbs []xethash.Hash
	// redownloaded holds the xorbs that left the cache during the rebuild
	// and were downloaded again. A second miss is a failure.
	redownloaded map[xethash.Hash]bool
}

func (p *packRebuild) rebuild(ctx context.Context, entries []ChunkedEntry) error {
	recipes, err := p.parse(entries)
	if err != nil {
		return recipeError(err)
	}
	if err := p.resolve(ctx, recipes); err != nil {
		return err
	}
	for i, r := range recipes {
		if err := p.progress(fmt.Sprintf("Rebuilding large files: %d/%d\r", i, len(recipes))); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the cancellation is the whole story
		}
		if err := p.appendBlob(ctx, r, entries[i].Size); err != nil {
			return err
		}
		p.stats.LogicalBytes += entries[i].Size
	}
	return p.progress(fmt.Sprintf("Rebuilding large files: %d/%d, done.\n", len(recipes), len(recipes)))
}

// parse decodes and checks the recipes, and lists their xorbs in p.xorbs.
// It refuses a logical size outside [64 KiB, bbchunk.MaxObjectSize], and two
// recipes for one blob.
func (p *packRebuild) parse(entries []ChunkedEntry) ([]*bbchunk.ChunkedPayload, error) {
	recipes := make([]*bbchunk.ChunkedPayload, len(entries))
	seen := map[xethash.Hash]bool{}
	oids := map[string]bool{}
	for i, e := range entries {
		if e.Size < minChunkedSize || e.Size > bbchunk.MaxObjectSize {
			return nil, fmt.Errorf("%w: recipe %d: logical size %d is outside [%d, %d]", ErrMalformedPack, i, e.Size, minChunkedSize, bbchunk.MaxObjectSize)
		}
		r, err := bbchunk.ParseChunkedPayload(e.Payload, p.rw.hashSize, e.Size)
		if err != nil {
			return nil, fmt.Errorf("recipe %d: %w", i, err)
		}
		if oids[string(r.OID)] {
			return nil, fmt.Errorf("%w: two recipes for blob %x", ErrMalformedPack, r.OID)
		}
		oids[string(r.OID)] = true
		recipes[i] = r
		entries[i].Payload = nil
		for _, h := range r.Xorbs {
			if !seen[h] {
				seen[h] = true
				p.xorbs = append(p.xorbs, h)
			}
		}
	}
	p.stats.Xorbs = len(p.xorbs)
	return recipes, nil
}

// progress sends msg on band 2. A failed write to git is a *StreamError.
func (p *packRebuild) progress(msg string) error {
	if p.cfg.Quiet {
		return nil
	}
	if err := p.out.Progress([]byte(msg)); err != nil {
		return &StreamError{Err: fmt.Errorf("bbfetch: progress: %w", err)}
	}
	return nil
}

// resolve puts every xorb of p.xorbs into the cache as a complete xorb.
func (p *packRebuild) resolve(ctx context.Context, recipes []*bbchunk.ChunkedPayload) error {
	ranges := termRanges(recipes)
	var missing []xethash.Hash
	for _, h := range p.xorbs {
		switch {
		case p.cfg.Store.HasComplete(h):
			if checkCached(p.cfg.Store, h, ranges[h]) {
				p.stats.Cached++
				continue
			}
			p.stats.Corrupt++
		case p.convertPushed(h):
			p.stats.Converted++
			continue
		}
		missing = append(missing, h)
	}
	if len(missing) == 0 {
		return nil
	}
	dp := &downloadProgress{p: p, total: len(missing)}
	sink := &countingSink{Store: p.cfg.Store, written: dp.add}
	err := p.cfg.Client.DownloadAll(ctx, missing, sink, dp.finished)
	p.stats.Downloaded, p.stats.DownloadedBytes = int(sink.count.Load()), sink.bytes.Load()
	if err != nil {
		if ctx.Err() != nil {
			return err //nolint:wrapcheck // the cancellation is the whole story
		}
		return recipeError(err)
	}
	return dp.done()
}

// termRanges returns, for each xorb, the chunk ranges that the recipes
// read from it.
func termRanges(recipes []*bbchunk.ChunkedPayload) map[xethash.Hash][][2]int {
	out := map[xethash.Hash][][2]int{}
	for _, r := range recipes {
		for _, t := range r.Terms {
			h := r.Xorbs[t.Xorb]
			out[h] = append(out[h], [2]int{int(t.ChunkStart), int(t.ChunkEnd)})
		}
	}
	return out
}

// checkCached reads the chunks of the cached xorb h that ranges name, and
// checks them, so no damaged chunk reaches git. A corrupt xorb is removed
// from the cache, so the fetch downloads it again. A download checks every
// chunk as it arrives, so only a xorb that an earlier run stored needs
// this. A range past the end of the xorb is left to the rebuild, which
// refuses it.
func checkCached(store *bbcache.Store, h xethash.Hash, ranges [][2]int) bool {
	x, ok := store.OpenComplete(h)
	if !ok {
		return false
	}
	n := x.Footer().Chunks()
	needed := make([]bool, n)
	for _, r := range ranges {
		for i := max(r[0], 0); i < min(r[1], n); i++ {
			needed[i] = true
		}
	}
	var err error
	for start := 0; start < n && err == nil; {
		if !needed[start] {
			start++
			continue
		}
		end := start
		for end < n && needed[end] {
			end++
		}
		var r io.Reader
		if r, err = x.Term(start, end); err == nil {
			_, err = io.Copy(io.Discard, r)
		}
		start = end
	}
	if closeErr := x.Close(); err == nil {
		err = closeErr
	}
	return err == nil
}

// convertPushed stores the complete form of h when a push of this clone
// built it. The cache checks the network form against h before it returns
// it, so the xorb needs no download.
func (p *packRebuild) convertPushed(h xethash.Hash) bool {
	x, ok := p.cfg.Store.Xorb(h)
	if !ok {
		return false
	}
	err := p.cfg.Store.PutComplete(h, func(w io.Writer) (*bbchunk.Footer, error) {
		if _, err := w.Write(x.Complete()); err != nil {
			return nil, err //nolint:wrapcheck // PutComplete names the xorb
		}
		return x.Footer, nil
	})
	if err != nil {
		debuglog.Printf("bbfetch: convert pushed xorb %s: %v; downloading it", h, err)
		return false
	}
	return true
}

// appendBlob rebuilds the blob of r from the cache and writes it in place of
// the next type-5 entry.
func (p *packRebuild) appendBlob(ctx context.Context, r *bbchunk.ChunkedPayload, size int64) error {
	br := &blobReader{ctx: ctx, p: p, recipe: r}
	oid, err := p.rw.AppendBlob(size, br)
	if closeErr := br.close(); err == nil && closeErr != nil {
		err = recipeError(closeErr)
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // the cancellation is the whole story
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(oid, r.OID) {
		return recipeError(fmt.Errorf("blob %x: the rebuilt content hashes to %x", r.OID, oid))
	}
	return nil
}

// blobReader reads the data of a recipe's terms from the cached xorbs, in
// order. It checks each term against the footer of its xorb, and each chunk
// against its hash. Every error it returns is a *RecipeError.
type blobReader struct {
	ctx    context.Context //nolint:containedctx // a reader that downloads in Read
	p      *packRebuild
	recipe *bbchunk.ChunkedPayload
	next   int
	term   io.Reader
	// xorb is the open xorb of the current term, and xorbIndex its index in
	// the recipe's xorb table.
	xorb      *bbcache.CachedXorb
	xorbIndex uint32
}

func (b *blobReader) Read(p []byte) (int, error) {
	for {
		if b.term != nil {
			n, err := b.term.Read(p)
			if errors.Is(err, io.EOF) {
				b.term = nil
				err = nil
				if n == 0 {
					continue
				}
			}
			if err != nil {
				return n, recipeError(err)
			}
			return n, nil
		}
		if b.next == len(b.recipe.Terms) {
			return 0, io.EOF
		}
		if err := b.openTerm(b.recipe.Terms[b.next]); err != nil {
			return 0, recipeError(fmt.Errorf("blob %x: term %d: %w", b.recipe.OID, b.next, err))
		}
		b.next++
	}
}

func (b *blobReader) openTerm(t bbchunk.ChunkedTerm) error {
	if b.xorb == nil || b.xorbIndex != t.Xorb {
		if err := b.close(); err != nil {
			return err
		}
		h := b.recipe.Xorbs[t.Xorb]
		x, err := b.p.open(b.ctx, h)
		if err != nil {
			return err
		}
		b.xorb, b.xorbIndex = x, t.Xorb
	}
	f := b.xorb.Footer()
	start, end := int(t.ChunkStart), int(t.ChunkEnd)
	first, last, err := f.StoredRange(start, end)
	if err != nil {
		return err //nolint:wrapcheck // StoredRange names the range
	}
	if first != t.FirstByte || last != t.LastByte {
		return fmt.Errorf("stored bytes [%d, %d] of chunks [%d, %d), the recipe says [%d, %d]", first, last, start, end, t.FirstByte, t.LastByte)
	}
	unpacked, err := f.UnpackedSize(start, end)
	if err != nil {
		return err //nolint:wrapcheck // UnpackedSize names the range
	}
	if unpacked != t.UnpackedBytes {
		return fmt.Errorf("chunks [%d, %d) hold %d bytes, the recipe says %d", start, end, unpacked, t.UnpackedBytes)
	}
	b.term, err = b.xorb.Term(start, end)
	return err //nolint:wrapcheck // Term names the range
}

// open opens the cached xorb h. Another process can trim the cache after
// resolve, so a miss downloads h again, once in a pack, with every check
// of a download.
func (p *packRebuild) open(ctx context.Context, h xethash.Hash) (*bbcache.CachedXorb, error) {
	if x, ok := p.cfg.Store.OpenComplete(h); ok {
		return x, nil
	}
	if p.redownloaded[h] {
		return nil, fmt.Errorf("xorb %s left the cache twice", h)
	}
	p.redownloaded[h] = true
	debuglog.Printf("bbfetch: xorb %s left the cache; downloading it again", h)
	if err := p.cfg.Client.Download(ctx, h, p.cfg.Store); err != nil {
		return nil, fmt.Errorf("xorb %s left the cache: %w", h, err)
	}
	p.stats.Downloaded++
	x, ok := p.cfg.Store.OpenComplete(h)
	if !ok {
		return nil, fmt.Errorf("xorb %s is not in the cache", h)
	}
	return x, nil
}

func (b *blobReader) close() error {
	if b.xorb == nil {
		return nil
	}
	err := b.xorb.Close()
	b.xorb = nil
	return err //nolint:wrapcheck // Close names the store
}

// countingSink counts the xorbs that a download stores and their bytes, and
// passes the length of each write to written.
type countingSink struct {
	*bbcache.Store

	written func(n int)

	count atomic.Int64
	bytes atomic.Int64
}

func (c *countingSink) PutComplete(h xethash.Hash, fill func(io.Writer) (*bbchunk.Footer, error)) error {
	var size int64
	err := c.Store.PutComplete(h, func(w io.Writer) (*bbchunk.Footer, error) {
		f, err := fill(&notifyWriter{w: w, written: c.written})
		if f != nil {
			size = f.Size()
		}
		return f, err
	})
	if err == nil {
		c.count.Add(1)
		c.bytes.Add(size)
	}
	return err //nolint:wrapcheck // PutComplete names the xorb
}

type notifyWriter struct {
	w       io.Writer
	written func(n int)
}

func (n *notifyWriter) Write(p []byte) (int, error) {
	k, err := n.w.Write(p)
	n.written(k)
	return k, err //nolint:wrapcheck // the download names the xorb
}

// downloadProgress reports the downloads on band 2: the finished xorbs and
// the bytes written so far. The downloads run in parallel, so it serializes
// the reports, and it sends one only when its text changes.
type downloadProgress struct {
	p     *packRebuild
	total int

	mu      sync.Mutex
	xorbs   int
	written int64
	last    string
	err     error
}

// add counts n written bytes.
func (d *downloadProgress) add(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written += int64(n)
	d.report()
}

// finished takes the count of finished downloads from DownloadAll.
func (d *downloadProgress) finished(done, _ int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.xorbs = done
	d.report()
}

func (d *downloadProgress) text() string {
	return fmt.Sprintf("Downloading large files: %d/%d xorbs, %s", d.xorbs, d.total, mebibytes(d.written))
}

func (d *downloadProgress) report() {
	if d.err != nil {
		return
	}
	text := d.text()
	if text == d.last {
		return
	}
	d.last = text
	d.err = d.p.progress(text + "\r")
}

// done sends the last report, or returns the error of an earlier one.
func (d *downloadProgress) done() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	return d.p.progress(d.text() + ", done.\n")
}

func mebibytes(n int64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
