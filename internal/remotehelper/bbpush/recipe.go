package bbpush

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// minRun is the shortest run of known chunks that a recipe takes from a
// xorb in place of new chunk data, unless the run ends the blob. Each run
// adds terms to the entry, and the spec asks the client to keep recipes
// small.
const minRun = 4

// blobChunks lists the chunks of one large blob, from a first read.
type blobChunks struct {
	hashes []xethash.Hash
	sizes  []uint32
}

// scanChunks reads blob b once and returns its chunk list.
func scanChunks(ctx context.Context, src *blobSource, b largeBlob) (*blobChunks, error) {
	c := &blobChunks{}
	err := src.read(b.oid, b.size, func(r io.Reader) error {
		return bbchunk.Split(ctx, r, func(ch bbchunk.Chunk) error {
			c.hashes = append(c.hashes, ch.Hash)
			c.sizes = append(c.sizes, uint32(len(ch.Data))) //nolint:gosec // a chunk is at most 128 KiB
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("chunk blob %s: %w", b.oid, err)
	}
	return c, nil
}

// knownXorb is a xorb that a recipe may name: one that the repo holds, or
// one that this push builds. The hash of a xorb that this push builds is
// set when it seals.
type knownXorb struct {
	hash   xethash.Hash
	sealed bool
	chunks []xethash.Hash
}

type chunkLoc struct {
	xorb  int
	index int
}

// chunkIndex maps chunk hashes to a place in a known xorb. The first place
// of a chunk wins.
type chunkIndex struct {
	xorbs  []*knownXorb
	chunks map[xethash.Hash]chunkLoc
	// stored maps the hash of a sealed xorb to its position in xorbs.
	stored map[xethash.Hash]int
}

func newChunkIndex() *chunkIndex {
	return &chunkIndex{chunks: map[xethash.Hash]chunkLoc{}, stored: map[xethash.Hash]int{}}
}

// addStored adds a sealed xorb with the given chunk hashes, unless the index
// has it.
func (ix *chunkIndex) addStored(h xethash.Hash, chunks []xethash.Hash) {
	if _, ok := ix.stored[h]; ok {
		return
	}
	id := len(ix.xorbs)
	ix.xorbs = append(ix.xorbs, &knownXorb{hash: h, sealed: true})
	ix.stored[h] = id
	for _, c := range chunks {
		ix.appendChunk(id, c)
	}
}

func (ix *chunkIndex) newOpen() int {
	ix.xorbs = append(ix.xorbs, &knownXorb{})
	return len(ix.xorbs) - 1
}

func (ix *chunkIndex) appendChunk(id int, h xethash.Hash) {
	x := ix.xorbs[id]
	if _, ok := ix.chunks[h]; !ok {
		ix.chunks[h] = chunkLoc{xorb: id, index: len(x.chunks)}
	}
	x.chunks = append(x.chunks, h)
}

func (ix *chunkIndex) seal(id int, h xethash.Hash) {
	ix.xorbs[id].hash, ix.xorbs[id].sealed = h, true
	if _, ok := ix.stored[h]; !ok {
		ix.stored[h] = id
	}
}

// idTerm is a term over the xorb at position xorb of the index.
type idTerm struct {
	xorb, start, end int
	bytes            uint64
}

// blobBuilder writes the recipe of one blob during its second read. Each
// chunk either extends the last term, starts a run in a known xorb, or goes
// into the open xorb of this push. The open xorb seals when it is full and
// at the end of the blob.
type blobBuilder struct {
	ix     *chunkIndex
	chunks *blobChunks
	sealed func(*bbchunk.Xorb) error

	// k is the index of the next chunk, and skip the number of chunks that
	// an accepted run still covers.
	k, skip int
	terms   []idTerm
	open    int
	xb      xorb.Builder
}

// buildRecipe reads blob b a second time and returns its recipe. sealed
// receives each new xorb.
func buildRecipe(ctx context.Context, src *blobSource, ix *chunkIndex, b largeBlob, chunks *blobChunks, sealed func(*bbchunk.Xorb) error) (*bbchunk.Recipe, error) {
	bb := &blobBuilder{ix: ix, chunks: chunks, sealed: sealed, open: -1}
	err := src.read(b.oid, b.size, func(r io.Reader) error {
		return bbchunk.Split(ctx, r, bb.add)
	})
	if err != nil {
		return nil, fmt.Errorf("build recipe of blob %s: %w", b.oid, err)
	}
	if bb.k != len(chunks.hashes) {
		return nil, fmt.Errorf("blob %s has %d chunks, the first read had %d", b.oid, bb.k, len(chunks.hashes))
	}
	if err := bb.sealOpen(); err != nil {
		return nil, err
	}
	return bb.recipe(b.size)
}

func (b *blobBuilder) add(c bbchunk.Chunk) error {
	k, h := b.k, c.Hash
	if k >= len(b.chunks.hashes) || h != b.chunks.hashes[k] {
		return errors.New("the blob changed between two reads")
	}
	b.k++
	if b.skip > 0 {
		b.skip--
		return nil
	}
	if b.extendLast(h, k) {
		return nil
	}
	if loc, ok := b.ix.chunks[h]; ok {
		if r := b.runLen(loc, k); r >= min(minRun, len(b.chunks.hashes)-k) {
			b.appendTerm(idTerm{xorb: loc.xorb, start: loc.index, end: loc.index + r, bytes: b.unpacked(k, k+r)})
			b.skip = r - 1
			return nil
		}
	}
	return b.addNew(c, k)
}

// extendLast extends the last term when chunk k is the next chunk of its
// xorb.
func (b *blobBuilder) extendLast(h xethash.Hash, k int) bool {
	if len(b.terms) == 0 {
		return false
	}
	t := &b.terms[len(b.terms)-1]
	x := b.ix.xorbs[t.xorb].chunks
	if t.end >= len(x) || x[t.end] != h {
		return false
	}
	t.end++
	t.bytes += uint64(b.chunks.sizes[k])
	return true
}

// runLen returns how many chunks from chunk k match the xorb at loc in a
// row.
func (b *blobBuilder) runLen(loc chunkLoc, k int) int {
	x := b.ix.xorbs[loc.xorb].chunks
	hs := b.chunks.hashes
	r := 0
	for k+r < len(hs) && loc.index+r < len(x) && x[loc.index+r] == hs[k+r] {
		r++
	}
	return r
}

func (b *blobBuilder) unpacked(from, to int) uint64 {
	var n uint64
	for _, s := range b.chunks.sizes[from:to] {
		n += uint64(s)
	}
	return n
}

func (b *blobBuilder) addNew(c bbchunk.Chunk, k int) error {
	if b.open < 0 {
		b.open = b.ix.newOpen()
	}
	_, err := b.xb.AddHashed(c.Data, c.Hash, xorb.Auto)
	if errors.Is(err, xorb.ErrFull) {
		if err := b.sealOpen(); err != nil {
			return err
		}
		b.open = b.ix.newOpen()
		_, err = b.xb.AddHashed(c.Data, c.Hash, xorb.Auto)
	}
	if err != nil {
		return fmt.Errorf("add chunk: %w", err)
	}
	pos := len(b.ix.xorbs[b.open].chunks)
	b.ix.appendChunk(b.open, c.Hash)
	b.appendTerm(idTerm{xorb: b.open, start: pos, end: pos + 1, bytes: uint64(b.chunks.sizes[k])})
	return nil
}

func (b *blobBuilder) appendTerm(t idTerm) {
	if n := len(b.terms); n > 0 && b.terms[n-1].xorb == t.xorb && b.terms[n-1].end == t.start {
		b.terms[n-1].end = t.end
		b.terms[n-1].bytes += t.bytes
		return
	}
	b.terms = append(b.terms, t)
}

func (b *blobBuilder) sealOpen() error {
	if b.open < 0 {
		return nil
	}
	x, err := bbchunk.Seal(&b.xb)
	if err != nil {
		return fmt.Errorf("seal xorb: %w", err)
	}
	b.ix.seal(b.open, x.Hash)
	b.open = -1
	return b.sealed(x)
}

// recipe converts the terms to a recipe over xorb hashes, in the order of
// first use.
func (b *blobBuilder) recipe(size int64) (*bbchunk.Recipe, error) {
	r := &bbchunk.Recipe{Size: size}
	pos := map[xethash.Hash]int{}
	for _, t := range b.terms {
		x := b.ix.xorbs[t.xorb]
		if !x.sealed {
			return nil, errors.New("a term names a xorb that did not seal")
		}
		i, ok := pos[x.hash]
		if !ok {
			i = len(r.Xorbs)
			pos[x.hash] = i
			r.Xorbs = append(r.Xorbs, x.hash)
		}
		if n := len(r.Terms); n > 0 && r.Terms[n-1].Xorb == i && r.Terms[n-1].ChunkEnd == t.start {
			r.Terms[n-1].ChunkEnd = t.end
			r.Terms[n-1].UnpackedBytes += t.bytes
			continue
		}
		r.Terms = append(r.Terms, bbchunk.Term{Xorb: i, ChunkStart: t.start, ChunkEnd: t.end, UnpackedBytes: t.bytes})
	}
	nodes := make([]xethash.Node, len(b.chunks.hashes))
	for i, h := range b.chunks.hashes {
		nodes[i] = xethash.Node{Hash: h, Size: uint64(b.chunks.sizes[i])}
	}
	r.FileHash = xethash.File(nodes)
	return r, nil
}
