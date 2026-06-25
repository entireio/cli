package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

func TestValidateTrailStatusFormat(t *testing.T) {
	t.Parallel()
	for _, format := range []string{trailStatusFormatStatusline, trailStatusFormatPlain, trailStatusFormatJSON} {
		require.NoError(t, validateTrailStatusFormat(format))
	}
	require.Error(t, validateTrailStatusFormat("yaml"))
	require.Error(t, validateTrailStatusFormat(""))
}

func TestTrailStatusFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	require.False(t, trailStatusFresh(trailStatusSnapshot{}, now), "zero CheckedAt is never fresh")
	require.True(t, trailStatusFresh(trailStatusSnapshot{CheckedAt: now.Add(-trailStatusFreshTTL + time.Second)}, now))
	require.False(t, trailStatusFresh(trailStatusSnapshot{CheckedAt: now.Add(-trailStatusFreshTTL - time.Second)}, now))
	require.False(t, trailStatusFresh(trailStatusSnapshot{CheckedAt: now.Add(time.Minute)}, now), "future CheckedAt (clock skew) is not fresh")
}

func TestPruneTrailStatusCache(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	entries := make(map[string]trailStatusSnapshot)
	for i := range trailStatusCacheMaxEntries + 10 {
		entries[fmt.Sprintf("k%03d", i)] = trailStatusSnapshot{CheckedAt: base.Add(time.Duration(i) * time.Minute)}
	}
	pruned := pruneTrailStatusCache(entries)
	require.Len(t, pruned, trailStatusCacheMaxEntries)
	// The newest entry must survive; the oldest must be dropped.
	require.Contains(t, pruned, fmt.Sprintf("k%03d", trailStatusCacheMaxEntries+9))
	require.NotContains(t, pruned, "k000")
}

func TestPruneTrailStatusCache_UnderCapUnchanged(t *testing.T) {
	t.Parallel()
	entries := map[string]trailStatusSnapshot{"a": {}, "b": {}}
	require.Len(t, pruneTrailStatusCache(entries), 2)
}

func TestTrailStatusDirFromStdin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"workspace current_dir preferred", `{"cwd":"/a","workspace":{"current_dir":"/b"}}`, "/b"},
		{"falls back to cwd", `{"cwd":"/a"}`, "/a"},
		{"empty payload", `{}`, ""},
		{"not json", `not json`, ""},
		{"empty input", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, trailStatusDirFromStdin(strings.NewReader(tc.in)))
		})
	}
}

func TestTrailStatusNumberOrTitle(t *testing.T) {
	t.Parallel()
	require.Equal(t, "#12 Add login", trailStatusNumberOrTitle(trailStatusSnapshot{Number: 12, Title: "Add login"}))
	require.Equal(t, "#12", trailStatusNumberOrTitle(trailStatusSnapshot{Number: 12}))
	require.Equal(t, "Add login", trailStatusNumberOrTitle(trailStatusSnapshot{Title: "Add login"}))
	require.Equal(t, "feature/x", trailStatusNumberOrTitle(trailStatusSnapshot{Branch: "feature/x"}))

	long := strings.Repeat("a", trailStatusMaxTitleRunes+20)
	got := trailStatusNumberOrTitle(trailStatusSnapshot{Number: 1, Title: long})
	require.LessOrEqual(t, len([]rune(got)), trailStatusMaxTitleRunes+len("#1 ")+1) // +1 for the ellipsis
	require.Contains(t, got, "…")
}

func TestTrailStatusFindingsText(t *testing.T) {
	t.Parallel()
	require.Equal(t, "1 open finding", trailStatusFindingsText(trailStatusSnapshot{OpenFindings: 1, FindingsKnown: true}))
	require.Equal(t, "3 open findings", trailStatusFindingsText(trailStatusSnapshot{OpenFindings: 3, FindingsKnown: true}))
	require.Equal(t, "3 open findings (2 high)", trailStatusFindingsText(trailStatusSnapshot{OpenFindings: 3, HighFindings: 2, FindingsKnown: true}))
	require.Equal(t,
		fmt.Sprintf("%d+ open findings", trailStatusFindingsScanLimit),
		trailStatusFindingsText(trailStatusSnapshot{OpenFindings: trailStatusFindingsScanLimit, FindingsKnown: true}))
}

func TestRenderTrailStatusPlain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		snap trailStatusSnapshot
		want string
	}{
		{"trail with findings", trailStatusSnapshot{State: trailStatusStateTrail, Number: 7, Title: "Add x", OpenFindings: 2, FindingsKnown: true, URL: "https://e.io/t/7"}, "Trail #7 Add x — 2 open findings (https://e.io/t/7)"},
		{"trail no findings", trailStatusSnapshot{State: trailStatusStateTrail, Number: 7, Title: "Add x"}, "Trail #7 Add x"},
		{"no trail", trailStatusSnapshot{State: trailStatusStateNoTrail, Branch: "feat"}, "No trail for branch feat"},
		{"disabled", trailStatusSnapshot{State: trailStatusStateDisabled}, "Trails are not enabled for this repository"},
		{"unauth", trailStatusSnapshot{State: trailStatusStateUnauth}, "Not logged in (run 'entire login')"},
		{"no repo", trailStatusSnapshot{State: trailStatusStateNoRepo}, "Not an Entire trails-supported repository"},
		{"error with message", trailStatusSnapshot{State: trailStatusStateError, Message: "boom"}, "Trail status unavailable: boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, renderTrailStatusPlain(tc.snap))
		})
	}
}

func TestTrailBannerLine(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		"Trail #7 Add x · https://e.io/t/7 · 2 open findings",
		trailBannerLine(trailStatusSnapshot{State: trailStatusStateTrail, Number: 7, Title: "Add x", URL: "https://e.io/t/7", OpenFindings: 2, FindingsKnown: true}))
	require.Equal(t, "Trail #7 Add x", trailBannerLine(trailStatusSnapshot{State: trailStatusStateTrail, Number: 7, Title: "Add x"}))
	require.Empty(t, trailBannerLine(trailStatusSnapshot{State: trailStatusStateNoTrail}))
	require.Empty(t, trailBannerLine(trailStatusSnapshot{State: trailStatusStateUnauth}))
}

func TestRenderTrailStatusLine(t *testing.T) {
	t.Parallel()
	line := renderTrailStatusLine(trailStatusSnapshot{State: trailStatusStateTrail, Number: 9, Title: "Fix bug", URL: "https://e.io/t/9", OpenFindings: 1, HighFindings: 1, FindingsKnown: true})
	require.Contains(t, line, "#9")
	require.Contains(t, line, "Fix bug")
	require.Contains(t, line, "open finding")

	require.Contains(t, renderTrailStatusLine(trailStatusSnapshot{State: trailStatusStateNoTrail}), "no trail")

	for _, state := range []string{trailStatusStateDisabled, trailStatusStateUnauth, trailStatusStateNoRepo, trailStatusStateError} {
		require.Empty(t, renderTrailStatusLine(trailStatusSnapshot{State: state}), "state %q must render empty for a status line", state)
	}
}

func TestWriteTrailStatus(t *testing.T) {
	t.Parallel()

	// JSON always emits, even for non-trail states.
	var jsonBuf bytes.Buffer
	require.NoError(t, writeTrailStatus(&jsonBuf, trailStatusSnapshot{State: trailStatusStateNoRepo}, trailStatusFormatJSON))
	var decoded trailStatusSnapshot
	require.NoError(t, json.Unmarshal(jsonBuf.Bytes(), &decoded))
	require.Equal(t, trailStatusStateNoRepo, decoded.State)

	// Statusline/plain emit nothing for a blank state.
	var slBuf bytes.Buffer
	require.NoError(t, writeTrailStatus(&slBuf, trailStatusSnapshot{State: trailStatusStateUnauth}, trailStatusFormatStatusline))
	require.Empty(t, slBuf.String())

	// Statusline emits a trailing newline for a trail.
	var trailBuf bytes.Buffer
	require.NoError(t, writeTrailStatus(&trailBuf, trailStatusSnapshot{State: trailStatusStateTrail, Number: 3, Title: "t"}, trailStatusFormatStatusline))
	require.True(t, strings.HasSuffix(trailBuf.String(), "\n"))
	require.Contains(t, trailBuf.String(), "#3")
}

func TestTrailStatusSnapshotFromError(t *testing.T) {
	t.Parallel()
	unauth := trailStatusSnapshotFromError(fmt.Errorf("wrap: %w", auth.ErrNotLoggedIn))
	require.Equal(t, trailStatusStateUnauth, unauth.State)

	generic := trailStatusSnapshotFromError(errors.New("network down\nsecond line"))
	require.Equal(t, trailStatusStateError, generic.State)
	require.Equal(t, "network down", generic.Message, "message is collapsed to the first line")
}

// TestTrailStatusCacheRoundTrip exercises the on-disk cache against an isolated
// temp repo (cache lives under the git common dir).
func TestTrailStatusCacheRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	ctx := context.Background()

	const key = "gh/acme/widgets@feature/login"
	_, ok := loadTrailStatusCache(ctx, key)
	require.False(t, ok, "cache miss before any save")

	snap := trailStatusSnapshot{State: trailStatusStateTrail, Number: 42, Title: "Login", CheckedAt: time.Now().UTC()}
	require.NoError(t, saveTrailStatusCache(ctx, key, snap))

	got, ok := loadTrailStatusCache(ctx, key)
	require.True(t, ok)
	require.Equal(t, 42, got.Number)
	require.Equal(t, "Login", got.Title)

	// A second key coexists with the first (per-branch entries).
	require.NoError(t, saveTrailStatusCache(ctx, "gh/acme/widgets@main", trailStatusSnapshot{State: trailStatusStateNoTrail, CheckedAt: time.Now().UTC()}))
	_, ok = loadTrailStatusCache(ctx, key)
	require.True(t, ok, "first entry survives a second branch's write")
}
