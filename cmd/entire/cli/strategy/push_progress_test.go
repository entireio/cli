package strategy

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParseRFC3339(t *testing.T, value string) time.Time {
	t.Helper()
	when, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return when
}

func TestParsePushSummaryFromLog(t *testing.T) {
	t.Parallel()

	t.Run("parses multiple commits into per-session aggregates", func(t *testing.T) {
		t.Parallel()
		gitLogOutput := strings.Join([]string{
			"abc1234|Checkpoint: a3b2c4d5e6f7|Entire-Session: sess-2026-06-12-abc123|2026-06-12T12:30:00+08:00",
			"def5678|Finalize transcript for Checkpoint: a3b2c4d5e6f7||2026-06-12T13:00:00+08:00",
			"ghi9012|Checkpoint: b4c3d5e6f7a8|Entire-Session: sess-2026-06-12-abc123|2026-06-12T14:05:00+08:00",
			"jkl3456|Checkpoint: c5d4e6f7a8b9|Entire-Session: sess-2026-06-11-def456|2026-06-11T10:00:00+08:00",
			"mno7890|Finalize transcript for Checkpoint: c5d4e6f7a8b9||2026-06-11T10:30:00+08:00",
		}, "\n")

		result := parsePushSummaryFromLog(gitLogOutput)
		require.Len(t, result, 2)

		first := result[0]
		assert.Equal(t, "sess-2026-06-12-abc123", first.SessionID)
		assert.Equal(t, 2, first.CheckpointCount)
		assert.Equal(t, 3, first.CommitCount)
		assert.Equal(t, mustParseRFC3339(t, "2026-06-12T12:30:00+08:00"), first.EarliestTime)
		assert.Equal(t, mustParseRFC3339(t, "2026-06-12T14:05:00+08:00"), first.LatestTime)

		second := result[1]
		assert.Equal(t, "sess-2026-06-11-def456", second.SessionID)
		assert.Equal(t, 1, second.CheckpointCount)
		assert.Equal(t, 2, second.CommitCount)
	})

	t.Run("returns empty array for empty input", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, parsePushSummaryFromLog(""))
	})

	t.Run("assigns unknown when session trailer missing", func(t *testing.T) {
		t.Parallel()
		gitLogOutput := strings.Join([]string{
			"abc1234|Finalize transcript for Checkpoint: a3b2c4d5e6f7||2026-06-12T12:30:00+08:00",
			"def5678|Update checkpoint summary for a3b2c4d5e6f7||2026-06-12T12:35:00+08:00",
		}, "\n")

		result := parsePushSummaryFromLog(gitLogOutput)
		require.Len(t, result, 1)
		assert.Equal(t, "unknown", result[0].SessionID)
		assert.Equal(t, 2, result[0].CommitCount)
		assert.Equal(t, 1, result[0].CheckpointCount)
	})

	t.Run("sorts sessions by latest time descending", func(t *testing.T) {
		t.Parallel()
		gitLogOutput := strings.Join([]string{
			"a|Checkpoint: aaa|Entire-Session: old-session|2026-06-01T10:00:00+08:00",
			"b|Checkpoint: bbb|Entire-Session: new-session|2026-06-12T10:00:00+08:00",
		}, "\n")

		result := parsePushSummaryFromLog(gitLogOutput)
		require.Len(t, result, 2)
		assert.Equal(t, "new-session", result[0].SessionID)
		assert.Equal(t, "old-session", result[1].SessionID)
	})
}

func TestFormatSessionTree(t *testing.T) {
	t.Parallel()

	t.Run("formats sessions into a tree with connector characters", func(t *testing.T) {
		t.Parallel()
		summaries := []sessionSummary{
			{
				SessionID:       "sess-2026-06-12-abc123",
				CheckpointCount: 3,
				CommitCount:     5,
				EarliestTime:    time.Date(2026, 6, 12, 12, 30, 0, 0, time.UTC),
				LatestTime:      time.Date(2026, 6, 12, 14, 5, 0, 0, time.UTC),
			},
			{
				SessionID:       "sess-2026-06-11-def456",
				CheckpointCount: 2,
				CommitCount:     3,
				EarliestTime:    time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC),
				LatestTime:      time.Date(2026, 6, 11, 10, 30, 0, 0, time.UTC),
			},
		}

		lines := formatSessionTree(summaries, formatSessionTreeOpts{
			TotalCommits: 8,
			NoColor:      true,
		})

		assert.Contains(t, lines[0], "8 commits, 2 sessions")
		assert.Contains(t, lines[1], "├─")
		assert.Contains(t, lines[1], "sess-2026-06-12-abc123")
		assert.Contains(t, lines[1], "3 checkpoints")
		assert.Contains(t, lines[2], "└─")
		assert.Contains(t, lines[2], "sess-2026-06-11-def456")
		assert.Contains(t, lines[2], "2 checkpoints")
	})

	t.Run("folds sessions beyond max display", func(t *testing.T) {
		t.Parallel()
		summaries := make([]sessionSummary, 8)
		for i := range summaries {
			day := 12 - i
			summaries[i] = sessionSummary{
				SessionID:       "sess-" + string(rune('0'+i)),
				CheckpointCount: 1,
				CommitCount:     2,
				EarliestTime:    time.Date(2026, 6, day, 10, 0, 0, 0, time.UTC),
				LatestTime:      time.Date(2026, 6, day, 11, 0, 0, 0, time.UTC),
			}
		}

		lines := formatSessionTree(summaries, formatSessionTreeOpts{
			TotalCommits: 16,
			NoColor:      true,
		})

		assert.Contains(t, lines[0], "16 commits, 8 sessions")
		treeLines := lines[1:]
		assert.Len(t, treeLines, 7)
		assert.Contains(t, treeLines[5], "... and 3 more sessions")
		assert.Contains(t, treeLines[6], "oldest:")
	})

	t.Run("uses singular checkpoint label", func(t *testing.T) {
		t.Parallel()
		summaries := []sessionSummary{{
			SessionID:       "sess-solo",
			CheckpointCount: 1,
			CommitCount:     1,
			EarliestTime:    time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
			LatestTime:      time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
		}}

		lines := formatSessionTree(summaries, formatSessionTreeOpts{
			TotalCommits: 1,
			NoColor:      true,
		})
		assert.Contains(t, lines[1], "1 checkpoint")
		assert.NotContains(t, lines[1], "1 checkpoints")
	})

	t.Run("shows new branch label", func(t *testing.T) {
		t.Parallel()
		summaries := []sessionSummary{{
			SessionID:       "sess-a",
			CheckpointCount: 1,
			CommitCount:     2,
			EarliestTime:    time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
			LatestTime:      time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
		}}

		lines := formatSessionTree(summaries, formatSessionTreeOpts{
			TotalCommits: 2,
			NoColor:      true,
			IsNewBranch:  true,
		})
		assert.Contains(t, lines[0], "new branch")
	})
}

func TestParseGitProgressLine(t *testing.T) {
	t.Parallel()

	t.Run("enumerating objects", func(t *testing.T) {
		t.Parallel()
		event := parseGitProgressLine("Enumerating objects: 47, done.")
		require.NotNil(t, event)
		assert.Equal(t, gitProgressPhaseCounting, event.Phase)
		assert.Equal(t, 47, event.Total)
		assert.True(t, event.Done)
	})

	t.Run("counting objects percentage", func(t *testing.T) {
		t.Parallel()
		event := parseGitProgressLine("Counting objects: 100% (47/47), done.")
		require.NotNil(t, event)
		assert.Equal(t, gitProgressPhaseCounting, event.Phase)
		assert.Equal(t, 100, event.Percent)
		assert.Equal(t, 47, event.Current)
		assert.Equal(t, 47, event.Total)
		assert.True(t, event.Done)
	})

	t.Run("compressing objects", func(t *testing.T) {
		t.Parallel()
		event := parseGitProgressLine("Compressing objects:  81% (31/38)")
		require.NotNil(t, event)
		assert.Equal(t, gitProgressPhaseCompressing, event.Phase)
		assert.Equal(t, 81, event.Percent)
		assert.Equal(t, 31, event.Current)
		assert.Equal(t, 38, event.Total)
		assert.False(t, event.Done)
	})

	t.Run("writing objects with speed", func(t *testing.T) {
		t.Parallel()
		event := parseGitProgressLine("Writing objects: 100% (42/42), 156.23 KiB | 312.00 KiB/s, done.")
		require.NotNil(t, event)
		assert.Equal(t, gitProgressPhaseWriting, event.Phase)
		assert.Equal(t, 100, event.Percent)
		assert.Equal(t, 42, event.Current)
		assert.Equal(t, 42, event.Total)
		assert.Equal(t, "156.23 KiB", event.Bytes)
		assert.Equal(t, "312.00 KiB/s", event.Speed)
		assert.True(t, event.Done)
	})

	t.Run("writing objects in progress", func(t *testing.T) {
		t.Parallel()
		event := parseGitProgressLine("Writing objects:  45% (9/20), 78.00 KiB")
		require.NotNil(t, event)
		assert.Equal(t, gitProgressPhaseWriting, event.Phase)
		assert.Equal(t, 45, event.Percent)
		assert.Equal(t, 9, event.Current)
		assert.Equal(t, 20, event.Total)
		assert.Equal(t, "78.00 KiB", event.Bytes)
		assert.Empty(t, event.Speed)
		assert.False(t, event.Done)
	})

	t.Run("returns nil for unrecognized lines", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, parseGitProgressLine("Total 42 (delta 2), reused 0 (delta 0)"))
		assert.Nil(t, parseGitProgressLine(""))
		assert.Nil(t, parseGitProgressLine("remote: Resolving deltas: 100%"))
		assert.Nil(t, parseGitProgressLine("Delta compression using up to 8 threads"))
	})
}
