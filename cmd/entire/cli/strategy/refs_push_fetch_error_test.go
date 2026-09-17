package strategy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithFetchDetail_KeepsCauseWhenGitIsSilent pins the regression that made a
// failed recovery unreadable: the fetch error used to be *replaced* by git's
// output, so a git killed before it wrote anything produced a bare
// "fetch failed: " — no cause, and nothing errors.Is could match.
func TestWithFetchDetail_KeepsCauseWhenGitIsSilent(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("signal: killed")

	silent := withFetchDetail(sentinel, nil)
	require.ErrorIs(t, silent, sentinel, "the cause must survive an empty output")
	assert.Equal(t, sentinel.Error(), silent.Error())

	detailed := withFetchDetail(sentinel, []byte("fatal: couldn't find remote ref\n"))
	require.ErrorIs(t, detailed, sentinel, "detail must be added to the cause, not substituted for it")
	assert.Contains(t, detailed.Error(), "couldn't find remote ref")

	elided := withFetchDetail(sentinel, []byte(strings.Repeat("noise ", 2000)))
	require.ErrorIs(t, elided, sentinel)
	assert.Less(t, len([]rune(elided.Error())), maxFetchErrorDetail+len(sentinel.Error())+8,
		"git output must be capped before it reaches a terminal or a log attribute")
}

// TestFetchAndRebaseRefCommon_CancelledContextNamesCause is the end-to-end half
// of the same regression: an interrupted flush must not log one contentless
// "fetch failed: " per queued ref.
func TestFetchAndRebaseRefCommon_CancelledContextNamesCause(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir) // CWD-based repository resolution; cannot run in parallel.

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := fetchAndRebaseRefCommon(ctx, bareDir, refs[0])
	require.Error(t, err)
	assert.NotEqual(t, "fetch failed: ", err.Error(), "a cancelled fetch must still name its cause")
	assert.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(err.Error(), "fetch failed:")))
}
