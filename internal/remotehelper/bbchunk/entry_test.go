package bbchunk

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"
)

func filledHash(b byte) xethash.Hash {
	var h xethash.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// TestAppendEntryLayout pins the byte layout of the spec's "Meta 0x03 entry".
func TestAppendEntryLayout(t *testing.T) {
	t.Parallel()
	oid := bytes.Repeat([]byte{0xaa}, 20)
	r := &Recipe{
		Size:     300,
		FileHash: filledHash(0xf0),
		Xorbs:    []xethash.Hash{filledHash(0x01), filledHash(0x02)},
		Terms: []Term{
			{Xorb: 0, ChunkStart: 0, ChunkEnd: 2, UnpackedBytes: 200},
			{Xorb: 1, ChunkStart: 5, ChunkEnd: 200, UnpackedBytes: 100},
		},
	}
	got, err := AppendEntry([]byte{0x99}, oid, r)
	require.NoError(t, err)

	var payload []byte
	payload = append(payload, oid...)
	payload = append(payload, bytes.Repeat([]byte{0xf0}, 32)...)
	payload = append(payload, 2)
	payload = append(payload, bytes.Repeat([]byte{0x01}, 32)...)
	payload = append(payload, bytes.Repeat([]byte{0x02}, 32)...)
	payload = append(payload, 2)
	payload = append(payload, 0, 0, 2, 0xc8, 0x01) // 200 is 0xc8 0x01 as a uvarint
	payload = append(payload, 1, 5, 0xc8, 0x01, 100)
	payload = append(payload, 0) // inline count
	want := []byte{0x99}
	// Type 5, size 300 = 0x12c: low nibble 0xc with the more bit, then 0x12.
	want = append(want, 0x80|5<<4|0x0c, 0x12)
	want = append(want, MetaClientChunked)
	want = binary.AppendUvarint(want, uint64(len(payload)))
	want = append(want, payload...)
	require.Equal(t, want, got)
}

func TestAppendEntryRoundTrip(t *testing.T) {
	t.Parallel()
	data := randomBytes(3<<20, 11)
	recipe, _ := chunkBlob(t, data)
	oid := bytes.Repeat([]byte{0x5a}, 32)
	entry, err := AppendEntry(nil, oid, recipe)
	require.NoError(t, err)

	got, rest := parseEntry(t, entry, len(oid))
	require.Empty(t, rest)
	require.Equal(t, oid, got.oid)
	require.Equal(t, recipe, got.recipe)
}

func TestAppendEntryRefuses(t *testing.T) {
	t.Parallel()
	valid := func() *Recipe {
		return &Recipe{
			Size:     150,
			FileHash: filledHash(9),
			Xorbs:    []xethash.Hash{filledHash(1), filledHash(2)},
			Terms: []Term{
				{Xorb: 0, ChunkStart: 0, ChunkEnd: 1, UnpackedBytes: 100},
				{Xorb: 1, ChunkStart: 0, ChunkEnd: 1, UnpackedBytes: 50},
			},
		}
	}
	oid := make([]byte, 20)
	_, err := AppendEntry(nil, oid, valid())
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		oid    []byte
		modify func(*Recipe) *Recipe
	}{
		"nil recipe":          {modify: func(*Recipe) *Recipe { return nil }},
		"short oid":           {oid: make([]byte, 19)},
		"zero size":           {modify: func(r *Recipe) *Recipe { r.Size = 0; return r }},
		"over object size":    {modify: func(r *Recipe) *Recipe { r.Size = MaxObjectSize + 1; return r }},
		"no xorbs":            {modify: func(r *Recipe) *Recipe { r.Xorbs = nil; return r }},
		"no terms":            {modify: func(r *Recipe) *Recipe { r.Terms = nil; return r }},
		"duplicate xorb":      {modify: func(r *Recipe) *Recipe { r.Xorbs[1] = r.Xorbs[0]; return r }},
		"index out of range":  {modify: func(r *Recipe) *Recipe { r.Terms[1].Xorb = 2; return r }},
		"negative index":      {modify: func(r *Recipe) *Recipe { r.Terms[1].Xorb = -1; return r }},
		"empty chunk range":   {modify: func(r *Recipe) *Recipe { r.Terms[0].ChunkEnd = 0; return r }},
		"negative start":      {modify: func(r *Recipe) *Recipe { r.Terms[0].ChunkStart = -1; return r }},
		"chunk end past xorb": {modify: func(r *Recipe) *Recipe { r.Terms[0].ChunkEnd = 8193; return r }},
		"zero unpacked":       {modify: func(r *Recipe) *Recipe { r.Terms[1].UnpackedBytes = 0; r.Size = 100; return r }},
		"size mismatch":       {modify: func(r *Recipe) *Recipe { r.Size = 151; return r }},
		"unused xorb": {modify: func(r *Recipe) *Recipe {
			r.Terms[1].Xorb = 0
			return r
		}},
		"payload over 1 MiB": {modify: func(r *Recipe) *Recipe {
			// 4-byte terms: 1 MiB / 4 terms take the payload past the limit.
			n := MaxPayloadSize / 4
			r.Xorbs = r.Xorbs[:1]
			r.Terms = make([]Term, n)
			for i := range r.Terms {
				r.Terms[i] = Term{Xorb: 0, ChunkStart: 0, ChunkEnd: 1, UnpackedBytes: 1}
			}
			r.Size = int64(n)
			return r
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := valid()
			if tc.modify != nil {
				r = tc.modify(r)
			}
			o := oid
			if tc.oid != nil {
				o = tc.oid
			}
			dst := []byte{1, 2, 3}
			got, err := AppendEntry(dst, o, r)
			require.Error(t, err)
			require.Equal(t, dst, got, "a refused entry leaves dst as it was")
		})
	}
}

type parsedEntry struct {
	oid    []byte
	recipe *Recipe
}

// parseEntry decodes an entry by the spec layout, independent of AppendEntry.
func parseEntry(t *testing.T, b []byte, hashSize int) (parsedEntry, []byte) {
	t.Helper()
	c := b[0]
	require.Equal(t, byte(ExternalObject), (c>>4)&7)
	size := int64(c & 0x0f)
	shift := 4
	b = b[1:]
	for c&0x80 != 0 {
		c = b[0]
		b = b[1:]
		size |= int64(c&0x7f) << shift
		shift += 7
	}
	require.Equal(t, MetaClientChunked, b[0])
	b = b[1:]
	n, k := binary.Uvarint(b)
	require.Positive(t, k)
	b = b[k:]
	payload, rest := b[:n], b[n:]

	uv := func() uint64 {
		v, k := binary.Uvarint(payload)
		require.Positive(t, k)
		payload = payload[k:]
		return v
	}
	hash := func() xethash.Hash {
		h, err := xethash.FromBytes(payload[:xethash.Size])
		require.NoError(t, err)
		payload = payload[xethash.Size:]
		return h
	}
	e := parsedEntry{oid: payload[:hashSize], recipe: &Recipe{Size: size}}
	payload = payload[hashSize:]
	e.recipe.FileHash = hash()
	for range uv() {
		e.recipe.Xorbs = append(e.recipe.Xorbs, hash())
	}
	for range uv() {
		e.recipe.Terms = append(e.recipe.Terms, Term{Xorb: int(uv()), ChunkStart: int(uv()), ChunkEnd: int(uv()), UnpackedBytes: uv()})
	}
	require.Zero(t, uv(), "inline count")
	require.Empty(t, payload)
	return e, rest
}
