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
	"unicode"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
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

// forgeStoredSessionID rewrites every stored JSON file under ref that names
// the session from, so it names to instead. Writes reject terminal controls in
// session IDs, but a checkpoint ref that arrives by fetch never passes through
// that check; this stands in for such a ref.
func forgeStoredSessionID(t *testing.T, repoDir string, ref plumbing.ReferenceName, from, to string) {
	t.Helper()
	repo, err := gitrepo.OpenPath(repoDir)
	require.NoError(t, err)
	defer repo.Close()

	head, err := repo.Reference(ref, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))

	fromJSON, err := json.Marshal(from)
	require.NoError(t, err)
	toJSON, err := json.Marshal(to)
	require.NoError(t, err)
	forged := 0
	for path, entry := range entries {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		blob, err := repo.BlobObject(entry.Hash)
		require.NoError(t, err)
		reader, err := blob.Reader()
		require.NoError(t, err)
		var content bytes.Buffer
		_, err = content.ReadFrom(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		if !bytes.Contains(content.Bytes(), fromJSON) {
			continue
		}
		hash, err := checkpoint.CreateBlobFromContent(repo, bytes.ReplaceAll(content.Bytes(), fromJSON, toJSON))
		require.NoError(t, err)
		entry.Hash = hash
		entries[path] = entry
		forged++
	}
	require.Positive(t, forged, "no stored JSON under %s names session %q", ref, from)

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)
	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, head.Hash(), "forged", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, commitHash)))
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

// The delete preview lists each stored session's ID and agent, and other
// local checkpoints of those sessions. Not parallel: the fixture changes CWD.
func TestCheckpointDeletePreview_SanitizesSessionMetadata(t *testing.T) {
	const placeholder = "sess-placeholder"
	sibling := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1C")

	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeleteCmdFixtureWithSession(t, placeholder, types.AgentType(hostileValue("agent", tc.payload)))
			writeDeleteSiblingCheckpoint(t, f.dir, sibling, placeholder)
			siblingRef, err := checkpoint.RefName(sibling)
			require.NoError(t, err)
			sessionID := hostileValue("session", tc.payload)
			forgeStoredSessionID(t, f.dir, f.ref, placeholder, sessionID)
			forgeStoredSessionID(t, f.dir, siblingRef, placeholder, sessionID)

			stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--dry-run")
			require.NoError(t, err)

			require.Contains(t, stdout, "Sessions in this checkpoint")
			require.Contains(t, stdout, "Other local checkpoints of these sessions")
			assertSanitizedField(t, stdout, "agent")
			assertSanitizedField(t, stdout, "session")
			assert.Equal(t, 2, strings.Count(stdout, "session-right"), "session ID in sessions and siblings:\n%s", stdout)
		})
	}
}

func TestPrintCheckpointDeletePlan_SanitizesSessionStates(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := &strategy.CheckpointDeletePlan{
				CheckpointID:  id.MustCheckpointID("a1b2c3d4e5f6"),
				SessionStates: []strategy.CheckpointDeleteState{{SessionID: hostileValue("state", tc.payload), Fields: []string{"last_checkpoint_id"}}},
			}
			var out bytes.Buffer
			printCheckpointDeletePlan(&out, plan, nil, true)
			assertSanitizedField(t, out.String(), "state")
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

// Blame and why print the agent, model and session of the checkpoint linked
// to each line. Not parallel: the fixture changes CWD.
func TestAttributionText_SanitizesCheckpointMetadata(t *testing.T) {
	const placeholder = "session-placeholder"

	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := newAttributionRepo(t)
			writeAttributionCheckpoint(t, repoRoot, "a1b2c3d4e5f6", checkpoint.WriteOptions{
				SessionID:        placeholder,
				Prompts:          []string{"Add an agent-owned helper."},
				FilesTouched:     []string{"auth.py"},
				Agent:            types.AgentType(hostileValue("agent", tc.payload)),
				Model:            hostileValue("model", tc.payload),
				CheckpointsCount: 1,
			})
			// Payload first: the views print only the session ID's first bytes.
			sessionID := tc.payload + "session-visible"
			forgeStoredSessionID(t, repoRoot, plumbing.NewBranchReferenceName(paths.MetadataBranchName), placeholder, sessionID)
			testutil.WriteFile(t, repoRoot, "auth.py", "human_line = 1\nai_line = 2\n")
			testutil.GitAdd(t, repoRoot, "auth.py")
			testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("agent update", id.MustCheckpointID("a1b2c3d4e5f6")))
			ctx := context.Background()

			var compact, long, fileWhy, lineWhy, jsonOut bytes.Buffer
			require.NoError(t, runAttributionBlame(ctx, &compact, "auth.py", attributionBlameOptions{}))
			assertNoTerminalControls(t, compact.String())
			require.NoError(t, runAttributionBlame(ctx, &long, "auth.py", attributionBlameOptions{Long: true}))
			assertNoTerminalControls(t, long.String())
			require.NoError(t, runAttributionWhy(ctx, &fileWhy, "auth.py", attributionWhyOptions{}))
			assertSanitizedField(t, fileWhy.String(), "agent")
			require.Contains(t, fileWhy.String(), "session ")
			require.NoError(t, runAttributionWhy(ctx, &lineWhy, "auth.py:2", attributionWhyOptions{}))
			assertSanitizedField(t, lineWhy.String(), "agent")
			assertSanitizedField(t, lineWhy.String(), "model")
			require.Contains(t, lineWhy.String(), "session ")

			require.NoError(t, runAttributionBlame(ctx, &jsonOut, "auth.py", attributionBlameOptions{LineFlag: "2", JSON: true}))
			var payload fileAttributionResult
			require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &payload))
			require.Len(t, payload.Lines, 1)
			assert.Equal(t, hostileValue("agent", tc.payload), payload.Lines[0].Agent, "JSON keeps the stored value")
			assert.Equal(t, sessionID, payload.Lines[0].SessionID, "JSON keeps the stored value")
		})
	}
}

// The attribution renderers sanitize their own input, so a caller that hands
// them stored metadata directly cannot reach the terminal with it either.
func TestAttributionRenderers_SanitizeMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := func(checkpointID string) attributionCandidate {
				return attributionCandidate{
					CheckpointID: checkpointID,
					SessionID:    tc.payload + "candidate-session",
					Agent:        hostileValue("candidate", tc.payload),
					Model:        hostileValue("candidate-model", tc.payload),
				}
			}
			line := attributionLine{
				LineNumber:   2,
				Status:       lineStatusCheckpoint,
				Author:       hostileValue("author", tc.payload),
				CheckpointID: "a1b2c3d4e5f6",
				SessionID:    tc.payload + "session-visible",
				Agent:        hostileValue("agent", tc.payload),
				Model:        hostileValue("model", tc.payload),
				// A source line never contains a newline, and its tabs are
				// kept as indentation, so the content payload drops both.
				Content:    hostileValue("content", strings.NewReplacer("\n", "", "\t", "").Replace(tc.payload)),
				Candidates: []attributionCandidate{candidate("a1b2c3d4e5f6"), candidate("b1b2c3d4e5f6")},
			}
			result := &fileAttributionResult{
				File:        hostileValue("file", tc.payload),
				Lines:       []attributionLine{line},
				Checkpoints: map[string]attributionCheckpointContext{"a1b2c3d4e5f6": candidate("a1b2c3d4e5f6")},
				Summary:     summarizeAttributionLines([]attributionLine{line}),
			}

			var lineWhy, fileWhy, compact, long bytes.Buffer
			renderAttributionLineWhy(&lineWhy, result.File, line)
			for _, field := range []string{"file", "content", "agent", "model", "candidate"} {
				assertSanitizedField(t, lineWhy.String(), field)
			}
			renderAttributionFileWhy(&fileWhy, result)
			for _, field := range []string{"file", "candidate"} {
				assertSanitizedField(t, fileWhy.String(), field)
			}
			renderAttributionBlameCompact(&compact, result, "")
			assertSanitizedField(t, compact.String(), "file")
			renderAttributionBlameLong(&long, result, "")
			for _, field := range []string{"file", "content"} {
				assertSanitizedField(t, long.String(), field)
			}

			assert.Equal(t, hostileValue("agent", tc.payload), result.Lines[0].Agent, "rendering must not modify the caller's result")
		})
	}
}

// entire status prints the agent, model and last prompt of every active
// session from local state, which agents write.
func TestStatusText_SanitizesActiveSessions(t *testing.T) {
	// setupTestRepo changes the working directory, so no t.Parallel.
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupTestRepo(t)
			ctx := context.Background()
			store, err := session.NewStateStore(ctx)
			require.NoError(t, err)
			require.NoError(t, store.Save(ctx, &session.State{
				SessionID:    "status-session",
				WorktreePath: repoRoot,
				StartedAt:    time.Now(),
				AgentType:    types.AgentType(hostileValue("agent", tc.payload)),
				ModelName:    hostileValue("model", tc.payload),
				LastPrompt:   hostileValue("prompt", tc.payload),
			}))

			var out bytes.Buffer
			writeActiveSessions(ctx, &out, newStatusStyles(&out))
			for _, field := range []string{"agent", "model", "prompt"} {
				assertSanitizedField(t, out.String(), field)
			}
		})
	}
}

// entire clean --all lists stored names before and after deleting them.
func TestCleanAllSections_SanitizeStoredNames(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			printSection(&out, "Session states", []string{hostileValue("listed", tc.payload)})
			printResultSection(&out, "Session states", []string{hostileValue("deleted", tc.payload)})
			assertSanitizedField(t, out.String(), "listed")
			assertSanitizedField(t, out.String(), "deleted")
		})
	}
}

// The session and checkpoint resume pickers label rows with stored agents,
// prompts and branch names; checkpoint agents can arrive by fetch.
func TestResumePickerLabels_SanitizeMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := &strategy.SessionState{
				SessionID:  "picker-session",
				AgentType:  types.AgentType(hostileValue("agent", tc.payload)),
				LastPrompt: hostileValue("prompt", tc.payload),
				StartedAt:  time.Now(),
			}
			cpID := id.MustCheckpointID("a1b2c3d4e5f6")
			resumable := resumeOptionLabel(resumableSession{state: state, branch: hostileValue("branch", tc.payload), checkpointID: cpID})
			for _, field := range []string{"branch", "prompt", "agent"} {
				assertSanitizedField(t, resumable, field)
			}
			unresumable := resumeOptionLabel(resumableSession{state: state})
			for _, field := range []string{"prompt", "agent"} {
				assertSanitizedField(t, unresumable, field)
			}
			assertSanitizedField(t, unresumableSessionMessage(resumableSession{state: &strategy.SessionState{
				SessionID: hostileValue("session", tc.payload),
			}}), "session")

			info := checkpoint.CheckpointInfo{
				CheckpointID: cpID,
				Agent:        types.AgentType(hostileValue("agent", tc.payload)),
				CreatedAt:    time.Now(),
			}
			branchIndex := map[string]string{cpID.String(): hostileValue("branch", tc.payload)}
			label := checkpointResumeOptionLabel(info, branchIndex)
			var list bytes.Buffer
			printCheckpointResumeList(&list, []checkpoint.CheckpointInfo{info}, branchIndex)
			for _, field := range []string{"branch", "agent"} {
				assertSanitizedField(t, label, field)
				assertSanitizedField(t, list.String(), field)
			}
		})
	}
}

// Load validates the session ID in a state file's name but returns the
// session_id from its body unchecked, so doctor and session adopt must
// sanitize the ID they print.
func TestForgedStateBodySessionID_Sanitized(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stateDir := t.TempDir()
			sourceWorktree := t.TempDir()
			now := time.Now()
			writeForgedState := func(fileID string, state session.State) {
				t.Helper()
				state.StartedAt = now
				state.LastInteractionTime = &now
				data, err := json.Marshal(state)
				require.NoError(t, err)
				testutil.WriteFile(t, stateDir, fileID+".json", string(data))
			}
			writeForgedState("forged-a", session.State{SessionID: hostileValue("session", tc.payload), WorktreePath: sourceWorktree})
			writeForgedState("forged-b", session.State{SessionID: hostileValue("other", tc.payload), WorktreePath: sourceWorktree})
			writeForgedState("forged-c", session.State{SessionID: "forged-c", WorktreePath: hostileValue("worktree", tc.payload)})
			store := session.NewStateStoreWithDir(stateDir)

			loaded, err := store.Load(ctx, "forged-a")
			require.NoError(t, err)
			require.NotNil(t, loaded)
			require.Equal(t, hostileValue("session", tc.payload), loaded.SessionID, "load returns the body's session ID")

			assertSanitizedField(t, stuckSessionPromptTitle(stuckSession{State: loaded}), "session")

			// Payload first: the adopt line prints only the ID's first bytes.
			var adopted bytes.Buffer
			writeAdoptResult(&adopted, &session.State{SessionID: tc.payload + "sessionvisible"}, sourceWorktree,
				adoptChanges{filesTouched: []string{hostileValue("file", tc.payload)}})
			assertSanitizedField(t, adopted.String(), "file")
			require.Contains(t, adopted.String(), "Adopted session ")

			_, err = selectAdoptSourceSession(ctx, store, sourceWorktree, "")
			require.Error(t, err)
			assertSanitizedField(t, err.Error(), "session")
			assertSanitizedField(t, err.Error(), "other")

			_, err = selectAdoptSourceSession(ctx, store, sourceWorktree, "forged-c")
			require.Error(t, err)
			assertSanitizedField(t, err.Error(), "worktree")
		})
	}
}

// main.go and the other error render sites print returned errors through
// RenderUserFacingError, and many errors name a stored session ID or carry a
// server message. The rendered message drops terminal controls but keeps line
// breaks and tabs, since some errors are multi-line, and the chain still
// unwraps.
func TestRenderUserFacingError_SanitizesMessage(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("session %s is ended: %w", hostileValue("session", tc.payload), sentinel)
			rendered := RenderUserFacingError(err)
			require.ErrorIs(t, rendered, sentinel)
			msg := rendered.Error()
			assert.NotContains(t, msg, "\x1b", "ESC reached the output")
			assert.NotContains(t, msg, "example.invalid", "OSC payload reached the output")
			for _, r := range msg {
				if r == '\n' || r == '\t' {
					continue
				}
				if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
					t.Fatalf("rendered error carries %U: %q", r, msg)
				}
			}
			assert.Contains(t, msg, "session-left")
			assert.Contains(t, msg, "session-right")
		})
	}

	t.Run("clean errors render unchanged", func(t *testing.T) {
		t.Parallel()
		err := fmt.Errorf("first line\n\tsecond line: %w", sentinel)
		assert.Same(t, err, RenderUserFacingError(err))
	})
}

// trail resume prints the server's trail fields in its header and suggests
// commands naming the branch, project and fetched session IDs.
func TestTrailResumeText_SanitizesTrailAndCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trail := trailResumeTrailContext{
				Number:  42,
				Title:   hostileValue("title", tc.payload),
				Project: "gh/acme",
				Branch:  hostileValue("branch", tc.payload),
				Base:    hostileValue("base", tc.payload),
				Status:  hostileValue("status", tc.payload),
				Phase:   hostileValue("phase", tc.payload),
				URL:     hostileValue("url", tc.payload),
			}
			var header bytes.Buffer
			printTrailResumeTrail(&header, trail)
			for _, field := range []string{"title", "status", "phase", "branch", "base", "url"} {
				assertSanitizedField(t, header.String(), field)
			}

			hostileID := hostileValue("session", tc.payload)
			commands := buildTrailResumeCommands(trailResumeContext{
				Trail:    trail,
				Sessions: []trailResumeSessionContext{{SessionID: hostileID}, {SessionID: "valid-session"}},
			})
			for _, command := range commands {
				assert.NotContains(t, command, "session-left", "a command names a session ID resume would refuse")
			}
			assert.Contains(t, strings.Join(commands, "\n"), "--session valid-session")

			var printed bytes.Buffer
			printTrailResumeCommands(&printed, commands)
			assertSanitizedField(t, printed.String(), "branch")
		})
	}
}

// Session state is local, but agents write it, and session list, info,
// doctor and clean print it.
func TestSessionStateText_SanitizesStoredLabels(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			state := &strategy.SessionState{
				SessionID:           hostileValue("session", tc.payload),
				AgentType:           types.AgentType(hostileValue("agent", tc.payload)),
				ModelName:           hostileValue("model", tc.payload),
				WorktreeID:          hostileValue("worktree", tc.payload),
				LastPrompt:          hostileValue("prompt", tc.payload),
				Phase:               session.Phase(hostileValue("phase", tc.payload)),
				FilesTouched:        []string{hostileValue("file", tc.payload)},
				StartedAt:           now,
				LastInteractionTime: &now,
			}

			var card bytes.Buffer
			writeSessionCard(&card, state, newStatusStyles(&card))
			for _, field := range []string{"session", "agent", "model", "worktree", "prompt", "phase"} {
				assertSanitizedField(t, card.String(), field)
			}

			// session info prints the last prompt with %q, which escapes it.
			infoState := *state
			infoState.LastPrompt = ""
			var info bytes.Buffer
			require.NoError(t, writeSessionInfoText(&info, &infoState, sessionPhaseLabel(&infoState), strategy.ResolutionNone))
			for _, field := range []string{"session", "agent", "model", "worktree", "file", "phase"} {
				assertSanitizedField(t, info.String(), field)
			}

			var doctor bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&doctor)
			displayStuckSession(cmd, stuckSession{State: state, Reason: "stuck"})
			for _, field := range []string{"session", "agent", "phase"} {
				assertSanitizedField(t, doctor.String(), field)
			}

			var tokens bytes.Buffer
			writeSessionTokensText(&tokens, buildSessionTokensReport(state, sessionPhaseLabel(state)))
			assertSanitizedField(t, tokens.String(), "phase")

			var clean bytes.Buffer
			printCurrentHeadCleanItems(&clean, []*strategy.SessionState{state}, nil)
			assertSanitizedField(t, clean.String(), "session")
		})
	}
}

// Resume prints restored session IDs, including inside the resume command a
// user is likely to copy, and the restored prompt as a comment after it.
func TestResumeText_SanitizesRestoredSessions(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileMetadataPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			restored := strategy.RestoredSession{
				SessionID:    hostileValue("session", tc.payload),
				CheckpointID: "a1b2c3d4e5f6",
				Agent:        agent.AgentTypeClaudeCode,
				Prompt:       hostileValue("prompt", tc.payload),
			}

			var single bytes.Buffer
			require.NoError(t, displayRestoredSessions(&single, []strategy.RestoredSession{restored}))
			assertSanitizedField(t, single.String(), "session")
			assertSanitizedField(t, single.String(), "prompt")
			require.Contains(t, single.String(), "claude -r session-left")

			second := restored
			second.CreatedAt = time.Now()
			var multi bytes.Buffer
			require.NoError(t, displayRestoredSessions(&multi, []strategy.RestoredSession{restored, second}))
			assertSanitizedField(t, multi.String(), "session")
			assertSanitizedField(t, multi.String(), "prompt")

			var trailSummary bytes.Buffer
			printTrailRestoredSessionSummary(&trailSummary, []strategy.RestoredSession{{SessionID: restored.SessionID}})
			assertSanitizedField(t, trailSummary.String(), "session")

			trailSession := trailResumeSessionContext{
				SessionID:    hostileValue("trailsession", tc.payload),
				Agent:        hostileValue("agent", tc.payload),
				LastPrompt:   hostileValue("prompt", tc.payload),
				CheckpointID: hostileValue("checkpoint", tc.payload),
			}
			var trailTable bytes.Buffer
			printTrailResumeSessions(&trailTable, []trailResumeSessionContext{trailSession}, "", 0)
			for _, field := range []string{"agent", "prompt", "checkpoint"} {
				assertSanitizedField(t, trailTable.String(), field)
			}

			label := trailResumeSessionOptionLabel(trailSession)
			for _, field := range []string{"trailsession", "agent", "prompt"} {
				assertSanitizedField(t, label, field)
			}

			choice := trailRestoredSessionChoiceLabel(strategy.RestoredSession{
				SessionID: restored.SessionID,
				Agent:     types.AgentType(hostileValue("agent", tc.payload)),
				Prompt:    restored.Prompt,
			}, true)
			for _, field := range []string{"session", "agent", "prompt"} {
				assertSanitizedField(t, choice, field)
			}
		})
	}
}
