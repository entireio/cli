package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

const attributionTestEmail = "test@example.com"

func TestParseBlamePorcelain(t *testing.T) {
	output := strings.Join([]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 1 1 1",
		"author Ada Lovelace",
		"author-time 1700000000",
		"\tprint('hello')",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 2 2 1",
		"author Grace Hopper",
		"author-time 1700000100",
		"\tprint('world')",
		"",
	}, "\n")

	lines, err := parseBlamePorcelain(output)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, 1, lines[0].LineNumber)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", lines[0].CommitSHA)
	require.Equal(t, "Ada Lovelace", lines[0].Author)
	require.Equal(t, "print('hello')", lines[0].Content)
	require.NotNil(t, lines[0].AuthorTime)
	require.Equal(t, time.UTC, lines[0].AuthorTime.Location())
	require.Equal(t, "2023-11-14T22:13:20Z", lines[0].AuthorTime.Format(time.RFC3339))
	require.Equal(t, 2, lines[1].LineNumber)
}

func TestParseBlamePorcelainSupportsSHA256ObjectIDs(t *testing.T) {
	sha256ID := strings.Repeat("a", 64)
	output := strings.Join([]string{
		sha256ID + " 1 1 1",
		"author Ada Lovelace",
		"author-time 1700000000",
		"\tprint('hello')",
		"",
	}, "\n")

	lines, err := parseBlamePorcelain(output)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, sha256ID, lines[0].CommitSHA)
	require.Equal(t, 1, lines[0].LineNumber)
	require.Equal(t, "print('hello')", lines[0].Content)
}

func TestIsZeroCommitSupportsSHA256ObjectIDs(t *testing.T) {
	require.True(t, isZeroCommit(strings.Repeat("0", 40)))
	require.True(t, isZeroCommit(strings.Repeat("0", 64)))
	require.False(t, isZeroCommit(strings.Repeat("0", 63)+"1"))
}

func TestParseAttributionLineRange(t *testing.T) {
	got, err := parseAttributionLineRange("12-20")
	require.NoError(t, err)
	require.Equal(t, &attributionLineRange{Start: 12, End: 20}, got)

	got, err = parseAttributionLineRange("7")
	require.NoError(t, err)
	require.Equal(t, &attributionLineRange{Start: 7, End: 7}, got)

	_, err = parseAttributionLineRange("20-12")
	require.Error(t, err)
}

func TestAttributionBlameShowsHumanAndAICheckpointLines(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "a1b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-ai-12345678",
		Prompts:          []string{"Add an agent-owned helper."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		Model:            "claude-sonnet-test",
		CheckpointsCount: 1,
		Attribution: &checkpoint.Attribution{
			AgentLines:        1,
			TotalCommitted:    1,
			TotalLinesChanged: 1,
			AgentPercentage:   100,
			MetricVersion:     2,
		},
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("agent update", checkpointid.MustCheckpointID("a1b2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py", attributionBlameOptions{}))
	text := out.String()
	require.Contains(t, text, "[HU]")
	require.Contains(t, text, "[AI]")
	require.Contains(t, text, "Agent")
	require.Contains(t, text, "Author")
	require.Contains(t, text, "Checkpoint")
	require.NotContains(t, text, "Model")
	require.NotContains(t, text, "Checkpoint/Session")
	require.Contains(t, text, "a1b2c3d4e5f6")
	require.Contains(t, text, "AI: 1")
	require.Contains(t, text, "Human: 1")
	requireCompactBlameTableFits(t, text, 80)
	requireCompactBlameColumnsAlign(t, text)
}

func TestAttributionBlameColumnExpandsForFiveDigitLines(t *testing.T) {
	lines := []attributionLine{
		{
			LineNumber: 9999,
			Authorship: attributionHuman,
			Author:     "Suhaan",
			Content:    "human_line = 1",
		},
		{
			LineNumber:   10000,
			Authorship:   attributionAI,
			Agent:        "Codex",
			Author:       "Codex",
			CheckpointID: "a1b2c3d4e5f6",
			Content:      "ai_line = 2",
		},
	}
	result := &fileAttributionResult{
		File:    "large.py",
		Lines:   lines,
		Summary: summarizeAttributionLines(lines),
	}

	var out bytes.Buffer
	renderAttributionBlameCompact(&out, result, "9999-10000")
	text := out.String()

	requireCompactBlameColumnsAlign(t, text)
	require.Contains(t, text, "10000  [AI]")
	require.Equal(t, 5, attributionLineColumnWidth(lines))
}

func TestAttributionBlameLongShowsDetailedColumns(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "a2b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-ai-12345678",
		Prompts:          []string{"Add an agent-owned helper."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		Model:            "claude-sonnet-test",
		CheckpointsCount: 1,
		Attribution: &checkpoint.Attribution{
			AgentLines:        1,
			TotalCommitted:    1,
			TotalLinesChanged: 1,
			AgentPercentage:   100,
			MetricVersion:     2,
		},
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("agent update", checkpointid.MustCheckpointID("a2b2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py", attributionBlameOptions{Long: true}))
	text := out.String()
	require.Contains(t, text, "Agent")
	require.Contains(t, text, "Model")
	require.Contains(t, text, "Author")
	require.Contains(t, text, "Checkpoint/Session")
	require.Contains(t, text, "claude-sonne")
	require.Contains(t, text, "a2b2c3d4e5f6")
}

func TestAttributionBlameMarksMixedCheckpoint(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "b1b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-mixed-12345678",
		Prompts:          []string{"Change agent code, then keep a user tweak."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		Model:            "claude-sonnet-test",
		CheckpointsCount: 1,
		Attribution: &checkpoint.Attribution{
			AgentLines:        1,
			HumanModified:     1,
			TotalCommitted:    1,
			TotalLinesChanged: 2,
			AgentPercentage:   50,
			MetricVersion:     2,
		},
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nmixed_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("mixed update", checkpointid.MustCheckpointID("b1b2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py", attributionBlameOptions{LineFlag: "2"}))
	require.Contains(t, out.String(), "[MX]")
	require.Contains(t, out.String(), "Mixed: 1")
}

func TestAttributionWhyLineShowsPromptAndCheckpoint(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "c1b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-why-12345678",
		Prompts:          []string{"Create a line that can be explained."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		Model:            "claude-sonnet-test",
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nwhy_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("why update", checkpointid.MustCheckpointID("c1b2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &out, "auth.py:2", attributionWhyOptions{}))
	text := out.String()
	require.Contains(t, text, "Prompt:")
	require.Contains(t, text, "Create a line that can be explained.")
	require.Contains(t, text, "c1b2c3d4e5f6")
	require.Contains(t, text, "entire checkpoint explain c1b2c3d4e5f6")
}

func TestAttributionBlameJSONIsStable(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "d1b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-json-12345678",
		Prompts:          []string{"Add JSON attributed line."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\njson_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("json update", checkpointid.MustCheckpointID("d1b2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py", attributionBlameOptions{JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	require.Equal(t, "auth.py", payload.File)
	require.Len(t, payload.Lines, 2)
	require.Equal(t, attributionAI, payload.Lines[1].Authorship)
	require.Equal(t, "d1b2c3d4e5f6", payload.Lines[1].CheckpointID)
	require.Contains(t, payload.Checkpoints, "d1b2c3d4e5f6")
}

func TestAttributionBlameJSONEmptyFileUsesEmptyLinesArray(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	testutil.WriteFile(t, repoRoot, "empty.txt", "")
	testutil.GitAdd(t, repoRoot, "empty.txt")
	testutil.GitCommit(t, repoRoot, "add empty file")

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "empty.txt", attributionBlameOptions{JSON: true}))
	require.Contains(t, out.String(), `"lines": []`)
}

func TestAttributionBlameJSONLineFilterPrunesCheckpoints(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "e1b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-filter-12345678",
		Prompts:          []string{"Add the second line only."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("line filter update", checkpointid.MustCheckpointID("e1b2c3d4e5f6")))

	var humanOut bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &humanOut, "auth.py", attributionBlameOptions{LineFlag: "1", JSON: true}))
	var humanPayload fileAttributionResult
	require.NoError(t, json.Unmarshal(humanOut.Bytes(), &humanPayload))
	require.Len(t, humanPayload.Lines, 1)
	require.Equal(t, attributionHuman, humanPayload.Lines[0].Authorship)
	require.Empty(t, humanPayload.Checkpoints)

	var aiOut bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &aiOut, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
	var aiPayload fileAttributionResult
	require.NoError(t, json.Unmarshal(aiOut.Bytes(), &aiPayload))
	require.Len(t, aiPayload.Lines, 1)
	require.Equal(t, attributionAI, aiPayload.Lines[0].Authorship)
	require.Contains(t, aiPayload.Checkpoints, "e1b2c3d4e5f6")
}

func TestAttributionBlameMixedUsesFileMatchingCheckpoint(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "f1b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-auth-12345678",
		Prompts:          []string{"Add auth line."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
		Attribution: &checkpoint.Attribution{
			AgentLines:        1,
			TotalCommitted:    1,
			TotalLinesChanged: 1,
			AgentPercentage:   100,
			MetricVersion:     2,
		},
	})
	writeAttributionCheckpoint(t, repoRoot, "f2b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-other-12345678",
		Prompts:          []string{"Mixed update in another file."},
		FilesTouched:     []string{"other.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
		Attribution: &checkpoint.Attribution{
			AgentLines:        1,
			HumanModified:     1,
			TotalCommitted:    1,
			TotalLinesChanged: 2,
			AgentPercentage:   50,
			MetricVersion:     2,
		},
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, formatCheckpointTrailers("squash-style update", "f2b2c3d4e5f6", "f1b2c3d4e5f6"))

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	require.Len(t, payload.Lines, 1)
	require.Equal(t, attributionAI, payload.Lines[0].Authorship)
	require.Equal(t, "f1b2c3d4e5f6", payload.Lines[0].CheckpointID)
	require.Equal(t, 0, payload.Summary.MixedLines)
	require.Equal(t, 1, payload.Summary.AILines)
}

func TestAttributionResolverUsesCheckpointReader(t *testing.T) {
	t.Parallel()

	cpID := checkpointid.MustCheckpointID("d9b2c3d4e5f6")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:    "session-ai",
				FilesTouched: []string{"auth.py"},
				Agent:        agent.AgentTypeClaudeCode,
				Model:        "claude-test",
			},
			Prompts: "Explain the authentication change.",
		},
	}
	resolver := &attributionResolver{
		ctx:             context.Background(),
		store:           reader,
		checkpointCache: make(map[string]attributionCheckpointContext),
	}

	ctx := resolver.readCheckpointContext(cpID, "auth.py")
	require.Equal(t, "session-ai", ctx.SessionID)
	require.Equal(t, "Claude Code", ctx.Agent)
	require.Equal(t, "claude-test", ctx.Model)
	require.Equal(t, "Explain the authentication change.", ctx.Prompt)
}

func TestAttributionResolverMissingMetadataIncludesReason(t *testing.T) {
	newAttributionRepo(t)

	cpID := checkpointid.MustCheckpointID("cab2c3d4e5f6")
	stubReader := &attributionCheckpointReaderStub{
		readErr: errors.New("checkpoint summary unavailable"),
	}
	resolver := &attributionResolver{
		ctx:             context.Background(),
		store:           stubReader,
		fetchOnMiss:     true,
		checkpointCache: make(map[string]attributionCheckpointContext),
	}

	ctx := resolver.readCheckpointContext(cpID, "auth.py")
	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "checkpoint summary unavailable")
	// "remote refresh failed" confirms fetch-on-miss was attempted.
	require.Contains(t, ctx.MetadataMissingReason, "remote refresh failed")
	require.Contains(t, ctx.MetadataMissingReason, "git fetch ")
	require.Contains(t, ctx.MetadataMissingReason, "entire/checkpoints/v1:entire/checkpoints/v1")
	require.Contains(t, ctx.MetadataMissingReason, "entire checkpoint explain cab2c3d4e5f6")
}

type attributionCheckpointReaderStub struct {
	summary *checkpoint.CheckpointSummary
	content *checkpoint.SessionContent
	readErr error
}

func (s *attributionCheckpointReaderStub) Read(context.Context, checkpointid.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.summary, nil
}

func (s *attributionCheckpointReaderStub) ReadSessionMetadataAndPrompts(context.Context, checkpointid.CheckpointID, int) (*checkpoint.Metadata, string, error) {
	if s.content == nil {
		return nil, "", nil
	}
	return &s.content.Metadata, s.content.Prompts, nil
}

func TestAttributionBlameScopesMixedToSessionNotCheckpoint(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "a9b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-scoped-12345678",
		Prompts:          []string{"Agent-only edit to auth.py."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
		// The session that touched auth.py is purely agent work...
		Attribution: &checkpoint.Attribution{
			AgentLines:        1,
			TotalCommitted:    1,
			TotalLinesChanged: 1,
			AgentPercentage:   100,
			MetricVersion:     2,
		},
		// ...even though the checkpoint as a whole mixed agent and human work
		// (e.g. a human-edited file elsewhere in the same checkpoint).
		CombinedAttribution: &checkpoint.Attribution{
			AgentLines:        1,
			HumanModified:     1,
			TotalCommitted:    2,
			TotalLinesChanged: 2,
			AgentPercentage:   50,
			MetricVersion:     2,
		},
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("scoped update", checkpointid.MustCheckpointID("a9b2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	require.Len(t, payload.Lines, 1)
	require.Equal(t, attributionAI, payload.Lines[0].Authorship)
	require.Equal(t, 0, payload.Summary.MixedLines)
}

func TestAttributionFlagsSessionFallbackForUnmatchedFile(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	// One checkpoint, two sessions, neither recording a touch to auth.py (e.g.
	// the file was renamed after the checkpoint). Attribution must fall back to
	// a session and flag that the agent/prompt shown is approximate.
	writeAttributionCheckpoint(t, repoRoot, "aab2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-one-12345678",
		Prompts:          []string{"Edit the first file."},
		FilesTouched:     []string{"old_name.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	writeAttributionCheckpoint(t, repoRoot, "aab2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-two-12345678",
		Prompts:          []string{"Edit a second file."},
		FilesTouched:     []string{"other.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("renamed update", checkpointid.MustCheckpointID("aab2c3d4e5f6")))

	var jsonOut bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &jsonOut, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &payload))
	require.Len(t, payload.Lines, 1)
	require.Equal(t, attributionAI, payload.Lines[0].Authorship)
	require.True(t, payload.Lines[0].SessionFallback)

	var whyOut bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &whyOut, "auth.py:2", attributionWhyOptions{}))
	require.Contains(t, whyOut.String(), "may have been renamed")
}

func TestAttributionFlagsSessionFallbackForMultiSessionEmptyPaths(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	// Two sessions under one checkpoint, neither touching auth.py. The first
	// (fallback) session recorded NO paths, so there is no rename evidence in its
	// FilesTouched — yet it is still only one of several sessions, picked as a
	// guess. The earlier `len(FilesTouched) > 0`-only rule left this uncaveated;
	// the union rule flags it via sessionsRead > 1. (Soph's review feedback.)
	writeAttributionCheckpoint(t, repoRoot, "bbc2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-empty-12345678",
		Prompts:          []string{"Attach session with no recorded paths."},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	writeAttributionCheckpoint(t, repoRoot, "bbc2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-other-12345678",
		Prompts:          []string{"Edit an unrelated file."},
		FilesTouched:     []string{"other.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("multi session", checkpointid.MustCheckpointID("bbc2c3d4e5f6")))

	var jsonOut bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &jsonOut, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &payload))
	require.Len(t, payload.Lines, 1)
	require.True(t, payload.Lines[0].SessionFallback, "multi-session empty-paths fallback should be flagged as a guess")
}

func TestAttributionDoesNotFlagSingleSessionEmptyPaths(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	// A single session that recorded no paths is "unknown", not rename evidence,
	// so it must NOT be caveated — the false positive the union rule still
	// suppresses (neither sessionsRead > 1 nor len(FilesTouched) > 0 holds).
	writeAttributionCheckpoint(t, repoRoot, "ccc2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-solo-12345678",
		Prompts:          []string{"Single session, no recorded paths."},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("single session", checkpointid.MustCheckpointID("ccc2c3d4e5f6")))

	var jsonOut bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &jsonOut, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &payload))
	require.Len(t, payload.Lines, 1)
	require.False(t, payload.Lines[0].SessionFallback, "single-session empty-paths must not be flagged")
}

func TestAttributionWhyHidesExplainHintWhenMetadataMissing(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	// A committed checkpoint trailer whose metadata was never written locally and
	// cannot be fetched (no remote). `why` must not print the bare
	// "Full context: entire checkpoint explain <id>" hint — that command fails
	// the same way the why fetch just did (Karthik's reported bug). It surfaces
	// the actionable fetch-then-explain remedy instead.
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nmissing_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("missing metadata", checkpointid.MustCheckpointID("bfc2c1df9e4b")))

	var out bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &out, "auth.py:2", attributionWhyOptions{}))
	text := out.String()
	require.Contains(t, text, "bfc2c1df9e4b")
	require.NotContains(t, text, "Full context:")
	require.Contains(t, text, "git fetch ")
	require.Contains(t, text, "entire checkpoint explain bfc2c1df9e4b")
}

func TestSummarizeAttributionLinesPercentagesSumTo100(t *testing.T) {
	lines := []attributionLine{
		{Authorship: attributionAI},
		{Authorship: attributionHuman},
		{Authorship: attributionMixed},
	}
	summary := summarizeAttributionLines(lines)
	require.Equal(t, 100, summary.AIPercentage+summary.HumanPercentage+summary.MixedPercentage)

	// An uncommitted line shares the 100%, so the three visible percentages
	// total less than 100 rather than each independently flooring to a sum
	// that drifts away from a coherent whole.
	lines = append(lines, attributionLine{Authorship: attributionUncommitted})
	summary = summarizeAttributionLines(lines)
	visible := summary.AIPercentage + summary.HumanPercentage + summary.MixedPercentage
	require.Equal(t, 75, visible)
}

func TestRunGitBlameWrapsExecError(t *testing.T) {
	repoRoot := newAttributionRepo(t)

	_, err := runGitBlame(context.Background(), repoRoot, "missing.py")
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Contains(t, err.Error(), "git blame --line-porcelain missing.py")
}

func TestAttributionWhyPreservesLineIndentation(t *testing.T) {
	var out bytes.Buffer
	renderAttributionLineWhy(&out, "auth.py", attributionLine{
		LineNumber:     2,
		Authorship:     attributionHuman,
		Tag:            "[HU]",
		Author:         "Test User",
		ShortCommitSHA: "abcdef12",
		Content:        "    return True",
	})

	require.Contains(t, out.String(), "      return True")
}

func TestAttributionWhyLineJSONShowsMissingMetadataReason(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nmissing_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("missing metadata", checkpointid.MustCheckpointID("fab2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &out, "auth.py:2", attributionWhyOptions{JSON: true}))

	var payload struct {
		File        string                                  `json:"file"`
		Line        attributionLine                         `json:"line"`
		Checkpoints map[string]attributionCheckpointContext `json:"checkpoints,omitempty"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	require.Equal(t, "auth.py", payload.File)
	require.True(t, payload.Line.MetadataMissing)
	require.Contains(t, payload.Line.MetadataMissingReason, "entire checkpoint explain fab2c3d4e5f6")
	require.Contains(t, payload.Line.MetadataMissingReason, "git fetch ")
	require.Contains(t, payload.Line.MetadataMissingReason, "entire/checkpoints/v1:entire/checkpoints/v1")
	checkpointCtx := payload.Checkpoints["fab2c3d4e5f6"]
	require.True(t, checkpointCtx.MetadataMissing)
	require.Equal(t, payload.Line.MetadataMissingReason, checkpointCtx.MetadataMissingReason)
}

func TestAttributionWhyFileJSONShowsMissingMetadataReason(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nmissing_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("missing metadata", checkpointid.MustCheckpointID("eab2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &out, "auth.py", attributionWhyOptions{JSON: true}))

	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	checkpointCtx := payload.Checkpoints["eab2c3d4e5f6"]
	require.True(t, checkpointCtx.MetadataMissing)
	require.Contains(t, checkpointCtx.MetadataMissingReason, "entire checkpoint explain eab2c3d4e5f6")
}

func TestAttributionWhyFileJSONLocalMetadataHasNoMissingReason(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "dab2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-why-file-12345678",
		Prompts:          []string{"Add a line with local checkpoint metadata."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nwhy_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("local metadata", checkpointid.MustCheckpointID("dab2c3d4e5f6")))

	var out bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &out, "auth.py", attributionWhyOptions{JSON: true}))

	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	checkpointCtx := payload.Checkpoints["dab2c3d4e5f6"]
	require.False(t, checkpointCtx.MetadataMissing)
	require.Empty(t, checkpointCtx.MetadataMissingReason)
}

func TestAttributionWhySuccessiveCallsKeepCheckpointMapStable(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nmissing_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("missing metadata", checkpointid.MustCheckpointID("bab2c3d4e5f6")))

	var lineOut bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &lineOut, "auth.py:2", attributionWhyOptions{JSON: true}))
	require.Contains(t, lineOut.String(), "bab2c3d4e5f6")

	var firstOut bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &firstOut, "auth.py", attributionWhyOptions{JSON: true}))
	var firstPayload fileAttributionResult
	require.NoError(t, json.Unmarshal(firstOut.Bytes(), &firstPayload))

	var secondOut bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &secondOut, "auth.py", attributionWhyOptions{JSON: true}))
	var secondPayload fileAttributionResult
	require.NoError(t, json.Unmarshal(secondOut.Bytes(), &secondPayload))

	require.Equal(t, firstPayload.Checkpoints, secondPayload.Checkpoints)
}

func newAttributionRepo(t *testing.T) string {
	t.Helper()
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, "initial human commit")
	return repoRoot
}

func writeAttributionCheckpoint(t *testing.T, repoRoot, checkpointID string, opts checkpoint.WriteOptions) {
	t.Helper()
	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	defer repo.Close()

	opts.CheckpointID = checkpointid.MustCheckpointID(checkpointID)
	opts.Strategy = "manual-commit"
	opts.Branch = "master"
	opts.Transcript = redact.AlreadyRedacted([]byte(`{"type":"user"}` + "\n"))
	opts.AuthorName = "Test User"
	opts.AuthorEmail = attributionTestEmail
	if opts.SessionID == "" {
		opts.SessionID = checkpointID
	}
	require.NoError(t, checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).Write(context.Background(), checkpoint.Session(opts)))

	// WriteCommitted uses git plumbing only, but keep the worktree file system
	// anchored for git CLI blame in these tests.
	require.DirExists(t, filepath.Join(repoRoot, ".git"))
	_, err = os.Stat(filepath.Join(repoRoot, "auth.py"))
	require.NoError(t, err)
}

func formatCheckpointTrailers(message string, checkpointIDs ...string) string {
	var b strings.Builder
	b.WriteString(message)
	b.WriteString("\n\n")
	for _, checkpointID := range checkpointIDs {
		fmt.Fprintf(&b, "%s: %s\n", trailers.CheckpointTrailerKey, checkpointID)
	}
	return b.String()
}

func requireCompactBlameTableFits(t *testing.T, text string, width int) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "Line  Tag"):
		case strings.Contains(line, "──"):
		case strings.Contains(line, "[HU]"):
		case strings.Contains(line, "[AI]"):
		default:
			continue
		}
		require.LessOrEqual(t, len([]rune(line)), width, line)
	}
}

func requireCompactBlameColumnsAlign(t *testing.T, text string) {
	t.Helper()
	lines := strings.Split(text, "\n")
	var header, humanRow, aiRow string
	for _, line := range lines {
		switch {
		case strings.Contains(line, "Line  Tag"):
			header = line
		case humanRow == "" && strings.Contains(line, "[HU]"):
			humanRow = line
		case aiRow == "" && strings.Contains(line, "[AI]"):
			aiRow = line
		}
	}
	require.NotEmpty(t, header)
	require.NotEmpty(t, humanRow)
	require.NotEmpty(t, aiRow)

	tagCol := strings.Index(header, "Tag")
	agentCol := strings.Index(header, "Agent")
	authorCol := strings.Index(header, "Author")
	checkpointCol := strings.Index(header, "Checkpoint")
	require.NotEqual(t, -1, tagCol)
	require.NotEqual(t, -1, agentCol)
	require.NotEqual(t, -1, authorCol)
	require.NotEqual(t, -1, checkpointCol)

	require.Equal(t, tagCol, strings.Index(humanRow, "[HU]"))
	require.Equal(t, tagCol, strings.Index(aiRow, "[AI]"))
	require.Equal(t, 8, authorCol-agentCol)
	require.Equal(t, agentCol, firstNonSpaceIndex(aiRow, agentCol, authorCol))
	require.Equal(t, authorCol, firstNonSpaceIndex(humanRow, authorCol, checkpointCol))
	require.Equal(t, authorCol, firstNonSpaceIndex(aiRow, authorCol, checkpointCol))
	require.NotEmpty(t, strings.TrimSpace(aiRow[agentCol:authorCol]))
	require.NotEmpty(t, strings.TrimSpace(humanRow[authorCol:checkpointCol]))
	require.NotEmpty(t, strings.TrimSpace(aiRow[authorCol:checkpointCol]))
}

func firstNonSpaceIndex(s string, start, end int) int {
	if start < 0 || end > len(s) || start >= end {
		return -1
	}
	for i := start; i < end; i++ {
		if s[i] != ' ' {
			return i
		}
	}
	return -1
}
