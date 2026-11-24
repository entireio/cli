//go:build unix

package strategy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// blockTranscriptRead points the fixture's transcript at a FIFO, so finalize's
// local preparation blocks on the read. Once finalize is blocked there,
// whileBlocked runs, and then the transcript is written and the read returns.
// waitBlocked fails the test if finalize never reached the read, so a test
// cannot pass on an earlier exit.
func (f *finalizeFixture) blockTranscriptRead(t *testing.T, whileBlocked func()) (waitBlocked func()) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), paths.TranscriptFileName)
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	f.state.TranscriptPath = fifo

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Opening a FIFO for writing waits until finalize opens it to read.
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open transcript FIFO: %v", err)
			return
		}
		defer w.Close()
		whileBlocked()
		if _, err := w.WriteString(testTranscriptPromptResponse); err != nil {
			t.Errorf("write transcript FIFO: %v", err)
		}
	}()
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("finalize never reached the transcript read")
		}
	}
}

// precomputedFinalTranscript reports whether finalize precomputed the blobs for
// the final transcript: the content-hash blob exists only once it has.
func (f *finalizeFixture) precomputedFinalTranscript(t *testing.T) bool {
	t.Helper()
	contentHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(testTranscriptPromptResponse)))
	// An unsaved encoded object only computes the hash; nothing is stored.
	probe := f.repo.Storer.NewEncodedObject()
	probe.SetType(plumbing.BlobObject)
	w, err := probe.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte(contentHash))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return f.repo.Storer.HasEncodedObject(probe.Hash()) == nil
}

// A deadline that passes during local preparation must stop finalize before
// the store open and the precompute, not only before the writes: the time they
// take is the headroom the hook needs to save state.
func TestFinalizeAllTurnCheckpoints_DeadlinePassesDuringPreparation(t *testing.T) {
	f := newFinalizeFixture(t)
	turnIDs := append([]string(nil), f.state.TurnCheckpointIDs...)
	// Far enough out for finalize to pass its entry check and reach the read;
	// waitBlocked fails the test if it does not.
	deadline := time.Now().Add(2 * time.Second)
	waitBlocked := f.blockTranscriptRead(t, func() {
		time.Sleep(time.Until(deadline) + 50*time.Millisecond)
	})

	s := NewManualCommitStrategy()
	fetches := 0
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error {
		fetches++
		return f.restore(refName)
	}
	errCount := s.finalizeAllTurnCheckpoints(context.Background(), f.state, deadline)
	waitBlocked()

	require.Zero(t, errCount, "an interruption is reported as capture degradation, not as errors")
	require.NotNil(t, f.state.CaptureDegradedAt)
	require.Zero(t, fetches, "no fetch may start once the deadline has passed")
	require.Equal(t, turnIDs, f.state.TurnCheckpointIDs, "the IDs survive for a repeated Stop")
	require.False(t, f.precomputedFinalTranscript(t), "precompute must not run once the deadline has passed")

	// The probe above can see a precompute: a finalize that runs does one.
	f.state.TranscriptPath = filepath.Join(t.TempDir(), paths.TranscriptFileName)
	require.NoError(t, os.WriteFile(f.state.TranscriptPath, []byte(testTranscriptPromptResponse), 0o600))
	require.Zero(t, s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Time{}))
	require.True(t, f.precomputedFinalTranscript(t))
	f.requireTranscripts(t, len(f.checkpointIDs))
}

// The host cancelling the hook during preparation makes the repository open
// fail. That is an interruption, not an open failure: the IDs must survive
// and no error is counted.
func TestFinalizeAllTurnCheckpoints_CancelledDuringPreparation(t *testing.T) {
	f := newFinalizeFixture(t)
	turnIDs := append([]string(nil), f.state.TurnCheckpointIDs...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitBlocked := f.blockTranscriptRead(t, func() {
		// Without the cached root, the repository open asks git under the
		// cancelled context, as a fresh hook process would.
		paths.ClearWorktreeRootCache()
		cancel()
	})

	errCount := NewManualCommitStrategy().finalizeAllTurnCheckpoints(ctx, f.state, time.Time{})
	waitBlocked()

	require.Zero(t, errCount, "an interruption is reported as capture degradation, not as errors")
	require.NotNil(t, f.state.CaptureDegradedAt)
	require.Equal(t, turnIDs, f.state.TurnCheckpointIDs, "the IDs survive for a repeated Stop")
}
