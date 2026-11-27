package bbchunk

import (
	"bytes"
	"errors"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"
)

// chunkBlob cuts data into chunks and packs them into xorbs in order. A xorb
// seals when the next chunk does not fit and at the end of the data. The
// recipe has one term per xorb, and names a repeated xorb once. This is the
// "xorbs of the input" recipe of the client vectors.
func chunkBlob(t *testing.T, data []byte) (*Recipe, []*Xorb) {
	t.Helper()
	r := &Recipe{Size: int64(len(data))}
	var xorbs []*Xorb
	var nodes []xethash.Node
	var b xorb.Builder
	seal := func() {
		x, err := Seal(&b)
		require.NoError(t, err)
		i := len(r.Xorbs)
		for j, h := range r.Xorbs {
			if h == x.Hash {
				i = j
			}
		}
		if i == len(r.Xorbs) {
			r.Xorbs = append(r.Xorbs, x.Hash)
			xorbs = append(xorbs, x)
		}
		n := len(x.Footer.ChunkHashes)
		unpacked := uint64(x.Footer.UnpackedChunkOffsets[n-1])
		r.Terms = append(r.Terms, Term{Xorb: i, ChunkStart: 0, ChunkEnd: n, UnpackedBytes: unpacked})
	}
	err := Split(t.Context(), bytes.NewReader(data), func(c Chunk) error {
		_, err := b.AddHashed(c.Data, c.Hash, xorb.Auto)
		if errors.Is(err, xorb.ErrFull) {
			seal()
			_, err = b.AddHashed(c.Data, c.Hash, xorb.Auto)
		}
		nodes = append(nodes, xethash.Node{Hash: c.Hash, Size: uint64(len(c.Data))})
		return err
	})
	require.NoError(t, err)
	if b.Len() > 0 {
		seal()
	}
	r.FileHash = xethash.File(nodes)
	return r, xorbs
}

func xorbHashes(xorbs []*Xorb) []xethash.Hash {
	hs := make([]xethash.Hash, len(xorbs))
	for i, x := range xorbs {
		hs[i] = x.Hash
	}
	return hs
}
