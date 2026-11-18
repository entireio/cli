package bbpush

import (
	"fmt"
	"strings"

	"github.com/jmwoliver/xet-go/xethash"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
)

// pushStats counts what the chunking and the uploads of one push did, for
// the summary line that Push prints after the uploads.
type pushStats struct {
	blobs     int
	blobBytes int64
	// newBytes is the chunk data that went into the xorbs that this push
	// built. The rest of the blob bytes came from xorbs that the server
	// holds, or from chunks that the push repeats.
	newBytes int64
	// built maps each xorb that this push built to its network size.
	built map[xethash.Hash]int64
}

func newPushStats(blobs []largeBlob) *pushStats {
	s := &pushStats{blobs: len(blobs), built: map[xethash.Hash]int64{}}
	for _, b := range blobs {
		s.blobBytes += b.size
	}
	return s
}

// addBuilt counts a xorb that this push sealed.
func (s *pushStats) addBuilt(x *bbchunk.Xorb) {
	n := len(x.Footer.UnpackedChunkOffsets)
	s.newBytes += int64(x.Footer.UnpackedChunkOffsets[n-1])
	s.built[x.Hash] = int64(len(x.Network))
}

// line returns the summary of a push whose entries name the xorbs named,
// with the answers to its uploads. For example:
//
//	Chunked 2 large blobs (1.10 GiB): 7 of 9 xorbs uploaded (480.00 MiB), 2 reused from the server; 38% of chunk bytes deduplicated
//
// A xorb that the push built and uploaded counts as uploaded, also when
// the server answered that it already held it. Every other named xorb is
// one that the dedup query found on the server.
func (s *pushStats) line(named []xethash.Hash, results map[xethash.Hash]*bbclient.UploadResult) string {
	var uploaded, held, reused int
	var sent int64
	for _, h := range named {
		r, ok := results[h]
		if !ok {
			if _, built := s.built[h]; !built {
				reused++
			}
			continue
		}
		uploaded++
		sent += s.built[h]
		if r.State == bbclient.StatePermanent {
			held++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chunked %d large %s (%s): %d of %d %s uploaded", s.blobs, plural(s.blobs, "blob"), humanBytes(s.blobBytes),
		uploaded, len(named), plural(len(named), "xorb"))
	switch {
	case uploaded > 0 && held > 0:
		fmt.Fprintf(&b, " (%s, %d already on the server)", humanBytes(sent), held)
	case uploaded > 0:
		fmt.Fprintf(&b, " (%s)", humanBytes(sent))
	}
	if reused > 0 {
		fmt.Fprintf(&b, ", %d reused from the server", reused)
	}
	fmt.Fprintf(&b, "; %d%% of chunk bytes deduplicated", dedupPercent(s.blobBytes, s.newBytes))
	return b.String()
}

// dedupPercent returns the share of total bytes that the push did not pack
// into new xorbs, rounded down.
func dedupPercent(total, packed int64) int64 {
	if total <= 0 || packed >= total {
		return 0
	}
	return (total - packed) * 100 / total
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// humanBytes formats n as git's strbuf_humanise_bytes does, so the line
// reads like git's own "Writing objects" totals.
func humanBytes(n int64) string {
	switch {
	case n > 1<<30:
		return fmt.Sprintf("%d.%02d GiB", n>>30, (n&(1<<30-1))/10737419)
	case n > 1<<20:
		x := n + 5243
		return fmt.Sprintf("%d.%02d MiB", x>>20, ((x&(1<<20-1))*100)>>20)
	case n > 1<<10:
		x := n + 5
		return fmt.Sprintf("%d.%02d KiB", x>>10, ((x&(1<<10-1))*100)>>10)
	case n == 1:
		return "1 byte"
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
