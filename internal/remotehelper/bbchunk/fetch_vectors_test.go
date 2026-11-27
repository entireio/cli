package bbchunk

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// fetchVectorsFile is a copy of entiredb's internal/xet/testdata/
// fetch_vectors.json, the known answers for stored chunked entries of the
// client-chunked fetch spec. Copy the file again when entiredb changes it. Do
// not edit it here.
const fetchVectorsFile = "testdata/fetch_vectors.json"

type fetchVectors struct {
	Entries        []fetchEntry         `json:"entries"`
	InvalidEntries []clientInvalidEntry `json:"invalid_entries"`
}

type fetchEntry struct {
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
		Xorb          uint32 `json:"xorb"`
		ChunkStart    uint32 `json:"chunk_start"`
		ChunkEnd      uint32 `json:"chunk_end"`
		UnpackedBytes uint64 `json:"unpacked_bytes"`
		FirstByte     uint64 `json:"first_byte"`
		LastByte      uint64 `json:"last_byte"`
	} `json:"terms"`
}

func loadFetchVectors(t *testing.T) *fetchVectors {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(fetchVectorsFile))
	require.NoError(t, err)
	var v fetchVectors
	require.NoError(t, json.Unmarshal(b, &v))
	require.NotEmpty(t, v.Entries)
	require.NotEmpty(t, v.InvalidEntries)
	return &v
}

func hashSizeOf(t *testing.T, objectFormat string) int {
	t.Helper()
	switch objectFormat {
	case "sha1":
		return sha1.Size
	case "sha256":
		return sha256.Size
	}
	t.Fatalf("unknown object format %q", objectFormat)
	return 0
}

// decodeStoredEntry splits a complete stored entry into its logical size and
// its payload, with the frame checks of the fetch path.
func decodeStoredEntry(b []byte, hashSize int) (int64, []byte, error) {
	if len(b) == 0 {
		return 0, nil, errors.New("empty entry")
	}
	c := b[0]
	typ, size := (c>>4)&7, int64(c&0x0f)
	i := 1
	for shift := 4; c&0x80 != 0; shift += 7 {
		if i == len(b) || shift > 56 {
			return 0, nil, errors.New("bad entry header")
		}
		c = b[i]
		i++
		size |= int64(c&0x7f) << shift
	}
	if typ != ExternalObject {
		return 0, nil, fmt.Errorf("entry type %d, want %d", typ, ExternalObject)
	}
	if i == len(b) {
		return 0, nil, errors.New("no meta byte")
	}
	meta := b[i]
	declared, k := binary.Uvarint(b[i+1:])
	if k <= 0 {
		return 0, nil, errors.New("bad payload size")
	}
	if err := CheckChunkedFrame(meta, declared, hashSize); err != nil {
		return 0, nil, err
	}
	payload := b[i+1+k:]
	switch {
	case uint64(len(payload)) < declared:
		return 0, nil, fmt.Errorf("payload is %d bytes, have %d", declared, len(payload))
	case uint64(len(payload)) > declared:
		return 0, nil, fmt.Errorf("%d bytes after the payload", uint64(len(payload))-declared)
	}
	return size, payload, nil
}

// TestFetchVectors checks the parser of stored chunked entries against
// entiredb's known answers. A mismatch is a contract break: fetches that
// refuse good recipes, or that accept bad ones. TestFetchVectorsEncode
// checks the encoder of the tests against the same answers.
func TestFetchVectors(t *testing.T) {
	t.Parallel()
	v := loadFetchVectors(t)
	for _, e := range v.Entries {
		t.Run("entry/"+e.Name, func(t *testing.T) {
			t.Parallel()
			checkFetchEntry(t, e)
		})
	}
	for _, e := range v.InvalidEntries {
		t.Run("invalid/"+e.ObjectFormat+"/"+e.Name, func(t *testing.T) {
			t.Parallel()
			b, err := hex.DecodeString(e.Hex)
			require.NoError(t, err)
			hashSize := hashSizeOf(t, e.ObjectFormat)
			size, payload, err := decodeStoredEntry(b, hashSize)
			if err == nil {
				require.Equal(t, e.Size, size)
				_, err = ParseChunkedPayload(payload, hashSize, size)
			}
			require.ErrorContains(t, err, e.Error)
		})
	}
}

// checkFetchEntry checks that a valid vector parses to its fields, and
// returns the parsed recipe.
func checkFetchEntry(t *testing.T, e fetchEntry) *ChunkedPayload {
	t.Helper()
	b, err := hex.DecodeString(e.Hex)
	require.NoError(t, err)
	hashSize := hashSizeOf(t, e.ObjectFormat)
	size, payload, err := decodeStoredEntry(b, hashSize)
	require.NoError(t, err)
	require.Equal(t, e.Size, size)
	got, err := ParseChunkedPayload(payload, hashSize, size)
	require.NoError(t, err)

	require.Equal(t, e.OID, hex.EncodeToString(got.OID))
	require.Equal(t, e.FileHash, got.FileHash.String())
	gotXorbs := make([]string, len(got.Xorbs))
	for i, h := range got.Xorbs {
		gotXorbs[i] = h.String()
	}
	require.Equal(t, e.Xorbs, gotXorbs)
	require.Len(t, got.Terms, len(e.Terms))
	for i, want := range e.Terms {
		require.Equal(t, ChunkedTerm{
			Xorb: want.Xorb, ChunkStart: want.ChunkStart, ChunkEnd: want.ChunkEnd,
			UnpackedBytes: want.UnpackedBytes, FirstByte: want.FirstByte, LastByte: want.LastByte,
		}, got.Terms[i], "term %d", i)
	}
	return got
}
