package bbpush

import (
	"context"
	"fmt"

	"github.com/jmwoliver/xet-go/xethash"

	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// maxAnchorRounds bounds the dedup queries of one push. The server answers
// the anchors of at most 64 xorbs per query and flags the rest as
// truncated; each further round asks for the anchors without a hit.
const maxAnchorRounds = 4

// loadKnownXorbs adds to ix the xorbs of the repo that hold chunks of the
// push. It takes the xorbs of the local store that hold a pushed chunk, and
// asks the server whether they still exist. For the other anchor chunks it
// asks the server which permanent xorb holds them. An anchor without an
// answer is a new chunk.
func loadKnownXorbs(ctx context.Context, store *bbcache.Store, client *bbclient.Client, ix *chunkIndex, blobs []*blobChunks) error {
	local, records := localCandidates(store, pushedChunks(blobs))
	anchors := anchorsToAsk(blobs, records)
	if len(anchors) == 0 && len(local) == 0 {
		return nil
	}
	res, err := client.Dedup(ctx, anchors, local)
	if err != nil {
		return fmt.Errorf("dedup query: %w", err)
	}
	for _, s := range res.Xorbs {
		if s.State != bbclient.StateMissing {
			ix.addStored(s.Xorb, footerHashes(records[s.Xorb].Chunks))
		}
	}
	for round := 1; ; round++ {
		if err := store.RecordDedup(res); err != nil {
			debuglog.Printf("bbpush: record dedup answer: %v", err)
		}
		for h, f := range res.Footers {
			ix.addStored(h, footerHashes(f))
		}
		anchors = withoutHits(anchors, res.Anchors)
		if !res.Truncated || len(anchors) == 0 || round == maxAnchorRounds {
			return nil
		}
		if res, err = client.Dedup(ctx, anchors, nil); err != nil {
			debuglog.Printf("bbpush: dedup query round %d: %v", round+1, err)
			return nil
		}
	}
}

func pushedChunks(blobs []*blobChunks) map[xethash.Hash]bool {
	pushed := map[xethash.Hash]bool{}
	for _, b := range blobs {
		for _, h := range b.hashes {
			pushed[h] = true
		}
	}
	return pushed
}

// anchorsToAsk returns the distinct anchor chunks of the push that no local
// record holds, in push order.
func anchorsToAsk(blobs []*blobChunks, records map[xethash.Hash]bbcache.Record) []xethash.Hash {
	held := map[xethash.Hash]bool{}
	for _, r := range records {
		for _, c := range r.Chunks {
			held[c.Hash] = true
		}
	}
	var anchors []xethash.Hash
	for _, b := range blobs {
		for _, h := range b.hashes {
			if bbchunk.IsAnchor(h) && !held[h] {
				held[h] = true
				anchors = append(anchors, h)
			}
		}
	}
	return anchors
}

func withoutHits(anchors []xethash.Hash, hits []bbclient.AnchorHit) []xethash.Hash {
	hit := make(map[xethash.Hash]bool, len(hits))
	for _, h := range hits {
		hit[h.Chunk] = true
	}
	out := anchors[:0:0]
	for _, a := range anchors {
		if !hit[a] {
			out = append(out, a)
		}
	}
	return out
}

// localCandidates returns the xorbs with a valid local record that hold at
// least one pushed chunk, and their records.
func localCandidates(store *bbcache.Store, pushed map[xethash.Hash]bool) ([]xethash.Hash, map[xethash.Hash]bbcache.Record) {
	var hs []xethash.Hash
	records := map[xethash.Hash]bbcache.Record{}
	for _, h := range store.TrustedXorbs() {
		r, ok := store.Record(h)
		if !ok || !holdsAny(r.Chunks, pushed) {
			continue
		}
		hs = append(hs, h)
		records[h] = r
	}
	return hs, records
}

func holdsAny(chunks []bbclient.FooterChunk, set map[xethash.Hash]bool) bool {
	for _, c := range chunks {
		if set[c.Hash] {
			return true
		}
	}
	return false
}

func footerHashes(f []bbclient.FooterChunk) []xethash.Hash {
	hs := make([]xethash.Hash, len(f))
	for i, c := range f {
		hs[i] = c.Hash
	}
	return hs
}
