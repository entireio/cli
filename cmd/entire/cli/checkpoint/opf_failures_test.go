package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOPFFailureLog_CountsConsecutiveFailuresAndClearsOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := NewOPFFailureLog(dir)
	now := time.Now()

	a := mustRefName(t, "a1b2c3d4e5f6")
	b := mustRefName(t, "b2c3d4e5f6a1")

	// A log that was never written reports nothing, rather than erroring.
	stuck, err := log.StuckRefs()
	require.NoError(t, err)
	assert.Empty(t, stuck)

	// One ref keeps failing while the other keeps succeeding.
	for range StuckOPFFailureThreshold {
		require.NoError(t, log.Record(
			[]plumbing.ReferenceName{a}, []plumbing.ReferenceName{b}, now))
	}

	counts, err := log.Counts()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{a.String(): StuckOPFFailureThreshold}, counts,
		"a ref that never failed must not appear at all")

	stuck, err = log.StuckRefs()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a}, stuck)

	// The moment it succeeds the count is gone: "consecutive" has to mean
	// consecutive, or one transient failure would brand a ref forever.
	require.NoError(t, log.Record(nil, []plumbing.ReferenceName{a}, now))
	stuck, err = log.StuckRefs()
	require.NoError(t, err)
	assert.Empty(t, stuck)

	// An empty log leaves no stray file behind.
	_, statErr := os.Stat(filepath.Join(dir, opfFailureFileName))
	assert.True(t, os.IsNotExist(statErr), "an empty log must remove its file")
}

// One failure short of the threshold is still "the worker has not got to it
// yet", which is the distinction the whole counter exists to draw.
func TestOPFFailureLog_BelowThresholdIsNotStuck(t *testing.T) {
	t.Parallel()
	log := NewOPFFailureLog(t.TempDir())
	a := mustRefName(t, "a1b2c3d4e5f6")

	for range StuckOPFFailureThreshold - 1 {
		require.NoError(t, log.Record([]plumbing.ReferenceName{a}, nil, time.Now()))
	}

	stuck, err := log.StuckRefs()
	require.NoError(t, err)
	assert.Empty(t, stuck)
}

// Advisory bookkeeping must never be able to wedge the background worker: a
// garbled file reads as an empty log and the next Record rewrites it, rather
// than failing every flush from here on.
func TestOPFFailureLog_CorruptFileReadsAsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := NewOPFFailureLog(dir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, opfFailureFileName), []byte("{not json"), 0o600))

	counts, err := log.Counts()
	require.NoError(t, err)
	assert.Empty(t, counts)

	a := mustRefName(t, "a1b2c3d4e5f6")
	require.NoError(t, log.Record([]plumbing.ReferenceName{a}, nil, time.Now()))
	counts, err = log.Counts()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{a.String(): 1}, counts)
}
