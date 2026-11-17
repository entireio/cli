package bbchunk

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"
)

// builderChunks returns chunks that select every compression scheme: random
// bytes stay uncompressed, a repeated byte takes LZ4 and a ramp of 32-bit
// words takes the byte grouping.
func builderChunks() [][]byte {
	random := randomBytes(40<<10, 1)
	ramp := make([]byte, 64<<10)
	for i := range ramp {
		ramp[i] = byte(i / 4 * (i % 4))
	}
	return [][]byte{random, bytes.Repeat([]byte{'a'}, 16<<10), ramp, []byte("tail")}
}

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// build packs chunks into one xorb as a push does.
func build(t *testing.T, chunks [][]byte) *Xorb {
	t.Helper()
	var b xorb.Builder
	for i, c := range chunks {
		_, err := b.AddHashed(c, xethash.Chunk(c), xorb.Auto)
		require.NoError(t, err, "chunk %d", i)
	}
	x, err := Seal(&b)
	require.NoError(t, err)
	require.Zero(t, b.Len(), "Seal empties the builder")
	return x
}

// TestSealMatchesEncode holds the xorbs that a push builds to the bytes that
// xet-go writes for the same chunks. entiredb's split also builds xorbs with
// xorb.Builder and xorb.Auto, so the server stores what it would have built
// itself.
func TestSealMatchesEncode(t *testing.T) {
	t.Parallel()
	chunks := builderChunks()
	x := build(t, chunks)

	obj, err := xorb.Encode(chunks, xorb.Auto)
	require.NoError(t, err)
	require.Equal(t, obj.Hash, x.Hash)
	require.Equal(t, obj.Body, x.Network)
	schemes := map[xorb.Scheme]bool{}
	require.Len(t, x.Footer.ChunkHashes, len(obj.Chunks))
	for i, c := range obj.Chunks {
		require.Equal(t, c.Hash, x.Footer.ChunkHashes[i], "chunk %d", i)
		require.Equal(t, c.SerializedOffset+c.SerializedLength, x.Footer.ChunkBoundaryOffsets[i], "chunk %d", i)
		require.Equal(t, c.UncompressedOffset+c.UncompressedLength, x.Footer.UnpackedChunkOffsets[i], "chunk %d", i)
		schemes[c.Scheme] = true
	}
	require.Len(t, schemes, 3, "the chunks must cover every scheme")
}

// TestSealIngestRoundTrip runs the check that the upload endpoint runs:
// xorb.IngestNetwork rehashes every chunk against the xorb hash. The footer
// that the server adds is the footer of the sealed xorb.
func TestSealIngestRoundTrip(t *testing.T) {
	t.Parallel()
	chunks := builderChunks()
	x := build(t, chunks)

	in, err := xorb.IngestNetwork(x.Network, x.Hash)
	require.NoError(t, err)
	require.Equal(t, x.Hash, in.Hash)
	require.Equal(t, x.Complete(), in.Complete)

	dec, err := xorb.Decode(x.Network)
	require.NoError(t, err)
	for i, c := range dec.Chunks {
		require.Equal(t, chunks[i], c.Data, "chunk %d", i)
	}

	_, err = xorb.IngestNetwork(x.Network, xethash.Chunk([]byte("other")))
	require.ErrorIs(t, err, xorb.ErrHashMismatch)
}

func TestSealEmpty(t *testing.T) {
	t.Parallel()
	var b xorb.Builder
	_, err := Seal(&b)
	require.Error(t, err)
}
