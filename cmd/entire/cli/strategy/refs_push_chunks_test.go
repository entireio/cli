package strategy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
)

func fakeRefNames(n int) []plumbing.ReferenceName {
	refs := make([]plumbing.ReferenceName, 0, n)
	for i := range n {
		refs = append(refs, plumbing.ReferenceName(fmt.Sprintf("refs/entire/checkpoints/%02x/x", i)))
	}
	return refs
}

// stallingPush fakes the batch push: a push carrying stallRef blocks until its
// ctx ends; every other push lands. It records each push's ref count.
func stallingPush(t *testing.T, stallRef plumbing.ReferenceName) *[]int {
	t.Helper()
	var mu sync.Mutex
	var sizes []int
	swapCheckpointRefsPush(t, func(ctx context.Context, opts remote.PushOptions) (remote.PushResult, error) {
		mu.Lock()
		sizes = append(sizes, len(opts.RefSpecs))
		mu.Unlock()
		for _, spec := range opts.RefSpecs {
			if strings.HasPrefix(spec, stallRef.String()+":") {
				<-ctx.Done()
				return remote.PushResult{}, ctx.Err()
			}
		}
		return remote.PushResult{}, nil
	})
	return &sizes
}

// assertEveryRefAccounted: a chunked pass may land, stall or leave a ref
// untried, but never drop one.
func assertEveryRefAccounted(t *testing.T, refs, landed []plumbing.ReferenceName, res chunkPushResult) {
	t.Helper()
	got := slices.Concat(landed, res.stalled, res.untried, res.failed)
	assert.ElementsMatch(t, refs, got)
}

// A stalled chunk shrinks the chunks after it in the same pass: a link too
// slow for one chunk should not stall every chunk behind it at the same size.
func TestPushRefChunks_StallShrinksLaterChunks(t *testing.T) {
	refs := fakeRefNames(14)
	sizes := stallingPush(t, refs[0])
	var landed []plumbing.ReferenceName

	res := pushRefChunks(t.Context(), "origin", refs, 8, 50*time.Millisecond, nil,
		func(chunk []plumbing.ReferenceName) { landed = append(landed, chunk...) })

	assert.Equal(t, []int{8, 2, 2, 2}, *sizes)
	assert.Equal(t, refs[:8], res.stalled)
	assert.Equal(t, 2, res.size, "the shrunk size is reported")
	assertEveryRefAccounted(t, refs, landed, res)
}

func TestPushRefChunks_StallMidListDropsNoRef(t *testing.T) {
	refs := fakeRefNames(13)
	sizes := stallingPush(t, refs[4])
	var landed []plumbing.ReferenceName

	res := pushRefChunks(t.Context(), "origin", refs, 4, 50*time.Millisecond, nil,
		func(chunk []plumbing.ReferenceName) { landed = append(landed, chunk...) })

	assert.Equal(t, []int{4, 4, 2, 2, 1}, *sizes)
	assert.Equal(t, refs[4:8], res.stalled)
	assert.Equal(t, 9, res.landed)
	assertEveryRefAccounted(t, refs, landed, res)
}

func TestPushRefChunks_BeforeChunkRunsBetweenChunks(t *testing.T) {
	refs := fakeRefNames(5)
	stallingPush(t, "refs/none")
	var events []string
	swapped := pushCheckpointRefs
	pushCheckpointRefs = func(ctx context.Context, opts remote.PushOptions) (remote.PushResult, error) {
		events = append(events, "push")
		return swapped(ctx, opts)
	}

	pushRefChunks(t.Context(), "origin", refs, 2, 0,
		func(context.Context) { events = append(events, "wait") },
		func([]plumbing.ReferenceName) {})

	assert.Equal(t, []string{"push", "wait", "push", "wait", "push"}, events)
}

// A wait that spends the budget ends the pass cleanly: the rest is untried,
// not a chunk failed on a dead context.
func TestPushRefChunks_BeforeChunkPastBudgetStopsCleanly(t *testing.T) {
	refs := fakeRefNames(4)
	stallingPush(t, "refs/none")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	res := pushRefChunks(ctx, "origin", refs, 2, 0,
		func(ctx context.Context) { <-ctx.Done() },
		func([]plumbing.ReferenceName) {})

	assert.Equal(t, 2, res.landed)
	assert.Empty(t, res.failed)
	assert.Equal(t, refs[2:], res.untried)
	assert.Contains(t, res.stopReason, "exhausted")
}

// The remembered size follows a pass that shrank after a stall, rather than
// quartering the starting size again or keeping it because later chunks landed.
func TestAdaptChunkSize_PersistsSizeShrunkInPass(t *testing.T) {
	t.Parallel()
	queue := checkpoint.NewPushQueue(t.TempDir())
	require.NoError(t, queue.SetChunkSizeHint(8))

	batch := chunkPushResult{landed: 6, stalled: fakeRefNames(8), size: 2}
	adaptChunkSize(t.Context(), queue, 8, batch, false)

	assert.Equal(t, 2, queue.ChunkSizeHint(checkpointRefPushChunkSize))
}
