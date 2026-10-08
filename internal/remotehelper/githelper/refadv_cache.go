package githelper

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// refAdvCache memoizes the receive-pack ref advertisement across a single
// helper session so the "push" command reuses the exact ref snapshot that
// "list for-push" fetched. This mirrors remote-curl.c's discovery cache
// (get_refs/last_refs): Git builds its remote_refs list from the
// "list for-push" advertisement, then runs the pre-push hook, then issues
// "push". The Entire pre-push hook pushes per-checkpoint refs to the same
// remote in that window, so a fresh info/refs at push time would hand
// send-pack a ref Git never asked to push. send-pack then reports
// `error <ref> no match` and Git — not finding it in remote_refs — warns
// `helper reported unexpected status of <ref>` (ENCLI-267). Reusing the
// snapshot keeps that phantom ref out of send-pack's view.
//
// Only the receive-pack (for-push) advertisement is cached; upload-pack and
// v2 fetches pass straight through, matching remote-curl's per-for_push cache.
type refAdvCache struct {
	receivePack []byte
	cached      bool
}

// infoRefs returns the ref advertisement for service. The receive-pack
// advertisement is fetched from the Transport once and replayed from an
// in-memory buffer on subsequent calls; every other service is fetched fresh.
func (c *refAdvCache) infoRefs(ctx context.Context, t Transport, service string) (io.ReadCloser, error) {
	if service != serviceReceivePack {
		rc, err := t.InfoRefs(ctx, service)
		if err != nil {
			return nil, fmt.Errorf("fetch %s advertisement: %w", service, err)
		}
		return rc, nil
	}
	if c.cached {
		return io.NopCloser(bytes.NewReader(c.receivePack)), nil
	}
	rc, err := t.InfoRefs(ctx, service)
	if err != nil {
		return nil, fmt.Errorf("fetch %s advertisement: %w", service, err)
	}
	defer rc.Close()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("buffer %s advertisement: %w", service, err)
	}
	c.receivePack = buf
	c.cached = true
	return io.NopCloser(bytes.NewReader(buf)), nil
}
