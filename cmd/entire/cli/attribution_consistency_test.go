package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/stretchr/testify/require"
)

func newStubAttributionResolver(reader *attributionCheckpointReaderStub) *attributionResolver {
	return &attributionResolver{
		ctx:             context.Background(),
		store:           reader,
		checkpointCache: make(map[string]attributionCheckpointContext),
	}
}

// Bug #1 (phantom prompt): a prompt sourced from the session's ReviewPrompt seed
// (rather than the checkpoint's own recorded prompt) must be flagged session-level
// so `why` can label it and point users at `checkpoint explain` instead of
// implying the seed prompt lives in this checkpoint's transcript.
func TestReadCheckpointContextFlagsReviewPromptAsSessionLevel(t *testing.T) {
	t.Parallel()
	cpID := checkpointid.MustCheckpointID("c1c2c3c4d5e6")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:    "session-trail",
				FilesTouched: []string{"auth.py"},
				Agent:        agent.AgentTypeClaudeCode,
				ReviewPrompt: "work on this trail please",
			},
			Prompts: "",
		},
	}
	ctx := newStubAttributionResolver(reader).readCheckpointContext(cpID, "auth.py")
	require.Equal(t, "work on this trail please", ctx.Prompt)
	require.True(t, ctx.PromptSessionLevel, "ReviewPrompt seed must be flagged session-level")
}

// Bug #1 (general case): prompt.txt is session-wide (extracted from transcript
// offset 0). On a LATER checkpoint (transcript start > 0) the leading prompt is
// from earlier in the session, so it may not match `checkpoint explain` for this
// checkpoint — it must be flagged session-level even though prompt.txt is non-empty.
func TestReadCheckpointContextFlagsSessionWidePromptOnLaterCheckpoint(t *testing.T) {
	t.Parallel()
	cpID := checkpointid.MustCheckpointID("d5e6f7a8b9c0")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:                 "session-multi-turn",
				FilesTouched:              []string{"auth.py"},
				Agent:                     agent.AgentTypeClaudeCode,
				CheckpointTranscriptStart: 120, // a later checkpoint in the session
			},
			Prompts: "work on this trail please\nthen add a leaderboard route",
		},
	}
	ctx := newStubAttributionResolver(reader).readCheckpointContext(cpID, "auth.py")
	require.True(t, ctx.PromptSessionLevel, "session-wide prompt on a later checkpoint must be flagged")
}

func TestReadCheckpointContextKeepsCheckpointPromptNotSessionLevel(t *testing.T) {
	t.Parallel()
	cpID := checkpointid.MustCheckpointID("c2c3c4d5e6f7")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:    "session-real",
				FilesTouched: []string{"auth.py"},
				Agent:        agent.AgentTypeClaudeCode,
				ReviewPrompt: "seed prompt",
			},
			Prompts: "Refactor the auth check.",
		},
	}
	ctx := newStubAttributionResolver(reader).readCheckpointContext(cpID, "auth.py")
	require.Equal(t, "Refactor the auth check.", ctx.Prompt)
	require.False(t, ctx.PromptSessionLevel)
}

// Bug #3a: a single-session checkpoint whose recorded paths don't include the
// file must still flag SessionFallback (it was previously gated on >1 session,
// so single-session mismatches printed a prompt with no caveat).
func TestReadCheckpointContextFlagsFallbackForSingleSession(t *testing.T) {
	t.Parallel()
	cpID := checkpointid.MustCheckpointID("c3c4d5e6f7a8")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:    "session-elsewhere",
				FilesTouched: []string{"other.py"},
				Agent:        agent.AgentTypeClaudeCode,
			},
		},
	}
	ctx := newStubAttributionResolver(reader).readCheckpointContext(cpID, "auth.py")
	require.Equal(t, "session-elsewhere", ctx.SessionID)
	require.True(t, ctx.SessionFallback, "single-session path mismatch must flag fallback")
}

// Audit must-fix: a session that recorded NO paths is not evidence of a rename,
// so the SessionFallback "may have been renamed" caveat must NOT fire. (The gate
// was relaxed to flag single-session mismatches, but an empty FilesTouched means
// "unknown", not "renamed".)
func TestReadCheckpointContextDoesNotFlagFallbackWhenPathsUnknown(t *testing.T) {
	t.Parallel()
	cpID := checkpointid.MustCheckpointID("c4d5e6f7a8b9")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:    "session-no-paths",
				FilesTouched: nil, // session recorded no paths
				Agent:        agent.AgentTypeClaudeCode,
			},
		},
	}
	ctx := newStubAttributionResolver(reader).readCheckpointContext(cpID, "auth.py")
	require.Equal(t, "session-no-paths", ctx.SessionID)
	require.False(t, ctx.SessionFallback, "no recorded paths is not evidence of a rename")
}

// Bug #4 completion: why's positional parser must agree with blame's
// splitFileLineSpec — a colon-then-non-numeric suffix is part of the filename,
// and a range gets a friendly "use entire blame" message instead of the generic
// error (and crucially leaves --line free to disambiguate).
func TestParseAttributionWhyTargetTreatsColonNonNumericAsFilename(t *testing.T) {
	t.Parallel()
	file, line, hasLine, err := parseAttributionWhyTarget("weird:name")
	require.NoError(t, err)
	require.False(t, hasLine)
	require.Equal(t, "weird:name", file)
	require.Zero(t, line)
}

func TestParseAttributionWhyTargetRejectsRangeWithFriendlyMessage(t *testing.T) {
	t.Parallel()
	_, _, _, err := parseAttributionWhyTarget("src/main.js:12-20")
	require.ErrorContains(t, err, "range")
}

func TestParseAttributionWhyTargetParsesFileColonLine(t *testing.T) {
	t.Parallel()
	file, line, hasLine, err := parseAttributionWhyTarget("auth.py:2")
	require.NoError(t, err)
	require.True(t, hasLine)
	require.Equal(t, "auth.py", file)
	require.Equal(t, 2, line)
}

// Bug #1 render: session-level prompts get a distinct label + caveat in `why`
// so users understand why the prompt may not appear in `checkpoint explain`.
func TestWhyRendersSessionLevelPromptCaveat(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	renderAttributionLineWhy(&out, "src/main.js", attributionLine{
		LineNumber:         279,
		Authorship:         attributionMixed,
		Tag:                "[MX]",
		Agent:              "Codex",
		Model:              "gpt-5.5",
		CheckpointID:       "bfc2c1df9e4b",
		SessionID:          "019edf9f",
		Prompt:             "work on this trail please",
		PromptSessionLevel: true,
	})
	s := out.String()
	require.Contains(t, s, "Session prompt:")
	require.Contains(t, s, "may not appear in this checkpoint")
	require.Contains(t, s, "checkpoint explain")
}

func TestWhyRendersPlainPromptLabelForCheckpointPrompt(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	renderAttributionLineWhy(&out, "a.go", attributionLine{
		LineNumber:   1,
		Authorship:   attributionAI,
		Tag:          "[AI]",
		Agent:        "Claude",
		CheckpointID: "abc123abc123",
		Prompt:       "do the thing",
	})
	s := out.String()
	require.Contains(t, s, "Prompt:")
	require.NotContains(t, s, "Session prompt:")
	require.NotContains(t, s, "may not appear in this checkpoint")
}

// Bug #4: shared file:line[-range] splitter used to give blame and why the same
// `file:line` syntax.
func TestSplitFileLineSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, file, spec string
	}{
		{"auth.py:2", "auth.py", "2"},
		{"auth.py:12-20", "auth.py", "12-20"},
		{"auth.py", "auth.py", ""},
		{"dir/sub/file.go:5", "dir/sub/file.go", "5"},
		{"weird:name", "weird:name", ""},
		{"trailing:", "trailing:", ""},
		{"file:1:2", "file:1", "2"},
	}
	for _, c := range cases {
		file, spec := splitFileLineSpec(c.in)
		require.Equalf(t, c.file, file, "file for %q", c.in)
		require.Equalf(t, c.spec, spec, "spec for %q", c.in)
	}
}

// Bug #4: blame accepts file:line and file:range positionally, matching why.
func TestBlameAcceptsFileColonLine(t *testing.T) {
	attributionRepoWithAILine2(t)
	var out bytes.Buffer
	require.NoError(t, runAttributionBlame(context.Background(), &out, "auth.py:2", attributionBlameOptions{JSON: true}))
	var payload fileAttributionResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	require.Len(t, payload.Lines, 1)
	require.Equal(t, 2, payload.Lines[0].LineNumber)
}

func TestBlameRejectsLineSpecWithFlag(t *testing.T) {
	attributionRepoWithAILine2(t)
	var out bytes.Buffer
	err := runAttributionBlame(context.Background(), &out, "auth.py:2", attributionBlameOptions{LineFlag: "1"})
	require.ErrorContains(t, err, "not both")
}

// Bug #4: why accepts --line as an alias for file:line, matching blame.
func TestWhyAcceptsLineFlag(t *testing.T) {
	attributionRepoWithAILine2(t)
	var out bytes.Buffer
	require.NoError(t, runAttributionWhy(context.Background(), &out, "auth.py", attributionWhyOptions{LineFlag: "2"}))
	require.Contains(t, out.String(), "Line 2")
}

func TestWhyRejectsLineSpecWithFlag(t *testing.T) {
	attributionRepoWithAILine2(t)
	var out bytes.Buffer
	err := runAttributionWhy(context.Background(), &out, "auth.py:2", attributionWhyOptions{LineFlag: "3"})
	require.ErrorContains(t, err, "not both")
}

func TestWhyLineFlagRejectsRange(t *testing.T) {
	attributionRepoWithAILine2(t)
	var out bytes.Buffer
	err := runAttributionWhy(context.Background(), &out, "auth.py", attributionWhyOptions{LineFlag: "2-3"})
	require.ErrorContains(t, err, "single line")
}

// Item 2: the compact blame table must disclose approximate (SessionFallback /
// MetadataMissing) and ambiguous (multiple candidate checkpoints) lines with a
// marker + legend, mirroring what `why` already shows, without breaking column
// alignment.
func TestBlameCompactMarksApproximateAndAmbiguousLines(t *testing.T) {
	t.Parallel()
	lines := []attributionLine{
		{LineNumber: 1, Authorship: attributionHuman, Author: "blackg", Content: "human = 1"},
		{LineNumber: 2, Authorship: attributionAI, Agent: "Claude", Author: "blackg", CheckpointID: "a1b2c3d4e5f6", Content: "ok = 2"},
		{LineNumber: 3, Authorship: attributionAI, Agent: "Codex", Author: "blackg", CheckpointID: "b1b2c3d4e5f6", SessionFallback: true, Content: "guess = 3"},
		{
			LineNumber: 4, Authorship: attributionMixed, Agent: "Codex", Author: "blackg", CheckpointID: "c1b2c3d4e5f6",
			Candidates: []attributionCandidate{{CheckpointID: "c1b2c3d4e5f6"}, {CheckpointID: "d1b2c3d4e5f6"}},
			Content:    "amb = 4",
		},
	}
	result := &fileAttributionResult{File: "f.py", Lines: lines, Summary: summarizeAttributionLines(lines)}

	var out bytes.Buffer
	renderAttributionBlameCompact(&out, result, "")
	text := out.String()

	require.Contains(t, text, "~", "approximate line should carry a marker")
	require.Contains(t, text, "?", "ambiguous line should carry a marker")
	require.Contains(t, text, "best-effort attribution")
	require.Contains(t, text, "candidate checkpoints")
	requireCompactBlameColumnsAlign(t, text)
	requireCompactBlameTableFits(t, text, 80)
}

func TestBlameCompactNoLegendWhenAllConfident(t *testing.T) {
	t.Parallel()
	lines := []attributionLine{
		{LineNumber: 1, Authorship: attributionHuman, Author: "blackg", Content: "human = 1"},
		{LineNumber: 2, Authorship: attributionAI, Agent: "Claude", Author: "blackg", CheckpointID: "a1b2c3d4e5f6", Content: "ok = 2"},
	}
	result := &fileAttributionResult{File: "f.py", Lines: lines, Summary: summarizeAttributionLines(lines)}

	var out bytes.Buffer
	renderAttributionBlameCompact(&out, result, "")
	text := out.String()
	require.NotContains(t, text, "best-effort attribution")
	require.NotContains(t, text, "candidate checkpoints")
	requireCompactBlameColumnsAlign(t, text)
}

func attributionRepoWithAILine2(t *testing.T) {
	t.Helper()
	repoRoot := newAttributionRepo(t)
	writeAttributionCheckpoint(t, repoRoot, "e1e2e3e4e5f6", checkpoint.WriteOptions{
		SessionID:        "session-line-12345678",
		Prompts:          []string{"Add an AI line."},
		FilesTouched:     []string{"auth.py"},
		Agent:            agent.AgentTypeClaudeCode,
		CheckpointsCount: 1,
	})
	testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
	testutil.GitAdd(t, repoRoot, "auth.py")
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("ai update", checkpointid.MustCheckpointID("e1e2e3e4e5f6")))
}
