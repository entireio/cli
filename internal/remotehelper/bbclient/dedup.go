package bbclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jmwoliver/xet-go/xethash"
)

// AnchorHit is a requested anchor chunk that the repo holds.
type AnchorHit struct {
	Chunk xethash.Hash
	// Xorb is the permanent xorb that holds the chunk.
	Xorb xethash.Hash
	// Index is the position of the chunk in Xorb.
	Index int
}

// XorbState is the state of a requested xorb.
type XorbState struct {
	Xorb  xethash.Hash
	State State
}

// DedupResult is the merged answer to a dedup query.
type DedupResult struct {
	// Anchors holds the hits whose footer the server sent. The server may
	// answer only some anchors, so a missing hit is no proof that the repo
	// lacks the chunk.
	Anchors []AnchorHit
	// Xorbs holds the state of each requested xorb. It carries no footer:
	// the client already knows the chunks of a xorb that it asks about.
	Xorbs []XorbState
	// Footers holds the footer of each xorb that an accepted anchor hit
	// names, and no other.
	Footers map[xethash.Hash][]FooterChunk
	// Truncated reports that the server dropped the hits on some xorbs to
	// bound its answer. A caller may ask again for the anchors without a
	// hit.
	Truncated bool
}

type dedupRequest struct {
	Anchors []textHash `json:"anchors,omitempty"`
	Xorbs   []textHash `json:"xorbs,omitempty"`
}

type dedupResponse struct {
	Anchors []struct {
		Chunk textHash `json:"chunk"`
		Xorb  textHash `json:"xorb"`
		Index int      `json:"index"`
	} `json:"anchors"`
	Xorbs []struct {
		Xorb  textHash `json:"xorb"`
		State State    `json:"state"`
	} `json:"xorbs"`
	Footers map[textHash]struct {
		Chunks []wireChunk `json:"chunks"`
	} `json:"footers"`
	Truncated bool `json:"truncated"`
}

// Dedup asks the repo which anchors and xorbs it holds. It sends at most
// MaxDedupEntries of each list per request, and merges the answers. It
// checks every footer that a hit uses against its xorb hash, and every
// anchor hit against its footer. It drops an anchor hit without a footer:
// the server caps the footers of one answer, and a caller only
// deduplicates against chunks whose footer it has. It drops a footer that
// no hit names, and refuses an answer with more footers than the server
// sends.
func (c *Client) Dedup(ctx context.Context, anchors, xorbs []xethash.Hash) (*DedupResult, error) {
	res := &DedupResult{Footers: map[xethash.Hash][]FooterChunk{}}
	for i := 0; i < len(anchors) || i < len(xorbs); i += MaxDedupEntries {
		if err := c.dedupBatch(ctx, window(anchors, i), window(xorbs, i), res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// window returns at most MaxDedupEntries entries of hs from i.
func window(hs []xethash.Hash, i int) []xethash.Hash {
	if i >= len(hs) {
		return nil
	}
	return hs[i:min(i+MaxDedupEntries, len(hs))]
}

func (c *Client) dedupBatch(ctx context.Context, anchors, xorbs []xethash.Hash, res *DedupResult) error {
	req := dedupRequest{Anchors: make([]textHash, len(anchors)), Xorbs: make([]textHash, len(xorbs))}
	for i, h := range anchors {
		req.Anchors[i] = textHash(h)
	}
	for i, h := range xorbs {
		req.Xorbs[i] = textHash(h)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("bbclient: encode dedup query: %w", err)
	}
	var wire dedupResponse
	if err := c.do(ctx, http.MethodPost, "/bb/dedup", "application/json", body, maxDedupResponse, &wire); err != nil {
		return err
	}

	if len(wire.Footers) > maxDedupFooters {
		return fmt.Errorf("bbclient: dedup answered %d footers, over the %d-footer limit", len(wire.Footers), maxDedupFooters)
	}
	asked := make(map[xethash.Hash]bool, len(anchors))
	for _, h := range anchors {
		asked[h] = true
	}
	// A footer counts only for a xorb that an accepted hit names. The
	// caller records such a xorb as permanent, so a footer that the server
	// adds on its own must not reach it.
	footers := map[xethash.Hash][]FooterChunk{}
	for _, a := range wire.Anchors {
		chunk, x := xethash.Hash(a.Chunk), xethash.Hash(a.Xorb)
		if !asked[chunk] {
			return fmt.Errorf("bbclient: dedup answered anchor %s, which was not asked", chunk)
		}
		f, ok := footers[x]
		if !ok {
			wf, sent := wire.Footers[a.Xorb]
			if !sent {
				continue
			}
			var err error
			if f, err = parseFooter(x, wf.Chunks); err != nil {
				return err
			}
			footers[x] = f
		}
		if a.Index < 0 || a.Index >= len(f) || chunk != f[a.Index].Hash {
			return fmt.Errorf("bbclient: dedup anchor %s at xorb %s chunk %d does not match the footer", chunk, x, a.Index)
		}
		res.Anchors = append(res.Anchors, AnchorHit{Chunk: chunk, Xorb: x, Index: a.Index})
	}
	clear(asked)
	for _, h := range xorbs {
		asked[h] = true
	}
	for _, s := range wire.Xorbs {
		x := xethash.Hash(s.Xorb)
		if !asked[x] {
			return fmt.Errorf("bbclient: dedup answered xorb %s, which was not asked", x)
		}
		switch s.State {
		case StateMissing, StateStaged, StatePermanent:
		default:
			return fmt.Errorf("bbclient: dedup answered xorb %s with state %q", x, s.State)
		}
		res.Xorbs = append(res.Xorbs, XorbState{Xorb: x, State: s.State})
	}
	for x, f := range footers {
		res.Footers[x] = f
	}
	res.Truncated = res.Truncated || wire.Truncated
	return nil
}
