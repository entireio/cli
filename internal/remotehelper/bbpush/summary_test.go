package bbpush

import (
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbclient"
)

func TestPushStatsLine(t *testing.T) {
	t.Parallel()
	a, b := xethash.Chunk([]byte("xorb a")), xethash.Chunk([]byte("xorb b"))
	server := xethash.Chunk([]byte("server xorb"))
	staged := func(h xethash.Hash) *bbclient.UploadResult {
		return &bbclient.UploadResult{Xorb: h, State: bbclient.StateStaged}
	}
	cases := map[string]struct {
		blobs   []largeBlob
		built   map[xethash.Hash]int64
		packed  int64
		named   []xethash.Hash
		results map[xethash.Hash]*bbclient.UploadResult
		want    string
	}{
		"all chunks new": {
			blobs:   []largeBlob{{size: 3 << 20}, {size: 1 << 20}},
			built:   map[xethash.Hash]int64{a: 2_000_000, b: 1_900_000},
			packed:  4 << 20,
			named:   []xethash.Hash{a, b},
			results: map[xethash.Hash]*bbclient.UploadResult{a: staged(a), b: staged(b)},
			want:    "Chunked 2 large blobs (4.00 MiB): 2 of 2 xorbs uploaded (3.72 MiB); 0% of chunk bytes deduplicated",
		},
		"full dedup against a server xorb": {
			blobs: []largeBlob{{size: 3 << 20}},
			built: map[xethash.Hash]int64{},
			named: []xethash.Hash{server},
			want:  "Chunked 1 large blob (3.00 MiB): 0 of 1 xorb uploaded, 1 reused from the server; 100% of chunk bytes deduplicated",
		},
		"upload that the server already held": {
			blobs:   []largeBlob{{size: 2 << 20}},
			built:   map[xethash.Hash]int64{a: 2_100_000},
			packed:  2 << 20,
			named:   []xethash.Hash{a},
			results: map[xethash.Hash]*bbclient.UploadResult{a: {Xorb: a, State: bbclient.StatePermanent}},
			want:    "Chunked 1 large blob (2.00 MiB): 1 of 1 xorb uploaded (2.00 MiB, 1 already on the server); 0% of chunk bytes deduplicated",
		},
		"partial dedup": {
			blobs:   []largeBlob{{size: 600 << 20}, {size: 526 << 20}},
			built:   map[xethash.Hash]int64{a: 300 << 20, b: 400 << 20},
			packed:  698 << 20,
			named:   []xethash.Hash{server, a, b},
			results: map[xethash.Hash]*bbclient.UploadResult{a: staged(a), b: staged(b)},
			want:    "Chunked 2 large blobs (1.09 GiB): 2 of 3 xorbs uploaded (700.00 MiB), 1 reused from the server; 38% of chunk bytes deduplicated",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newPushStats(tc.blobs)
			s.built, s.newBytes = tc.built, tc.packed
			require.Equal(t, tc.want, s.line(tc.named, tc.results))
		})
	}
}

// TestHumanBytes holds humanBytes to the output of git's
// strbuf_humanise_bytes for the same values. git truncates gibibytes, so
// 1.5 GiB prints as 1.49 GiB.
func TestHumanBytes(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{
		0:                   "0 bytes",
		1:                   "1 byte",
		1024:                "1024 bytes",
		1025:                "1.00 KiB",
		1536:                "1.50 KiB",
		1<<20 + 1:           "1.00 MiB",
		480 << 20:           "480.00 MiB",
		1 << 30:             "1024.00 MiB",
		1<<30 + 1<<29:       "1.49 GiB",
		1181116006:          "1.09 GiB",
		5<<30 + 1<<30/10*3:  "5.29 GiB",
		1<<20 + 1<<20/100*7: "1.07 MiB",
	} {
		require.Equal(t, want, humanBytes(n), "%d bytes", n)
	}
}
