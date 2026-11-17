package githelper

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// TestFakeBBParsesClientVectors holds the fake server's entry parser to
// entiredb's golden vectors: it takes every valid entry with its fields, and
// refuses every invalid one. Then a push that the fake takes is one that
// entiredb takes.
func TestFakeBBParsesClientVectors(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.FromSlash("../bbchunk/testdata/client_vectors.json"))
	require.NoError(t, err)
	var v struct {
		Entries []struct {
			Name         string   `json:"name"`
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
		} `json:"entries"`
		InvalidEntries []struct {
			Name         string `json:"name"`
			ObjectFormat string `json:"object_format"`
			Hex          string `json:"hex"`
			Error        string `json:"error"`
		} `json:"invalid_entries"`
	}
	require.NoError(t, json.Unmarshal(b, &v))
	require.NotEmpty(t, v.Entries)
	require.NotEmpty(t, v.InvalidEntries)
	oidSize := map[string]int{"sha1": 20, "sha256": 32}

	for _, e := range v.Entries {
		t.Run("valid/"+e.Name, func(t *testing.T) {
			t.Parallel()
			raw, err := hex.DecodeString(e.Hex)
			require.NoError(t, err)
			got, n, err := parseClientEntry(raw, oidSize[e.ObjectFormat], 0)
			require.NoError(t, err)
			require.Len(t, raw, n)
			require.Equal(t, e.Size, got.size)
			require.Equal(t, e.OID, hex.EncodeToString(got.oid))
			require.Equal(t, e.FileHash, got.fileHash.String())
			require.Len(t, got.xorbs, len(e.Xorbs))
			for i, x := range e.Xorbs {
				require.Equal(t, xethash.MustParse(x), got.xorbs[i])
			}
			require.Len(t, got.terms, len(e.Terms))
			for i, term := range e.Terms {
				require.Equal(t, bbchunk.Term{Xorb: term.Xorb, ChunkStart: term.ChunkStart, ChunkEnd: term.ChunkEnd, UnpackedBytes: term.UnpackedBytes}, got.terms[i])
			}
			_, _, err = parseClientEntry(raw, oidSize[e.ObjectFormat], e.Size+1)
			require.Error(t, err, "an entry below the threshold")
		})
	}
	for _, e := range v.InvalidEntries {
		t.Run("invalid/"+e.Name, func(t *testing.T) {
			t.Parallel()
			raw, err := hex.DecodeString(e.Hex)
			require.NoError(t, err)
			_, _, err = parseClientEntry(raw, oidSize[e.ObjectFormat], 0)
			require.Error(t, err, "entiredb refuses with %q", e.Error)
		})
	}
	// Every prefix of a valid entry is refused without a panic.
	raw, err := hex.DecodeString(v.Entries[0].Hex)
	require.NoError(t, err)
	for i := range raw {
		_, _, err := parseClientEntry(raw[:i], 20, 0)
		require.Error(t, err, "prefix of %d bytes", i)
	}
}
