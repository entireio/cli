# OpenCode Native Subagent Tracking Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record each OpenCode `task` tool call as a durable task record on the parent session, with the child's transcript and exact tokens, and stop child sessions from appearing as top-level Entire sessions.

**Architecture:** The embedded OpenCode plugin learns child sessions from `parentID`, suppresses their lifecycle hooks, and fires two new hooks: `subagent-start` from the parent's task part once it carries the child ID, and `subagent-stop` from `tool.execute.after`. The Go adapter exports the child transcript at stop time and emits a `Final` + `CompletionWithoutLaunch` `SubagentEnd` with a declared transcript path; the shared final capture extracts files from that transcript and completes the record exactly once. Two small generic changes make this work: a `DeferredCompletion` flag on `SubagentStart` that records an in-flight marker, and decoupling "skip the child transcript" from `CompletionWithoutLaunch`.

**Tech Stack:** Go 1.26 (`cmd/entire/cli`), TypeScript plugin embedded via `go:embed`, `testify`, integration tests (`//go:build integration`), e2e harness.

**Spec:** `docs/superpowers/specs/2026-09-11-opencode-native-subagent-tracking-design.md`. Contract evidence: `cmd/entire/cli/agent/opencode/AGENT.md`.

**Conventions to honour** (from `CLAUDE.md`): `t.Parallel()` unless the test uses `t.Chdir`; git in tests only via `testutil`; never `git.PlainOpen`; hooks stay silent to the user and log via `logging`; no user content in logs; `mise run fmt && mise run lint` before every commit; `mise run check` before the final commit. Every commit message ends with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` (the `git commit -m` lines below omit it for brevity; append it). Do not run paid e2e tests without explicit authorization.

---

## File Structure

| File | Change | Responsibility |
|------|--------|----------------|
| `cmd/entire/cli/agent/event.go` | modify | add `DeferredCompletion` to `Event` |
| `cmd/entire/cli/lifecycle.go` | modify | `handleLifecycleSubagentStart` records in-flight marker for `DeferredCompletion`; `handleSubagentStopFinal` keys `eventFilesOnly` on `SubagentTranscriptUnavailable` |
| `cmd/entire/cli/lifecycle_test.go` | modify | two tests for the generic changes |
| `cmd/entire/cli/agent/opencode/types.go` | modify | `subagentStartRaw`, `subagentStopRaw` |
| `cmd/entire/cli/agent/opencode/lifecycle.go` | modify | hook names, `parseSubagentStart`, `parseSubagentStop` |
| `cmd/entire/cli/agent/opencode/lifecycle_test.go` | modify | parse tests, `TestHookNames` |
| `cmd/entire/cli/agent/opencode/entire_plugin.ts` | modify | child suppression, `subagent-start`, `subagent-stop`, injection guard |
| `cmd/entire/cli/agent/opencode/hooks_test.go` | modify | rendered-plugin content tests |
| `.opencode/plugins/entire.ts` | modify | the repo's committed dogfood copy of the plugin; `TestCommittedDogfoodPluginIsCurrent` pins it byte-identical to the template, so re-render it after every template edit |
| `cmd/entire/cli/integration_test/hooks.go` | modify | `SimulateOpenCodeSubagentStart/Stop` helpers |
| `cmd/entire/cli/integration_test/opencode_subagent_test.go` | create | end-to-end hook → record → condensation test |
| `e2e/tests/subagent_commit_flow_test.go` | modify | task-record assertion for opencode |
| `docs/architecture/agent-guide.md` | modify | OpenCode column in the event table |
| `docs/architecture/sessions-and-checkpoints.md` | modify | OpenCode producer bullet |
| `cmd/entire/cli/agent/opencode/AGENT.md` | modify | mark the current-behaviour section as historical |

---

## Chunk 1: Generic lifecycle changes

### Task 1: `Event.DeferredCompletion`

**Files:**
- Modify: `cmd/entire/cli/agent/event.go` (after `CompletionWithoutLaunch`, ~line 138)

- [ ] **Step 1: Add the field**

```go
	// DeferredCompletion marks a SubagentStart whose completion arrives as a
	// separate Final SubagentEnd rather than at the launch hook. The framework
	// records an in-flight task record (so the task is listed as running and
	// swept at SessionEnd if the agent dies) and skips the worktree pre-task
	// baseline, which the analyzer-only final capture never reads. OpenCode sets
	// it: its task part binds the tool call to the child session ID before the
	// child does any work, and completion comes from tool.execute.after.
	DeferredCompletion bool
```

- [ ] **Step 2: Build**

Run: `go build ./cmd/entire/...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add cmd/entire/cli/agent/event.go
git commit -m "agent: add Event.DeferredCompletion for launch-then-final subagent shapes"
```

### Task 2: Record an in-flight marker on a deferred start

**Files:**
- Modify: `cmd/entire/cli/lifecycle.go:1416-1462` (`handleLifecycleSubagentStart`), add `recordDeferredTaskLaunch` next to `recordInFlightTaskLaunch` (~line 1542)
- Test: `cmd/entire/cli/lifecycle_test.go` (near `TestHandleLifecycleSubagentEnd_LaunchDispatch`, ~line 3418)

- [ ] **Step 1: Write the failing test**

Uses the existing helpers `setupSubagentEndTestRepo`, `saveInFlightSession`, `newMockAgent` (see `lifecycle_test.go:3280-3375`).

```go
// TestHandleLifecycleSubagentStart_DeferredCompletion_RecordsInFlightMarker pins
// the OpenCode launch shape: the start hook knows the tool call, the child ID and
// the labels, and completion arrives later as a Final SubagentEnd. The start
// must leave a live record (so `checkpoint list --pending` shows it and the
// SessionEnd sweep can complete it) and must not disturb a record a racing
// Final already completed.
func TestHandleLifecycleSubagentStart_DeferredCompletion_RecordsInFlightMarker(t *testing.T) {
	// NOT parallel: setupSubagentEndTestRepo uses t.Chdir.
	_, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	const sessionID = "opencode-deferred-start"
	saveInFlightSession(ctx, t, sessionID, headHash)

	start := &agent.Event{
		Type:               agent.SubagentStart,
		SessionID:          sessionID,
		ToolUseID:          "call_red",
		SubagentID:         "ses_child_red",
		SubagentType:       "general",
		TaskDescription:    "Create docs/red.md",
		DeferredCompletion: true,
		Timestamp:          time.Now(),
	}
	require.NoError(t, handleLifecycleSubagentStart(ctx, newMockAgent(), start))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	rec := state.FindTaskRecord("call_red")
	require.NotNil(t, rec, "deferred start must record an in-flight marker")
	assert.True(t, rec.CompletedAt.IsZero())
	assert.Equal(t, "ses_child_red", rec.AgentID)
	assert.Equal(t, "general", rec.SubagentType)
	assert.Equal(t, "Create docs/red.md", rec.TaskDescription)

	// A late duplicate start after completion must not reopen the record.
	require.NoError(t, strategy.MutateSessionState(ctx, sessionID, func(s *strategy.SessionState) error {
		require.True(t, s.CompleteTaskRecord("call_red", time.Now()))
		return nil
	}))
	require.NoError(t, handleLifecycleSubagentStart(ctx, newMockAgent(), start))
	state, err = strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, state.TaskRecords, 1)
	assert.False(t, state.TaskRecords[0].CompletedAt.IsZero(), "duplicate start must not overwrite a completed record")
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/entire/cli/ -run TestHandleLifecycleSubagentStart_DeferredCompletion_RecordsInFlightMarker -count=1`
Expected: FAIL at `require.NotNil(t, rec, ...)` (no record is written today; the start only captures pre-task state).

- [ ] **Step 3: Implement**

In `handleLifecycleSubagentStart`, after the Codex branch and before `CapturePreTaskState`:

```go
	if event.DeferredCompletion {
		// The launch already names the tool call, the child and the labels;
		// completion arrives as a separate Final SubagentEnd whose capture is
		// analyzer-only, so the worktree baseline would never be read.
		return recordDeferredTaskLaunch(logCtx, event)
	}
```

Refactor `recordInFlightTaskLaunch` (~line 1542) so both launch shapes share one
body — a near-copy would trip the `dupl` linter at CI's threshold 75:

```go
// recordInFlightTaskLaunch handles a background Task launch (Claude Code's
// run_in_background post-task stub): the record is replaced wholesale, which
// is what a retried launch event wants.
func recordInFlightTaskLaunch(logCtx context.Context, event *agent.Event) error {
	logging.Debug(logCtx, "background subagent launch detected; deferring capture to subagent-stop",
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
		slog.String("agent_id", event.SubagentID),
	)
	return recordTaskLaunchMarker(logCtx, event, func(state *strategy.SessionState, rec session.TaskRecord) {
		state.AddTaskRecord(rec)
	})
}

// recordDeferredTaskLaunch handles a start hook that already carries full
// identity and whose completion is a later Final event
// (Event.DeferredCompletion). EnsureTaskRecord rather than AddTaskRecord: a
// start replayed after the Final completed the record must enrich, never
// overwrite, so a completed record is never reopened.
func recordDeferredTaskLaunch(logCtx context.Context, event *agent.Event) error {
	return recordTaskLaunchMarker(logCtx, event, func(state *strategy.SessionState, rec session.TaskRecord) {
		state.EnsureTaskRecord(rec)
	})
}

// recordTaskLaunchMarker writes the launch-time task record through mutate.
// Tolerates strategy.ErrStateNotFound the way the completion producers do: a
// launch can arrive before session state exists, and the Final capture
// creates the record itself in that case.
func recordTaskLaunchMarker(logCtx context.Context, event *agent.Event, mutate func(*strategy.SessionState, session.TaskRecord)) error {
	rec := session.TaskRecord{
		ToolUseID:       event.ToolUseID,
		AgentID:         event.SubagentID,
		StartedAt:       time.Now(),
		SubagentType:    event.SubagentType,
		TaskDescription: event.TaskDescription,
	}
	mutErr := strategy.MutateSessionState(logCtx, event.SessionID, func(state *strategy.SessionState) error {
		mutate(state, rec)
		return nil
	})
	switch {
	case errors.Is(mutErr, strategy.ErrStateNotFound):
		logging.Info(logCtx, "no session state to record task launch marker on",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID))
	case mutErr != nil:
		logging.Warn(logCtx, "failed to record task launch marker",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("error", mutErr.Error()))
	}
	return nil
}
```

`session.EnsureTaskRecord` (`session/state.go:531`) adds when absent and only
fills empty fields when present, without touching `CompletedAt`, so the
"duplicate start" half of the test holds without further guards.

- [ ] **Step 4: Run the test**

Run: `go test ./cmd/entire/cli/ -run 'TestHandleLifecycleSubagentStart_DeferredCompletion_RecordsInFlightMarker|TestHandleLifecycleSubagentEnd' -count=1`
Expected: PASS, including every pre-existing `TestHandleLifecycleSubagentEnd_*`.

- [ ] **Step 5: Format and lint**

Run: `mise run fmt && mise run lint`
Expected: clean. If `dupl` still flags the two wrappers, they are small enough that the shared helper is the fix, not a `//nolint`.

- [ ] **Step 6: Commit**

```bash
git add cmd/entire/cli/lifecycle.go cmd/entire/cli/lifecycle_test.go
git commit -m "lifecycle: record an in-flight task marker for deferred-completion starts"
```

### Task 3: Decouple "skip the child transcript" from `CompletionWithoutLaunch`

**Files:**
- Modify: `cmd/entire/cli/lifecycle.go:1676` (`eventFilesOnly: event.CompletionWithoutLaunch`)
- Test: `cmd/entire/cli/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Model on `TestHandleLifecycleSubagentEnd_SubagentStop_UnresolvableTranscript_SkipsParentAttribution`
(~line 3979). The analyzer lives on the wrapper `mockAnalyzerAgent`
(`lifecycle_test.go:108-135`), not on `mockLifecycleAgent`, and its
`ExtractModifiedFilesFromOffset` currently discards its path argument — extend
it (test-only) to record the last path it was asked to scan, e.g. a
`scannedPath string` field set in the method, so the test can prove the child
transcript was scanned rather than `SessionRef`.

Use a repo-relative file name. `completeSubagentTaskRecord` normalizes against
git's realpath of the worktree, and on macOS `t.TempDir()` is under
`/var/folders` while git reports `/private/var/...`, so an absolute path built
from `repoDir` is dropped by `paths.ToRelativePath` (see the trap documented at
`lifecycle_test.go:1109-1121`). A relative path passes through unchanged, and
`filterToUncommittedFiles` keeps any path absent from HEAD whether or not it
exists on disk.

```go
// TestHandleLifecycleSubagentEnd_CompletionWithoutLaunch_UsesDeclaredTranscript
// pins the OpenCode stop shape: no launch marker is required, but a child
// transcript IS declared, so the final capture must extract the child's files
// from it instead of treating the completion as event-files-only (Copilot's
// shape, which additionally sets SubagentTranscriptUnavailable).
func TestHandleLifecycleSubagentEnd_CompletionWithoutLaunch_UsesDeclaredTranscript(t *testing.T) {
	// NOT parallel: setupSubagentEndTestRepo uses t.Chdir.
	_, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	const sessionID = "opencode-completion-declared"
	saveInFlightSession(ctx, t, sessionID, headHash)

	mainPath, childPath := writeSubagentTranscripts(t, "ses_child_red")

	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: newMockAgent(),
		analyzerFiles:      []string{"docs_red.md"},
	}

	event := finalSubagentEvent(sessionID, "call_red", "ses_child_red")
	event.SessionRef = mainPath
	event.SubagentTranscriptPath = childPath
	event.CompletionWithoutLaunch = true
	event.SubagentType = "general"
	event.TokenUsage = &agent.TokenUsage{InputTokens: 12, OutputTokens: 3}

	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, event))

	assert.Equal(t, childPath, ag.scannedPath, "files must be extracted from the declared child transcript, not the parent")

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	rec := state.FindTaskRecord("call_red")
	require.NotNil(t, rec, "a completion learned at stop time creates the record when no launch marker exists")
	assert.False(t, rec.CompletedAt.IsZero())
	assert.Equal(t, []string{"docs_red.md"}, rec.Files)
	assert.Equal(t, childPath, rec.DeclaredTranscriptPath)
	assert.False(t, rec.TranscriptUnavailable)
	require.NotNil(t, rec.TokenUsage)
	assert.Equal(t, 12, rec.TokenUsage.InputTokens)
	assert.Contains(t, state.FilesTouched, "docs_red.md")
}
```

Check the exact `agent.TokenUsage` field names before finalizing.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/entire/cli/ -run TestHandleLifecycleSubagentEnd_CompletionWithoutLaunch_UsesDeclaredTranscript -count=1`
Expected: FAIL on `ag.scannedPath` (empty), `rec.Files` (empty) and `rec.DeclaredTranscriptPath` (empty), because `eventFilesOnly` is currently true for every `CompletionWithoutLaunch` event so the declared transcript is never resolved or scanned.

- [ ] **Step 3: Implement**

In `handleSubagentStopFinal` change the capture options:

```go
	captureErr := completeSubagentTaskRecord(logCtx, ag, event, subagentCaptureOptions{
		bypassNoChangesSkip: true,
		analyzerFilesOnly:   true,
		// Only an agent with no standalone child transcript (Copilot CLI) has
		// nothing to scan; a completion learned at stop time that DOES declare a
		// transcript (OpenCode) still attributes files from it.
		eventFilesOnly: event.SubagentTranscriptUnavailable,
	})
```

Update the doc comment on `subagentCaptureOptions.eventFilesOnly` to say it is keyed on transcript unavailability, and fix the stale comment above this call (~line 1667, "reaching this point means a live marker WAS found") — `CompletionWithoutLaunch` events reach it with no marker.

- [ ] **Step 4: Run the tests**

Run: `go test ./cmd/entire/cli/ -run 'TestHandleLifecycleSubagentEnd' -count=1`
Expected: PASS, including `TestHandleLifecycleSubagentEnd_CorrelatedCompletionCreatesDistinctRecord` (Copilot sets both flags, so it is unaffected).

- [ ] **Step 5: Format and lint**

Run: `mise run fmt && mise run lint`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/entire/cli/lifecycle.go cmd/entire/cli/lifecycle_test.go
git commit -m "lifecycle: key event-files-only capture on transcript unavailability"
```

---

## Chunk 2: OpenCode Go adapter

### Task 4: Payload types and hook names

**Files:**
- Modify: `cmd/entire/cli/agent/opencode/types.go` (after `turnEndRaw`)
- Modify: `cmd/entire/cli/agent/opencode/lifecycle.go:50-66` (constants, `HookNames`)
- Test: `cmd/entire/cli/agent/opencode/lifecycle_test.go:228` (`TestHookNames`)

- [ ] **Step 1: Update `TestHookNames`**

Add `HookNameSubagentStart, HookNameSubagentStop` to `expected`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/entire/cli/agent/opencode/ -run TestHookNames -count=1`
Expected: compile error (undefined constants).

- [ ] **Step 3: Implement**

`lifecycle.go`:

```go
const (
	HookNameSessionStart  = "session-start"
	HookNameSessionEnd    = "session-end"
	HookNameTurnStart     = "turn-start"
	HookNameTurnEnd       = "turn-end"
	HookNameCompaction    = "compaction"
	HookNameSubagentStart = "subagent-start"
	HookNameSubagentStop  = "subagent-stop"
)
```

and append both to the `HookNames()` slice. Update the comment at `hooks.go:145` that says `HookNames()` returns 5 hooks (it now returns 7 while `GetSupportedHooks` still returns 4).

`types.go`:

```go
// subagentStartRaw is the payload the plugin sends when the parent's `task`
// tool part first carries the child session ID (state.metadata.sessionId).
type subagentStartRaw struct {
	SessionID       string `json:"session_id"`      // parent
	ToolUseID       string `json:"tool_use_id"`     // task part callID
	SubagentID      string `json:"subagent_id"`     // child session ID
	SubagentType    string `json:"subagent_type"`   // task args.subagent_type
	TaskDescription string `json:"task_description"` // task args.description
}

// subagentStopRaw is the payload the plugin sends from tool.execute.after for
// the `task` tool. Same identity fields as subagentStartRaw plus the model the
// parent was using.
type subagentStopRaw struct {
	SessionID       string `json:"session_id"`
	ToolUseID       string `json:"tool_use_id"`
	SubagentID      string `json:"subagent_id"`
	SubagentType    string `json:"subagent_type"`
	TaskDescription string `json:"task_description"`
	Model           string `json:"model"`
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./cmd/entire/cli/agent/opencode/ -run TestHookNames -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/entire/cli/agent/opencode/types.go cmd/entire/cli/agent/opencode/lifecycle.go cmd/entire/cli/agent/opencode/lifecycle_test.go
git commit -m "opencode: declare subagent-start and subagent-stop hook verbs"
```

### Task 5: Parse `subagent-start`

**Files:**
- Modify: `cmd/entire/cli/agent/opencode/lifecycle.go` (`ParseHookEvent` switch)
- Test: `cmd/entire/cli/agent/opencode/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestParseHookEvent_SubagentStart(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	input := `{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"ses_child","subagent_type":"general","task_description":"Create docs/red.md"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStart, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	assert.Equal(t, agent.SubagentStart, event.Type)
	assert.Equal(t, "ses_parent", event.SessionID)
	assert.True(t, strings.HasSuffix(event.SessionRef, filepath.Join(paths.EntireTmpDir, "ses_parent.json")), event.SessionRef)
	assert.Equal(t, "call_red", event.ToolUseID)
	assert.Equal(t, "ses_child", event.SubagentID)
	assert.Equal(t, "general", event.SubagentType)
	assert.Equal(t, "Create docs/red.md", event.TaskDescription)
	assert.True(t, event.DeferredCompletion, "OpenCode completes from tool.execute.after, so the start must record a marker")
}

func TestParseHookEvent_SubagentStart_RejectsUnsafeIDs(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	for name, input := range map[string]string{
		"child traversal": `{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"../etc"}`,
		"missing tool id": `{"session_id":"ses_parent","subagent_id":"ses_child"}`,
		"missing child":   `{"session_id":"ses_parent","tool_use_id":"call_red"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStart, strings.NewReader(input))
			require.Error(t, err)
		})
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/entire/cli/agent/opencode/ -run 'TestParseHookEvent_SubagentStart' -count=1`
Expected: FAIL — `ParseHookEvent` returns `nil, nil` for the unknown verb.

- [ ] **Step 3: Implement**

Add a case to `ParseHookEvent`:

```go
	case HookNameSubagentStart:
		raw, err := agent.ReadAndParseHookInput[subagentStartRaw](stdin)
		if err != nil {
			return nil, err
		}
		if err := validateSubagentIdentity(raw.SessionID, raw.ToolUseID, raw.SubagentID); err != nil {
			return nil, err
		}
		parentRef, err := sessionTranscriptPath(ctx, raw.SessionID)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:               agent.SubagentStart,
			SessionID:          raw.SessionID,
			SessionRef:         parentRef,
			ToolUseID:          raw.ToolUseID,
			SubagentID:         raw.SubagentID,
			SubagentType:       raw.SubagentType,
			TaskDescription:    raw.TaskDescription,
			DeferredCompletion: true,
			Timestamp:          time.Now(),
		}, nil
```

and the helper:

```go
// validateSubagentIdentity checks the three IDs an OpenCode subagent payload
// must carry. The child ID becomes an `opencode export` argument and a file
// name under .entire/tmp; the tool-use ID becomes tasks/<id>/ in the checkpoint.
func validateSubagentIdentity(parentID, toolUseID, childID string) error {
	if err := validation.ValidateSessionID(parentID); err != nil {
		return fmt.Errorf("invalid parent session ID: %w", err)
	}
	if toolUseID == "" {
		return errors.New("opencode subagent payload missing tool_use_id")
	}
	if err := validation.ValidateToolUseID(toolUseID); err != nil {
		return fmt.Errorf("invalid tool_use_id: %w", err)
	}
	if err := validation.ValidateSessionID(childID); err != nil {
		return fmt.Errorf("invalid subagent session ID: %w", err)
	}
	return nil
}
```

`validation.ValidateToolUseID` (`validation/validators.go:59`, regex `^[a-zA-Z0-9_-]+$`) accepts Gemini's 16-char call IDs and Anthropic's `toolu_…`, and also accepts the empty string — which is why the explicit empty check above is load-bearing; say so in the helper's comment. `lifecycle.go` does not yet import `errors`; add it.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/entire/cli/agent/opencode/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/entire/cli/agent/opencode/lifecycle.go cmd/entire/cli/agent/opencode/lifecycle_test.go
git commit -m "opencode: parse subagent-start into a deferred-completion SubagentStart"
```

### Task 6: Parse `subagent-stop` (export the child, tokens, declared path)

**Files:**
- Modify: `cmd/entire/cli/agent/opencode/lifecycle.go`
- Test: `cmd/entire/cli/agent/opencode/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

These stub `runOpenCodeExportToFileFn` the way `TestFetchAndCacheExport_WritesAndValidatesExportFile` does (`t.Chdir` + `paths.ClearWorktreeRootCache`, so NOT parallel).

```go
const childExportFixture = `{"info":{"id":"ses_child","parentID":"ses_parent","agent":"general"},"messages":[` +
	`{"info":{"id":"m1","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"make red"}]},` +
	`{"info":{"id":"m2","role":"assistant","time":{"created":2},"tokens":{"input":100,"output":20,"reasoning":0,"cache":{"read":5,"write":0}}},` +
	`"parts":[{"type":"tool","tool":"write","callID":"w1","state":{"status":"completed","input":{"filePath":"/repo/docs/red.md"},"metadata":{"files":[{"filePath":"/repo/docs/red.md"}]}}}]}]}`

func TestParseHookEvent_SubagentStop_ExportsChildAndDeclaresTranscript(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	original := runOpenCodeExportToFileFn
	var exported []string
	runOpenCodeExportToFileFn = func(_ context.Context, root *os.Root, sessionID, outputName string) error {
		exported = append(exported, sessionID)
		return root.WriteFile(outputName, []byte(childExportFixture), 0o600)
	}
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	ag := &OpenCodeAgent{}
	input := `{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"ses_child","subagent_type":"general","task_description":"Create docs/red.md","model":"gemini-2.5-flash"}`
	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)

	assert.Equal(t, []string{"ses_child"}, exported, "stop must export exactly the child")
	assert.Equal(t, agent.SubagentEnd, event.Type)
	assert.True(t, event.Final)
	assert.True(t, event.CompletionWithoutLaunch)
	assert.False(t, event.SubagentTranscriptUnavailable)
	assert.Equal(t, "ses_parent", event.SessionID)
	assert.Equal(t, "call_red", event.ToolUseID)
	assert.Equal(t, "ses_child", event.SubagentID)
	assert.Equal(t, "general", event.SubagentType)
	assert.Equal(t, "Create docs/red.md", event.TaskDescription)
	assert.Equal(t, "gemini-2.5-flash", event.Model)
	assert.True(t, strings.HasSuffix(event.SubagentTranscriptPath, filepath.Join(paths.EntireTmpDir, "ses_child.json")), event.SubagentTranscriptPath)
	assert.FileExists(t, event.SubagentTranscriptPath)
	require.NotNil(t, event.TokenUsage)
	assert.Equal(t, 100, event.TokenUsage.InputTokens)
	assert.Equal(t, 20, event.TokenUsage.OutputTokens)
	assert.Empty(t, event.ModifiedFiles, "files come from the declared transcript at capture time, not the event")
}

func TestParseHookEvent_SubagentStop_ExportFailureStillCompletes(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(context.Context, *os.Root, string, string) error {
		return errors.New("opencode not reachable")
	}
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	ag := &OpenCodeAgent{}
	input := `{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"ses_child"}`
	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(input))
	require.NoError(t, err, "a failed export must degrade the record, not drop the completion")
	require.NotNil(t, event)
	assert.True(t, event.Final)
	assert.True(t, event.CompletionWithoutLaunch)
	assert.True(t, event.SubagentTranscriptUnavailable)
	assert.Empty(t, event.SubagentTranscriptPath)
	assert.Nil(t, event.TokenUsage)
}

func TestParseHookEvent_SubagentStop_RejectsUnsafeIDs(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop,
		strings.NewReader(`{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"../../evil"}`))
	require.Error(t, err)
}
```

`agent.TokenUsage` fields are `InputTokens`, `OutputTokens`, `CacheReadTokens`, `CacheCreationTokens`, `APICallCount`; this fixture yields input 100, output 20, cache read 5, one API call. `lifecycle_test.go` currently imports only `testify/require`; add `github.com/stretchr/testify/assert`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/entire/cli/agent/opencode/ -run 'TestParseHookEvent_SubagentStop' -count=1`
Expected: FAIL (nil event for unknown verb).

- [ ] **Step 3: Implement**

Add to `ParseHookEvent`:

```go
	case HookNameSubagentStop:
		raw, err := agent.ReadAndParseHookInput[subagentStopRaw](stdin)
		if err != nil {
			return nil, err
		}
		if err := validateSubagentIdentity(raw.SessionID, raw.ToolUseID, raw.SubagentID); err != nil {
			return nil, err
		}
		parentRef, err := sessionTranscriptPath(ctx, raw.SessionID)
		if err != nil {
			return nil, err
		}
		event := &agent.Event{
			Type:            agent.SubagentEnd,
			SessionID:       raw.SessionID,
			SessionRef:      parentRef,
			ToolUseID:       raw.ToolUseID,
			SubagentID:      raw.SubagentID,
			SubagentType:    raw.SubagentType,
			TaskDescription: raw.TaskDescription,
			Model:           raw.Model,
			Timestamp:       time.Now(),
			// tool.execute.after fires once, at true completion, and is the
			// first signal that can name both the tool call and the finished
			// child — so it is the authoritative final capture, and it must not
			// depend on the start having been seen (a plugin restarted
			// mid-task never saw it). AGENT.md's earlier "leave Final false"
			// suggestion predates the DeferredCompletion start; the spec
			// supersedes it.
			Final:                   true,
			CompletionWithoutLaunch: true,
		}
		a.attachSubagentTranscript(ctx, event)
		return event, nil
```

and:

```go
// attachSubagentTranscript exports the child session and declares it on the
// event with its exact token usage. On failure the event is left marked
// transcript-unavailable: a record that says so is more useful than no record,
// and the sweep has no other way to fetch a child.
func (a *OpenCodeAgent) attachSubagentTranscript(ctx context.Context, event *agent.Event) {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	path, err := a.fetchAndCacheExport(ctx, event.SubagentID)
	if err != nil {
		logging.Warn(logCtx, "opencode: could not export subagent transcript; completing task without it",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("subagent_id", event.SubagentID),
			slog.String("error", err.Error()))
		event.SubagentTranscriptUnavailable = true
		return
	}
	event.SubagentTranscriptPath = path
	data, err := a.ReadTranscript(path)
	if err != nil {
		logging.Warn(logCtx, "opencode: could not read exported subagent transcript for token usage",
			slog.String("subagent_id", event.SubagentID), slog.String("error", err.Error()))
		return
	}
	usage, err := a.CalculateTokenUsage(data, 0)
	if err != nil {
		logging.Warn(logCtx, "opencode: could not compute subagent token usage",
			slog.String("subagent_id", event.SubagentID), slog.String("error", err.Error()))
		return
	}
	event.TokenUsage = usage
}
```

`ReadTranscript` is at `opencode.go:62`; `CalculateTokenUsage` at `transcript.go:288`. Confirm their exact signatures before wiring.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/entire/cli/agent/opencode/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/entire/cli/agent/opencode/lifecycle.go cmd/entire/cli/agent/opencode/lifecycle_test.go
git commit -m "opencode: parse subagent-stop, export the child, and declare its transcript"
```

---

## Chunk 3: Plugin

### Task 7: Suppress child sessions in the plugin

**Files:**
- Modify: `cmd/entire/cli/agent/opencode/entire_plugin.ts`
- Test: `cmd/entire/cli/agent/opencode/hooks_test.go`

The rendered plugin is tested by string inspection (see `TestInstallHooks_SessionStartIsGuardedBySessionSwitch`, `hooks_test.go:81`). Keep the markers below verbatim so the tests can find them.

**Dogfood copy.** This repository commits its own rendered plugin at `.opencode/plugins/entire.ts`, and `TestCommittedDogfoodPluginIsCurrent` (`hooks_test.go:437`) requires it to be byte-identical to the embedded template. After every edit to `entire_plugin.ts` run:

```bash
cp cmd/entire/cli/agent/opencode/entire_plugin.ts .opencode/plugins/entire.ts
```

and include that file in the commit, or the package tests go red. `TestPlugin_SpawnsHooksUnderNode` also loads the rendered plugin under `node --experimental-strip-types`, so a TypeScript syntax error surfaces in the normal `go test` run.

- [ ] **Step 1: Write the failing test**

```go
func TestInstallHooks_ChildSessionsNeverFireLifecycleHooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".opencode", "plugins", "entire.ts"))
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}
	content := string(data)

	// The child set is learned from parentID and consulted before the event switch.
	learn := "if (info?.parentID && info?.id) childSessions.add(info.id)"
	guard := "if (eventSessionID && childSessions.has(eventSessionID)) return"
	sw := "switch (event.type) {"
	learnIdx, guardIdx, swIdx := strings.Index(content, learn), strings.Index(content, guard), strings.Index(content, sw)
	if learnIdx == -1 || guardIdx == -1 || swIdx == -1 {
		t.Fatalf("plugin missing child-session learn/guard/switch: %d %d %d", learnIdx, guardIdx, swIdx)
	}
	if !(learnIdx < guardIdx && guardIdx < swIdx) {
		t.Fatalf("child guard must run before the event switch: learn=%d guard=%d switch=%d", learnIdx, guardIdx, swIdx)
	}
	// The one-time context injection is for the user's session only.
	if !strings.Contains(content, "if (input?.sessionID && childSessions.has(input.sessionID)) return") {
		t.Fatal("system.transform must not spend the parent's injection on a child session")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/entire/cli/agent/opencode/ -run TestInstallHooks_ChildSessionsNeverFireLifecycleHooks -count=1`
Expected: FAIL (markers absent).

- [ ] **Step 3: Implement**

Near the other state declarations at the top of `EntirePlugin`:

```ts
  // Child (subagent) sessions, learned from `parentID` on session.created /
  // session.updated. OpenCode's task tool runs each subagent as a real session,
  // so without this set a child would register as the user's session: its
  // write would take the checkpoint and the parent would log "no files
  // modified". Children are reported to Entire only through subagent-start /
  // subagent-stop, fired from the PARENT's task part and tool hook.
  const childSessions = new Set<string>()
  // task callIDs already announced via subagent-start (the running part
  // update repeats).
  const announcedTasks = new Set<string>()
```

At the top of the `event` handler, before `switch (event.type) {`:

```ts
        const props = (event as any).properties
        const info = props?.info
        if (info?.parentID && info?.id) childSessions.add(info.id)
        const eventSessionID: string | undefined =
          props?.sessionID ?? info?.sessionID ?? info?.id ?? props?.part?.sessionID
        if (eventSessionID && childSessions.has(eventSessionID)) return
```

`session.created` carries only `properties.info` (no `properties.sessionID`), so the guard reaches the child through the `info?.id` fallback — which is why learning must run before the guard: the same event that teaches us the child is the first one we suppress.

In `experimental.chat.system.transform`, take the input and guard:

```ts
    "experimental.chat.system.transform": async (input: { sessionID?: string }, output: { system: string[] }) => {
      if (input?.sessionID && childSessions.has(input.sessionID)) return
      if (pendingInjection && Array.isArray(output.system)) {
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/entire/cli/agent/opencode/ -count=1`
Expected: PASS after re-rendering the dogfood copy (`cp cmd/entire/cli/agent/opencode/entire_plugin.ts .opencode/plugins/entire.ts`), including `TestCommittedDogfoodPluginIsCurrent`, `TestPlugin_SpawnsHooksUnderNode`, `TestInstallHooks_RewritesWhenContentDiffers` and `TestCheckHookConfig` (content-based freshness means an old plugin now reads as outdated — that is the intended upgrade path).

- [ ] **Step 5: Commit**

```bash
git add cmd/entire/cli/agent/opencode/entire_plugin.ts .opencode/plugins/entire.ts cmd/entire/cli/agent/opencode/hooks_test.go
git commit -m "opencode plugin: never forward child session lifecycle as the user's session"
```

### Task 8: Fire `subagent-start` and `subagent-stop` from the plugin

**Files:**
- Modify: `cmd/entire/cli/agent/opencode/entire_plugin.ts`
- Test: `cmd/entire/cli/agent/opencode/hooks_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestInstallHooks_SubagentHooksFireFromParentTaskSignals(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".opencode", "plugins", "entire.ts"))
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		// start: parent task part, running, with the child ID bound, once per callID
		`part.type === "tool" && part.tool === "task"`,
		`part.state?.status === "running"`,
		`part.state?.metadata?.sessionId`,
		`announcedTasks.has(part.callID)`,
		`callHookSync("subagent-start", {`,
		// stop: tool.execute.after for the task tool, foreground only, synchronous
		`"tool.execute.after": async (input, output) => {`,
		`if (input.tool !== "task") return`,
		`if (output?.metadata?.background === true) return`,
		`callHookSync("subagent-stop", {`,
		`subagent_id: childID`,
		`tool_use_id: input.callID`,
		// a child's own task call (nested subagents, off by default) is not ours
		`if (childSessions.has(input.sessionID)) return`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plugin missing %q", want)
		}
	}
	if strings.Contains(content, `callHook("subagent-stop"`) {
		t.Error("subagent-stop must be synchronous: opencode run exits on the parent's idle right after")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/entire/cli/agent/opencode/ -run TestInstallHooks_SubagentHooksFireFromParentTaskSignals -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

In the `message.part.updated` case, after the existing turn-start logic (the parent's part update is not a child event, so it reaches here):

```ts
            // Subagent launch: the parent's task part is the first signal that
            // binds the tool call to the child session (state.metadata.sessionId).
            // tool.execute.before fires earlier but has no child ID yet, and
            // session.created for the child can interleave with a sibling's, so
            // neither is a safe join. Announce once per callID; the running
            // update repeats.
            if (part.type === "tool" && part.tool === "task" && part.callID &&
                part.state?.status === "running" && part.state?.metadata?.sessionId &&
                !announcedTasks.has(part.callID)) {
              announcedTasks.add(part.callID)
              const sessionID = part.sessionID ?? currentSessionID
              if (sessionID) {
                callHookSync("subagent-start", {
                  session_id: sessionID,
                  tool_use_id: part.callID,
                  subagent_id: part.state.metadata.sessionId,
                  subagent_type: part.state?.input?.subagent_type ?? "",
                  task_description: part.state?.input?.description ?? "",
                })
              }
            }
```

Add a new hook next to `experimental.chat.system.transform` in the returned object:

```ts
    // Subagent completion. tool.execute.after for the task tool fires once, at
    // true completion, with the child session ID in output.metadata. Background
    // tasks (experimental) return immediately with metadata.background and are
    // not tracked. Synchronous: `opencode run` exits on the parent's idle right
    // after this, and an async hook would be killed before it finished.
    "tool.execute.after": async (input, output) => {
      if (input.tool !== "task") return
      // A child's own task call (subagent_depth > 1) belongs to a session we do
      // not track; report only the user's session's tasks.
      if (childSessions.has(input.sessionID)) return
      if (output?.metadata?.background === true) return
      const childID = output?.metadata?.sessionId
      if (!childID) return
      callHookSync("subagent-stop", {
        session_id: input.sessionID,
        tool_use_id: input.callID,
        subagent_id: childID,
        subagent_type: input.args?.subagent_type ?? "",
        task_description: input.args?.description ?? "",
        model: currentModel ?? "",
      })
    },
```

Do not annotate the handler parameters: the test marker requires the bare `async (input, output) => {` form, and the `Plugin` type in `@opencode-ai/plugin` 1.18.30 already types `tool.execute.after` with `args: any` and `metadata: any`, so `input.args` and `output.metadata` need no casts.

Clear `childSessions` and `announcedTasks` alongside the other resets in `session.deleted` and `server.instance.disposed` (not in `resetSessionTracking`, which runs per parent session switch while children may still be finishing). Correctness does not depend on it — call IDs are unique per parent — but it keeps the sets bounded in a long TUI session.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/entire/cli/agent/opencode/ -count=1`
Expected: PASS.

- [ ] **Step 5: Type-check the plugin against the real SDK types**

`go test` already catches syntax errors (`TestPlugin_SpawnsHooksUnderNode`). For a real type check, run `tsc` from a directory whose `node_modules` holds `@opencode-ai/plugin` at the running version — `~/.config/opencode/` has one when its `package.json` pins the installed OpenCode version:

```bash
cp cmd/entire/cli/agent/opencode/entire_plugin.ts ~/.config/opencode/entire_plugin_check.ts
(cd ~/.config/opencode && npx --yes tsc --noEmit --skipLibCheck --target es2022 --module esnext --moduleResolution bundler entire_plugin_check.ts)
rm ~/.config/opencode/entire_plugin_check.ts
```

Expected: no errors. If `~/.config/opencode/node_modules` is absent, skip this step and rely on the Node load test plus Task 12's live probe.

- [ ] **Step 6: Commit**

```bash
cp cmd/entire/cli/agent/opencode/entire_plugin.ts .opencode/plugins/entire.ts
git add cmd/entire/cli/agent/opencode/entire_plugin.ts .opencode/plugins/entire.ts cmd/entire/cli/agent/opencode/hooks_test.go
git commit -m "opencode plugin: report task launches and completions as subagent hooks"
```

---

## Chunk 4: Integration, e2e, docs, verification

### Task 9: Integration test — hook flow to condensed task record

**Files:**
- Modify: `cmd/entire/cli/integration_test/hooks.go` (after `SimulateOpenCodeTurnEnd`, ~line 1054, plus `TestEnv` wrappers ~line 1197)
- Create: `cmd/entire/cli/integration_test/opencode_subagent_test.go`

- [ ] **Step 1: Add simulate helpers**

```go
// SimulateOpenCodeSubagentStart simulates the subagent-start hook: the parent's
// task part bound callID to the child session.
func (r *OpenCodeHookRunner) SimulateOpenCodeSubagentStart(parentID, toolUseID, childID, subagentType, description string) error {
	r.T.Helper()
	return r.runOpenCodeHookWithInput("subagent-start", map[string]string{
		"session_id":       parentID,
		"tool_use_id":      toolUseID,
		"subagent_id":      childID,
		"subagent_type":    subagentType,
		"task_description": description,
	})
}

// SimulateOpenCodeSubagentStop simulates the subagent-stop hook. The Go handler
// exports the child with `opencode export`; under ENTIRE_TEST_OPENCODE_MOCK_EXPORT
// it reads .entire/tmp/<childID>.json instead, so callers copy the child's
// transcript there first with env.CopyTranscriptToEntireTmp (the same helper
// mid-turn tests use for the parent). Omitting that copy exercises the
// export-failure path.
func (r *OpenCodeHookRunner) SimulateOpenCodeSubagentStop(parentID, toolUseID, childID, subagentType, description string) error {
	r.T.Helper()
	return r.runOpenCodeHookWithInput("subagent-stop", map[string]string{
		"session_id":       parentID,
		"tool_use_id":      toolUseID,
		"subagent_id":      childID,
		"subagent_type":    subagentType,
		"task_description": description,
		"model":            "test-model",
	})
}
```

Do not add a third copy of the copy-to-`.entire/tmp` block; `TestEnv.CopyTranscriptToEntireTmp` (`hooks.go:1206`) already exists and `dupl` would flag it.

Add matching `TestEnv` wrapper methods beside `SimulateOpenCodeSessionEnd` (~line 1197), following the existing one-line delegation pattern.

- [ ] **Step 2: Write the failing test**

```go
//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// TestOpenCodeSubagentTaskRecord drives the OpenCode subagent contract through
// the real hook binary: the parent turn starts, a child session is announced
// and completed, and the commit condenses the parent with the child's work as
// a task record — never as a session of its own.
func TestOpenCodeSubagentTaskRecord(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	parent := env.NewOpenCodeSession()
	child := env.NewOpenCodeSession() // stands in for the task tool's child session
	const toolUseID = "call_red_1"

	require.NoError(t, env.SimulateOpenCodeSessionStart(parent.ID, parent.TranscriptPath))
	require.NoError(t, env.SimulateOpenCodeTurnStart(parent.ID, parent.TranscriptPath, "use a subagent to create docs/red.md"))

	require.NoError(t, env.SimulateOpenCodeSubagentStart(parent.ID, toolUseID, child.ID, "general", "Create docs/red.md"))
	state, err := env.GetSessionState(parent.ID)
	require.NoError(t, err)
	live := state.FindTaskRecord(toolUseID)
	require.NotNil(t, live, "subagent-start must leave an in-flight record on the parent")
	require.True(t, live.CompletedAt.IsZero())
	require.Equal(t, child.ID, live.AgentID)

	// The child does the work: file on disk plus its own export naming the write.
	env.WriteFile("docs/red.md", "Red is a warm colour.\n")
	childTranscript := child.CreateOpenCodeTranscript("Create docs/red.md", []FileChange{
		{Path: "docs/red.md", Content: "Red is a warm colour.\n"},
	})
	env.CopyTranscriptToEntireTmp(child.ID, childTranscript)
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, toolUseID, child.ID, "general", "Create docs/red.md"))

	state, err = env.GetSessionState(parent.ID)
	require.NoError(t, err)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero(), "subagent-stop must complete the record")
	require.Equal(t, []string{"docs/red.md"}, rec.Files, "files must come from the child's own export")
	require.Contains(t, state.FilesTouched, "docs/red.md")
	require.NotEmpty(t, rec.DeclaredTranscriptPath)
	require.False(t, rec.TranscriptUnavailable)
	require.NotNil(t, rec.TokenUsage, "child tokens are exact and must be recorded")

	// No child session state may exist: the child is a task, not a session.
	// GetSessionState returns (nil, nil) for a missing state file.
	childState, err := env.GetSessionState(child.ID)
	require.NoError(t, err)
	require.Nil(t, childState, "child session must not have its own Entire session state")

	// A stop whose child export cannot be fetched still completes the record,
	// marked transcript-unavailable (spec acceptance criterion). No copy to
	// .entire/tmp precedes this call, so the mock export fails.
	const orphanToolUseID = "call_orphan_2"
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, orphanToolUseID, "opencode-session-missing", "explore", "Look around"))
	state, err = env.GetSessionState(parent.ID)
	require.NoError(t, err)
	orphan := state.FindTaskRecord(orphanToolUseID)
	require.NotNil(t, orphan)
	require.False(t, orphan.CompletedAt.IsZero())
	require.True(t, orphan.TranscriptUnavailable)
	require.Empty(t, orphan.Files)

	// Parent turn ends with the child's file present, then the user commits.
	// GitCommitWithShadowHooks (TTY shape) is deliberate and matches
	// opencode_hooks_test.go; codex_subagent_test.go's ...AsAgent variant is
	// the agent-commit shape and is not what this test is about.
	parent.CreateOpenCodeTranscript("use a subagent to create docs/red.md", nil)
	require.NoError(t, env.SimulateOpenCodeTurnEnd(parent.ID, parent.TranscriptPath))
	env.GitCommitWithShadowHooks("Add red.md via subagent", "docs/red.md")

	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID)
	_, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, "task.json"))
	require.True(t, ok, "task.json must be materialized under the parent checkpoint's tasks/ subtree")
	stored, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, paths.AgentTranscriptFileName(child.ID)))
	require.True(t, ok, "the child's export must be materialized as the task transcript")
	require.Contains(t, stored, "docs/red.md")
}
```

`CheckpointTaskFilePath(checkpointID, toolUseID, fileName)`, `GetSessionState` (returns `(nil, nil)` when absent) and `CopyTranscriptToEntireTmp` already exist in the integration package.

- [ ] **Step 3: Run the test**

This test is written after the implementation (Chunks 1–3), so it is not a red/green step.

Run: `go test -tags integration ./cmd/entire/cli/integration_test/ -run TestOpenCodeSubagentTaskRecord -count=1`
Expected: PASS. A failure is a real defect in the adapter or lifecycle, not in the test; do not loosen the assertions. (`mise run test:integration` runs the whole integration lane; the direct `go test` form above is the unambiguous single-test command.)

- [ ] **Step 4: Commit**

```bash
git add cmd/entire/cli/integration_test/hooks.go cmd/entire/cli/integration_test/opencode_subagent_test.go
git commit -m "integration: cover OpenCode subagent hooks through condensation"
```

### Task 10: e2e assertion for opencode

**Files:**
- Modify: `e2e/tests/subagent_commit_flow_test.go:41-43`

- [ ] **Step 1: Extend the assertion**

```go
		switch s.Agent.Name() {
		case "copilot-cli", "opencode":
			testutil.AssertCheckpointHasTaskRecord(t, s.Dir, cpID)
		}
		if s.Agent.Name() == "opencode" {
			// The child must not be a session of its own: one OpenCode session,
			// the one the user drove, owns the checkpoint.
			assert.Len(t, meta.Sessions, 1, "OpenCode child sessions must be task records, not sessions")
		}
```

Place the `meta.Sessions` assertion after `meta` is read (it is read a few lines below the current copilot check; move the block or read `meta` earlier).

- [ ] **Step 2: Canary**

Run: `mise run test:e2e:canary TestSubagentCommitFlow`
Expected: PASS (Vogon does not hit the opencode-only branch).

- [ ] **Step 3: Commit**

```bash
git add e2e/tests/subagent_commit_flow_test.go
git commit -m "e2e: assert OpenCode subagent work lands as a task record on the parent"
```

- [ ] **Step 4: Paid e2e (ONLY with explicit user authorization)**

Machine-specific note (Peyton's laptop, 2026-09-11, not a repo fact): OpenCode's stored Anthropic key is rejected and the OpenAI login is a ChatGPT account with a usage cap; Google works. Elsewhere the default `anthropic/claude-haiku-4-5` is fine.

Run: `E2E_OPENCODE_MODEL=google/gemini-2.5-flash mise run test:e2e --agent opencode TestSubagentCommitFlow`
Expected: PASS with the task-record and single-session assertions.

### Task 11: Documentation

**Files:**
- Modify: `docs/architecture/agent-guide.md:444-445` (OpenCode column, currently `*(not used)*`)
- Modify: `docs/architecture/sessions-and-checkpoints.md` "Producers" list (~line 319)
- Modify: `cmd/entire/cli/agent/opencode/AGENT.md` ("Current Entire Behaviour" section)

- [ ] **Step 1: agent-guide event table**

OpenCode column: `SubagentStart` → `` `subagent-start` (plugin: parent task part `running` with `metadata.sessionId`; `DeferredCompletion`) ``; `SubagentEnd` → `` `subagent-stop` (plugin: `tool.execute.after` for `task`; `Final` + `CompletionWithoutLaunch`, child exported and declared) ``. In "Declaring a subagent transcript" (~line 58) add OpenCode to the list of agents that declare a path.

- [ ] **Step 2: sessions-and-checkpoints producers**

Add a bullet after the Factory Droid one:

```markdown
- **OpenCode task tool**: `subagent-start` (parent task part bound to the child
  session) records the in-flight marker via `DeferredCompletion`;
  `subagent-stop` (`tool.execute.after`) exports the child session with
  `opencode export`, declares it as the transcript, attaches exact tokens, and
  completes the record through the Final path with `CompletionWithoutLaunch`,
  so a start the plugin never saw still completes. Child sessions fire no
  lifecycle hooks of their own.
```

- [ ] **Step 3: AGENT.md**

Retitle `## Current Entire Behaviour (before this work)` to `## Behaviour Before Native Tracking` and open it with one sentence: "Fixed by the design in `docs/superpowers/specs/2026-09-11-opencode-native-subagent-tracking-design.md`; kept as the record of what the probe observed." Retitle `## Proposed Mapping` to `## Implemented Mapping`, replace its table's `(suppress)`/`(attach)` rows with the shipped behaviour (plugin suppresses children; `subagent-start` fires from the running task part with `DeferredCompletion`; `subagent-stop` from `tool.execute.after` with `Final` + `CompletionWithoutLaunch`), and delete the sentence recommending `Final` false. Keep the captured-payload and verification sections unchanged.

- [ ] **Step 4: Commit**

```bash
git add docs/architecture/agent-guide.md docs/architecture/sessions-and-checkpoints.md cmd/entire/cli/agent/opencode/AGENT.md
git commit -m "docs: OpenCode subagent tracking contract and mapping"
```

### Task 12: Live verification and repository gates

- [ ] **Step 1: Build a candidate binary and run the probe against it**

```bash
go build -o /tmp/entire-candidate ./cmd/entire
PROBE_KEEP=1 OPENCODE_MODEL=google/gemini-2.5-flash ENTIRE_BIN=/tmp/entire-candidate \
  scripts/test-opencode-subagent-integration.sh --run-cmd --scenario single
```

Expected in the "Entire's view of the run" section: exactly one OpenCode session listed (the parent, with the user's prompt), a checkpoint whose title is the user's prompt, and `checkpoint list --pending`/`explain` showing a `[Task]` row or `tasks/<callID>/` entries. Repeat with `--scenario concurrent` (two records, disjoint files) and `--scenario readonly` (one record, no files). Each run is one paid Gemini prompt; get authorization first.

- [ ] **Step 2: Duplication and gates**

Run: `mise run dup` — fix any new duplication in the opencode package (the two parse cases share `validateSubagentIdentity`; keep them otherwise distinct).
Run: `mise run fmt && mise run lint` — fix everything; rerun lint after fmt.
Run: `caffeinate -i -s mise run check` — must pass (unit, integration, canary). The canary lane includes a roger-roger sub-lane that fails hard when `roger-roger`/`entire-agent-roger-roger` are not on `PATH`; if that is the only failure, say so rather than treating it as a regression.

- [ ] **Step 3: Commit and push**

```bash
git add -A
git commit -m "opencode: native subagent tracking"  # only if anything remains uncommitted
git push
```

Then read the trail's review findings (`entire api --to cell /api/v1/trails/gh/entireio/cli/<n>/reviews/comments`) and address them before marking the PR ready. Never merge without explicit approval.
