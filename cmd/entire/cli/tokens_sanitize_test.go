package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/redact"
)

// hostileMetadataPayloads are terminal-control payloads a stored session ID,
// agent, model, branch or skill label could carry. Each is placed between two
// visible markers so a test can check that the payload is gone and the
// surrounding text is still printed on one line.
var hostileMetadataPayloads = []struct {
	name    string
	payload string
}{
	{"CSI clear screen", "\x1b[2J"},
	{"OSC 8 hyperlink terminated by BEL", "\x1b]8;;http://example.invalid\a"},
	{"OSC 52 clipboard terminated by ST", "\x1b]52;c;QQ==\x1b\\"},
	{"C0 controls", "\a\b\r\x00\x7f"},
	{"C1 controls", "\u009b2J\u0085"},
	{"bidi override", "\u202Egnp.exe\u202C"},
	{"line break forging a report line", "\n- forged recommendation\t"},
}

func hostileValue(field, payload string) string {
	return field + "-left" + payload + field + "-right"
}

// assertSanitizedField fails when out carries a terminal control, or when the
// visible text around a payload is missing or split across lines.
func assertSanitizedField(t *testing.T, out, field string) {
	t.Helper()
	assertNoTerminalControls(t, out)
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, field+"-left") {
			line = l
			break
		}
	}
	require.NotEmpty(t, line, "%s is missing from the output:\n%s", field, out)
	assert.Contains(t, line, field+"-right", "%s was split across lines:\n%s", field, out)
}

func assertNoTerminalControls(t *testing.T, out string) {
	t.Helper()
	assert.NotContains(t, out, "\x1b", "ESC reached the output")
	assert.NotContains(t, out, "example.invalid", "OSC payload reached the output")
	assert.NotContains(t, out, "forged recommendation\n", "a stored value forged its own line")
	for _, r := range out {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			t.Errorf("output carries %U:\n%q", r, out)
			return
		}
	}
}

func TestCheckpointTokensText_SanitizesMetadata(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta := func(n string) *checkpoint.Metadata {
				return &checkpoint.Metadata{
					SessionID:   hostileValue("session"+n, tc.payload),
					Agent:       types.AgentType(hostileValue("agent"+n, tc.payload)),
					Model:       hostileValue("model"+n, tc.payload),
					Branch:      hostileValue("branch"+n, tc.payload),
					TokenUsage:  &agent.TokenUsage{InputTokens: 100, OutputTokens: 20, APICallCount: 1},
					SkillEvents: []agent.SkillEvent{{Collapse: agent.SkillEventCollapse{Label: hostileValue("skill"+n, tc.payload)}}},
				}
			}

			single := buildCheckpointTokensReport(cpID, nil, []*checkpoint.Metadata{meta("1")}, 0)
			assert.Equal(t, hostileValue("agent1", tc.payload), single.Agent, "the report keeps raw values; sanitizing is the text writer's job")
			var text bytes.Buffer
			writeCheckpointTokensText(&text, single)
			for _, field := range []string{"session1", "agent1", "model1", "branch1", "skill1"} {
				assertSanitizedField(t, text.String(), field)
			}
			var brief bytes.Buffer
			writeCheckpointTokensAgentBrief(&brief, single)
			assertNoTerminalControls(t, brief.String())

			multi := buildCheckpointTokensReport(cpID, nil, []*checkpoint.Metadata{meta("1"), meta("2")}, 0)
			var multiText bytes.Buffer
			writeCheckpointTokensText(&multiText, multi)
			for _, field := range []string{"agent1", "agent2", "model1", "model2", "branch1", "skill2"} {
				assertSanitizedField(t, multiText.String(), field)
			}
		})
	}
}

func TestSessionTokensText_SanitizesSessionState(t *testing.T) {
	t.Parallel()

	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := &strategy.SessionState{
				SessionID:   hostileValue("session", tc.payload),
				AgentType:   types.AgentType(hostileValue("agent", tc.payload)),
				ModelName:   hostileValue("model", tc.payload),
				TokenUsage:  &agent.TokenUsage{InputTokens: 100, OutputTokens: 20, APICallCount: 1},
				SkillEvents: []agent.SkillEvent{{Collapse: agent.SkillEventCollapse{Label: hostileValue("skill", tc.payload)}}},
			}
			report := buildSessionTokensReport(state, "active")
			assert.Equal(t, hostileValue("session", tc.payload), report.SessionID, "the report keeps raw values; sanitizing is the text writer's job")

			var text bytes.Buffer
			writeSessionTokensText(&text, report)
			for _, field := range []string{"session", "agent", "model", "skill"} {
				assertSanitizedField(t, text.String(), field)
			}

			var brief bytes.Buffer
			writeSessionTokensAgentBrief(&brief, report)
			assertSanitizedField(t, brief.String(), "session")
		})
	}
}

// The delete preview lists each stored session's ID and agent, the local
// session states it clears, and other local checkpoints of those sessions.
// Writes reject C0 controls in session IDs, so the session ID carries the C1
// and bidi controls that validation lets through, and the agent carries every
// payload. Not parallel: the fixture changes CWD.
func TestCheckpointDeletePreview_SanitizesSessionMetadata(t *testing.T) {
	const sessionPayload = "\u202Egnp.exe\u202C\u009b2J"
	sessionID := hostileValue("session", sessionPayload)
	sibling := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1C")

	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeleteCmdFixtureWithSession(t, sessionID, types.AgentType(hostileValue("agent", tc.payload)))
			writeDeleteSiblingCheckpoint(t, f.dir, sibling, sessionID)
			now := time.Now()
			require.NoError(t, strategy.SaveSessionState(context.Background(), &strategy.SessionState{
				SessionID: sessionID, StartedAt: now, LastInteractionTime: &now, Phase: session.PhaseEnded, LastCheckpointID: f.cid,
			}))

			stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--dry-run")
			require.NoError(t, err)

			require.Contains(t, stdout, "Sessions in this checkpoint")
			require.Contains(t, stdout, "Local session state to clear")
			require.Contains(t, stdout, "Other local checkpoints of these sessions")
			assertSanitizedField(t, stdout, "agent")
			assertNoTerminalControls(t, stdout)
			assert.Equal(t, 3, strings.Count(stdout, "session-left"), "session ID in sessions, states and siblings:\n%s", stdout)
			assert.Equal(t, 3, strings.Count(stdout, "session-right"), "session ID in sessions, states and siblings:\n%s", stdout)
		})
	}
}

func writeDeleteSiblingCheckpoint(t *testing.T, dir string, cid id.CheckpointID, sessionID string) {
	t.Helper()
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Agent:        "Claude Code",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
}
