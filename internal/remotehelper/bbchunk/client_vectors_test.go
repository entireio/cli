package bbchunk

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"
)

// clientVectorsFile is a copy of entiredb's internal/xet/testdata/
// client_vectors.json, the known answers of the client-chunked push spec.
// Copy the file again when entiredb changes it. Do not edit it here.
const clientVectorsFile = "testdata/client_vectors.json"

type clientVectors struct {
	Chunker struct {
		TargetSize int `json:"target_size"`
		MinSize    int `json:"min_size"`
		MaxSize    int `json:"max_size"`
	} `json:"chunker"`
	AnchorModulus  int                  `json:"anchor_modulus"`
	Inputs         []clientInput        `json:"inputs"`
	Entries        []clientEntry        `json:"entries"`
	InvalidEntries []clientInvalidEntry `json:"invalid_entries"`
}

type clientInput struct {
	Name        string            `json:"name"`
	Parts       []clientInputPart `json:"parts"`
	Size        int               `json:"size"`
	SHA256      string            `json:"sha256"`
	GitSHA1     string            `json:"git_sha1"`
	GitSHA256   string            `json:"git_sha256"`
	ChunkEnds   []int             `json:"chunk_ends"`
	ChunkHashes []string          `json:"chunk_hashes"`
	Anchors     []int             `json:"anchors"`
	FileHash    string            `json:"file_hash"`
	Xorbs       []clientXorb      `json:"xorbs"`
}

// clientInputPart sets exactly one field.
type clientInputPart struct {
	SplitMix64 *struct {
		Seed uint64 `json:"seed"`
		Size int    `json:"size"`
	} `json:"splitmix64"`
	Fill *struct {
		Byte byte `json:"byte"`
		Size int  `json:"size"`
	} `json:"fill"`
	Slice *struct {
		Input string `json:"input"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	} `json:"slice"`
}

type clientXorb struct {
	Hash           string   `json:"hash"`
	Chunks         int      `json:"chunks"`
	UnpackedSize   int      `json:"unpacked_size"`
	StoredEnds     []uint32 `json:"stored_ends"`
	NetworkSize    int      `json:"network_size"`
	NetworkSHA256  string   `json:"network_sha256"`
	CompleteSize   int      `json:"complete_size"`
	CompleteSHA256 string   `json:"complete_sha256"`
	CompleteHex    string   `json:"complete_hex"`
	// FirstChunk is set on the xorbs of an input.
	FirstChunk int `json:"first_chunk"`
	// InputChunks is set on the new xorbs of a dedup entry.
	InputChunks []int `json:"input_chunks"`
}

type clientEntry struct {
	Name         string   `json:"name"`
	Input        string   `json:"input"`
	Recipe       string   `json:"recipe"`
	ObjectFormat string   `json:"object_format"`
	Size         int64    `json:"size"`
	Hex          string   `json:"hex"`
	OID          string   `json:"oid"`
	FileHash     string   `json:"file_hash"`
	Xorbs        []string `json:"xorbs"`
	Terms        []struct {
		Xorb          int    `json:"xorb"`
		ChunkStart    int    `json:"chunk_start"`
		ChunkEnd      int    `json:"chunk_end"`
		UnpackedBytes uint64 `json:"unpacked_bytes"`
	} `json:"terms"`
	NewXorbs []clientXorb `json:"new_xorbs"`
}

type clientInvalidEntry struct {
	Name         string `json:"name"`
	ObjectFormat string `json:"object_format"`
	Size         int64  `json:"size"`
	Hex          string `json:"hex"`
	Error        string `json:"error"`
}

// recipeOfTheInput is the recipe kind of an entry that has one term per xorb
// of its input, as chunkBlob makes.
const recipeOfTheInput = "xorbs of the input"

func loadClientVectors(t *testing.T) *clientVectors {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(clientVectorsFile))
	require.NoError(t, err)
	var v clientVectors
	require.NoError(t, json.Unmarshal(b, &v))
	require.NotEmpty(t, v.Inputs)
	require.NotEmpty(t, v.Entries)
	require.NotEmpty(t, v.InvalidEntries)
	return &v
}

// materialize rebuilds the bytes of each input by the rules in the file's
// notes.
func materialize(t *testing.T, inputs []clientInput) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(inputs))
	for _, in := range inputs {
		var data []byte
		for i, p := range in.Parts {
			switch {
			case p.SplitMix64 != nil:
				data = append(data, splitMix64Bytes(p.SplitMix64.Size, p.SplitMix64.Seed)...)
			case p.Fill != nil:
				data = append(data, bytes.Repeat([]byte{p.Fill.Byte}, p.Fill.Size)...)
			case p.Slice != nil:
				base, ok := out[p.Slice.Input]
				require.True(t, ok, "%s part %d: slice of unknown or later input %q", in.Name, i, p.Slice.Input)
				data = append(data, base[p.Slice.Start:p.Slice.End]...)
			default:
				t.Fatalf("%s part %d: no generator", in.Name, i)
			}
		}
		out[in.Name] = data
	}
	return out
}

func gitBlobID(h hash.Hash, data []byte) string {
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestClientVectors checks the chunker, the xorb builder and the meta 0x03
// encoder against entiredb's known answers. A mismatch is a contract break:
// chunks that do not deduplicate, or pushes that the server refuses.
func TestClientVectors(t *testing.T) {
	t.Parallel()
	v := loadClientVectors(t)
	require.Equal(t, targetChunkSize, v.Chunker.TargetSize)
	require.Equal(t, minChunkSize, v.Chunker.MinSize)
	require.Equal(t, maxChunkSize, v.Chunker.MaxSize)
	require.Equal(t, anchorModulus, v.AnchorModulus)

	data := materialize(t, v.Inputs)
	inputs := make(map[string]clientInput, len(v.Inputs))
	for _, in := range v.Inputs {
		inputs[in.Name] = in
		t.Run("input/"+in.Name, func(t *testing.T) {
			t.Parallel()
			checkClientInput(t, in, data[in.Name])
		})
	}
	for _, e := range v.Entries {
		t.Run("entry/"+e.Name, func(t *testing.T) {
			t.Parallel()
			in, ok := inputs[e.Input]
			require.True(t, ok, "unknown input %q", e.Input)
			checkClientEntry(t, e, in, data[e.Input])
		})
	}
	for _, e := range v.InvalidEntries {
		t.Run("invalid/"+e.Name, func(t *testing.T) {
			t.Parallel()
			checkInvalidEntry(t, e)
		})
	}
}

func checkClientInput(t *testing.T, in clientInput, data []byte) {
	t.Helper()
	require.Len(t, data, in.Size)
	require.Equal(t, in.SHA256, sha256Hex(data))
	require.Equal(t, in.GitSHA1, gitBlobID(sha1.New(), data))
	require.Equal(t, in.GitSHA256, gitBlobID(sha256.New(), data))

	chunks := splitAll(t, bytes.NewReader(data))
	ends := make([]int, len(chunks))
	hashes := make([]string, len(chunks))
	anchors := []int{}
	end := 0
	for i, c := range chunks {
		end += len(c.Data)
		ends[i] = end
		hashes[i] = c.Hash.String()
		if IsAnchor(c.Hash) {
			anchors = append(anchors, i)
		}
	}
	require.Equal(t, in.ChunkEnds, ends)
	require.Equal(t, in.ChunkHashes, hashes)
	require.Equal(t, in.Anchors, anchors)

	if in.Size == 0 {
		require.Equal(t, in.FileHash, xethash.File(nil).String())
		require.Empty(t, in.Xorbs)
		return
	}
	recipe, sealed := chunkBlob(t, data)
	require.Equal(t, in.FileHash, recipe.FileHash.String())
	require.Len(t, recipe.Terms, len(in.Xorbs), "one term per xorb of the input")
	byHash := make(map[xethash.Hash]*Xorb, len(sealed))
	for _, x := range sealed {
		byHash[x.Hash] = x
	}
	first := 0
	for i, want := range in.Xorbs {
		term := recipe.Terms[i]
		h := recipe.Xorbs[term.Xorb]
		require.Equal(t, want.Hash, h.String(), "xorb %d", i)
		require.Equal(t, want.FirstChunk, first, "xorb %d", i)
		require.Equal(t, 0, term.ChunkStart)
		require.Equal(t, want.Chunks, term.ChunkEnd)
		require.Equal(t, uint64(want.UnpackedSize), term.UnpackedBytes)
		checkClientXorb(t, want, byHash[h])
		first += want.Chunks
	}
	require.Equal(t, len(chunks), first)
}

// checkClientXorb compares a built xorb with its vector: the hash, the
// chunk table, and both serialized forms.
func checkClientXorb(t *testing.T, want clientXorb, x *Xorb) {
	t.Helper()
	require.NotNil(t, x, "xorb %s was not built", want.Hash)
	require.Equal(t, want.Hash, x.Hash.String())
	require.Len(t, x.Footer.ChunkHashes, want.Chunks)
	require.Equal(t, want.StoredEnds, x.Footer.ChunkBoundaryOffsets)
	require.Equal(t, want.UnpackedSize, int(x.Footer.UnpackedChunkOffsets[want.Chunks-1]))
	require.Len(t, x.Network, want.NetworkSize)
	require.Equal(t, want.NetworkSHA256, sha256Hex(x.Network))

	in, err := xorb.IngestNetwork(x.Network, x.Hash)
	require.NoError(t, err)
	require.Len(t, in.Complete, want.CompleteSize)
	require.Equal(t, want.CompleteSHA256, sha256Hex(in.Complete))
	if want.CompleteHex != "" {
		require.Equal(t, want.CompleteHex, hex.EncodeToString(in.Complete))
	}
}

func checkClientEntry(t *testing.T, e clientEntry, in clientInput, data []byte) {
	t.Helper()
	oid, err := hex.DecodeString(e.OID)
	require.NoError(t, err)
	switch e.ObjectFormat {
	case "sha1":
		require.Equal(t, in.GitSHA1, e.OID)
	case "sha256":
		require.Equal(t, in.GitSHA256, e.OID)
	default:
		t.Fatalf("unknown object format %q", e.ObjectFormat)
	}
	require.Equal(t, int64(in.Size), e.Size)
	require.Equal(t, in.FileHash, e.FileHash)

	r := &Recipe{Size: e.Size, FileHash: xethash.MustParse(e.FileHash)}
	for _, x := range e.Xorbs {
		r.Xorbs = append(r.Xorbs, xethash.MustParse(x))
	}
	for _, term := range e.Terms {
		r.Terms = append(r.Terms, Term{Xorb: term.Xorb, ChunkStart: term.ChunkStart, ChunkEnd: term.ChunkEnd, UnpackedBytes: term.UnpackedBytes})
	}
	got, err := AppendEntry(nil, oid, r)
	require.NoError(t, err)
	require.Equal(t, e.Hex, hex.EncodeToString(got))

	chunks := splitAll(t, bytes.NewReader(data))
	switch e.Recipe {
	case recipeOfTheInput:
		want, _ := chunkBlob(t, data)
		require.Equal(t, want, r)
	default:
		// "dedup against <base>": the vectors choose the terms. Check the
		// xorbs of the chunks that the base input does not hold.
		for _, nx := range e.NewXorbs {
			var b xorb.Builder
			for _, i := range nx.InputChunks {
				_, err := b.AddHashed(chunks[i].Data, chunks[i].Hash, xorb.Auto)
				require.NoError(t, err)
			}
			x, err := Seal(&b)
			require.NoError(t, err)
			checkClientXorb(t, nx, x)
		}
	}
}

// errUnrepresentable marks an invalid entry that AppendEntry cannot write:
// its layout differs from every entry the encoder makes.
var errUnrepresentable = errors.New("the encoder cannot write this layout")

// checkInvalidEntry asserts that AppendEntry refuses the recipe of an entry
// that entiredb refuses. An entry whose layout the encoder never writes is
// checked to be outside the encoder's output.
func checkInvalidEntry(t *testing.T, e clientInvalidEntry) {
	t.Helper()
	b, err := hex.DecodeString(e.Hex)
	require.NoError(t, err)
	oidSize := map[string]int{"sha1": 20, "sha256": 32}[e.ObjectFormat]
	require.NotZero(t, oidSize, "object format %q", e.ObjectFormat)

	oid, r, declared, err := decodeClientEntry(b, oidSize)
	require.Equal(t, e.Size, r.Size, "logical size")
	switch {
	case declared > MaxPayloadSize:
		// The vector holds only the header and the payload size. Refuse a
		// recipe whose valid payload is over the limit.
		r = oversizeRecipe(e.Size)
		_, err := AppendEntry(nil, make([]byte, oidSize), r)
		require.ErrorContains(t, err, "over the "+strconv.Itoa(MaxPayloadSize)+"-byte limit")
	case errors.Is(err, errUnrepresentable):
		t.Logf("%s: %v", e.Name, err)
	default:
		require.NoError(t, err)
		_, err := AppendEntry(nil, oid, r)
		require.Error(t, err, "server refuses with %q", e.Error)
	}
}

// decodeClientEntry parses a type-5 entry by the spec layout. It returns
// errUnrepresentable for a layout that AppendEntry never writes: another
// meta byte, a short payload, inline xorbs or bytes after them.
func decodeClientEntry(b []byte, oidSize int) (oid []byte, r *Recipe, declared uint64, err error) {
	c := b[0]
	r = &Recipe{Size: int64(c & 0x0f)}
	if (c>>4)&7 != ExternalObject {
		return nil, r, 0, fmt.Errorf("type %d", (c>>4)&7)
	}
	shift := 4
	b = b[1:]
	for c&0x80 != 0 {
		c, b = b[0], b[1:]
		r.Size |= int64(c&0x7f) << shift
		shift += 7
	}
	if b[0] != MetaClientChunked {
		return nil, r, 0, fmt.Errorf("meta %#x: %w", b[0], errUnrepresentable)
	}
	declared, k := binary.Uvarint(b[1:])
	if k <= 0 {
		return nil, r, 0, errors.New("bad payload size")
	}
	p := b[1+k:]
	if uint64(len(p)) != declared {
		return nil, r, declared, fmt.Errorf("payload of %d bytes, header says %d: %w", len(p), declared, errUnrepresentable)
	}
	d := payloadDecoder{p: p}
	oid = d.bytes(oidSize)
	r.FileHash = d.hash()
	for range d.uvarint() {
		r.Xorbs = append(r.Xorbs, d.hash())
	}
	for range d.uvarint() {
		r.Terms = append(r.Terms, Term{Xorb: int(d.uvarint()), ChunkStart: int(d.uvarint()), ChunkEnd: int(d.uvarint()), UnpackedBytes: d.uvarint()})
	}
	inline := d.uvarint()
	switch {
	case d.err != nil:
		return nil, r, declared, fmt.Errorf("%w: %w", d.err, errUnrepresentable)
	case inline != 0:
		return nil, r, declared, fmt.Errorf("%d inline xorbs: %w", inline, errUnrepresentable)
	case len(d.p) != 0:
		return nil, r, declared, fmt.Errorf("%d bytes after the inline count: %w", len(d.p), errUnrepresentable)
	}
	return oid, r, declared, nil
}

type payloadDecoder struct {
	p   []byte
	err error
}

func (d *payloadDecoder) bytes(n int) []byte {
	if d.err != nil || len(d.p) < n {
		d.err = errors.New("truncated payload")
		return make([]byte, n)
	}
	out := d.p[:n]
	d.p = d.p[n:]
	return out
}

func (d *payloadDecoder) hash() xethash.Hash {
	return xethash.Hash(d.bytes(xethash.Size))
}

func (d *payloadDecoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, k := binary.Uvarint(d.p)
	if k <= 0 {
		d.err = errors.New("bad uvarint")
		return 0
	}
	d.p = d.p[k:]
	return v
}

// oversizeRecipe returns a recipe of size bytes that passes every check but
// the payload limit: one term per distinct xorb, so the xorb table alone is
// over MaxPayloadSize.
func oversizeRecipe(size int64) *Recipe {
	n := MaxPayloadSize/xethash.Size + 1
	r := &Recipe{Size: size}
	for i := range n {
		var h xethash.Hash
		binary.LittleEndian.PutUint64(h[:], uint64(i)+1)
		r.Xorbs = append(r.Xorbs, h)
		r.Terms = append(r.Terms, Term{Xorb: i, ChunkStart: 0, ChunkEnd: 1, UnpackedBytes: 1})
	}
	r.Terms[n-1].UnpackedBytes = uint64(size) - uint64(n-1)
	return r
}
