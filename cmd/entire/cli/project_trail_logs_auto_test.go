package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestAutoLogStartOffset(t *testing.T) {
	t.Parallel()
	previous := autoLogCursor{Scope: "old", Transcript: "file", Offset: 20}
	for _, tc := range []struct {
		name, scope, path string
		size              int64
		exists            bool
		want              int64
	}{
		{"first linked session", "old", "file", 50, false, 0},
		{"unlinked session", "", "file", 50, false, 50},
		{"large history", "old", "file", localTranscriptMaxBytes + 1, false, localTranscriptMaxBytes + 1},
		{"same scope resume", "old", "file", 50, true, 20},
		{"reparent", "new", "file", 50, true, 50},
		{"unlink", "", "file", 50, true, 50},
		{"new transcript", "old", "other", 50, true, 50},
		{"truncated transcript", "old", "file", 10, true, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, autoLogStartOffset(previous, tc.exists, tc.scope, tc.path, tc.size))
		})
	}
}

func newAutoLogReaderTest(t *testing.T, content string) (*autoLogReader, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	_, err = file.WriteString(content)
	require.NoError(t, err)
	info, err := file.Stat()
	require.NoError(t, err)
	return &autoLogReader{ctx: t.Context(), file: file, initial: info, stat: func() (os.FileInfo, error) { return os.Lstat(path) },
		check: func() (bool, error) { return true, nil }, save: func(int64) error { return nil }, interval: time.Millisecond}, file
}

func TestAutoLogReaderFollowsAndStops(t *testing.T) {
	t.Parallel()
	reader, file := newAutoLogReaderTest(t, "first\n")
	var ended atomic.Bool
	reader.check = func() (bool, error) { return ended.Load(), nil }
	buf := make([]byte, 64)
	n, err := reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "first\n", string(buf[:n]))
	done := make(chan string, 1)
	go func() {
		n, err := reader.Read(buf)
		if err != nil {
			done <- err.Error()
		} else {
			done <- string(buf[:n])
		}
	}()
	select {
	case <-done:
		t.Fatal("temporary EOF ended capture")
	case <-time.After(10 * time.Millisecond):
	}
	_, err = file.WriteString("second\n")
	require.NoError(t, err)
	select {
	case got := <-done:
		require.Equal(t, "second\n", got)
	case <-time.After(time.Second):
		t.Fatal("tail did not resume")
	}
	ended.Store(true)
	_, err = reader.Read(buf)
	require.ErrorIs(t, err, io.EOF)
}

func TestAutoLogReaderRejectsUnsafeChanges(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"rewrite", "truncate", "replace", "symlink", "association", "save", "limit", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			reader, file := newAutoLogReaderTest(t, "first\n")
			buf := make([]byte, 64)
			_, err := reader.Read(buf)
			require.NoError(t, err)
			switch kind {
			case "rewrite":
				_, err = file.WriteAt([]byte("other\n"), 0)
				require.NoError(t, err)
			case "truncate":
				require.NoError(t, file.Truncate(0))
			case "replace":
				replacement := filepath.Join(t.TempDir(), "replacement")
				require.NoError(t, os.WriteFile(replacement, []byte("second\n"), 0o600))
				reader.stat = func() (os.FileInfo, error) { return os.Lstat(replacement) }
			case "symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(file.Name(), link); err != nil {
					t.Skip("symlink unavailable")
				}
				reader.stat = func() (os.FileInfo, error) { return os.Lstat(link) }
			case "association":
				reader.check = func() (bool, error) { return false, errors.New("branch changed") }
			case "save":
				reader.save = func(int64) error { return errors.New("disk full") }
				_, err = file.WriteString("second\n")
				require.NoError(t, err)
			case "limit":
				reader.captured = make([]byte, localTranscriptMaxBytes)
				reader.offset = int64(len(reader.captured))
				require.NoError(t, file.Truncate(reader.offset+1))
				_, err = file.WriteAt(reader.captured, 0)
				require.NoError(t, err)
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				reader.ctx = ctx
			}
			n, err := reader.Read(buf)
			require.Error(t, err)
			require.Zero(t, n)
		})
	}
}

func TestAutoLogReaderSkipsPartialResumeLine(t *testing.T) {
	t.Parallel()
	reader, _ := newAutoLogReaderTest(t, "partial\ncomplete\n")
	reader.offset = 3
	reader.skipPartial = autoLogPartialLine(reader.file, 3)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "complete\n", string(got))
}

func TestAutoLogCaptureFailureStopsIdleTail(t *testing.T) {
	t.Parallel()
	reader, _ := newAutoLogReaderTest(t, "invalid JSON\n")
	reader.check = func() (bool, error) { return false, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	readerCtx, stopInput := context.WithCancel(ctx)
	defer stopInput()
	reader.ctx = readerCtx
	var records []localLogRecord
	err := publishLocalTranscriptCapture(ctx, reader, io.Discard, "pi", nil, func(_ context.Context, b []localLogRecord) error { records = append(records, b...); return nil }, stopInput)
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "capture failure must not leave a tail waiting for future data")
	require.NotEmpty(t, records)
	for _, record := range records {
		require.Equal(t, localLogKindGap, record.Kind)
	}
}

func TestAutoLogSuffixFormats(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pi":          `{"type":"message","id":"m1","parentId":"earlier","timestamp":"2026-10-12T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"new turn"}]}}`,
		"codex":       `{"timestamp":"2026-10-12T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"new turn"}]}}`,
		"claude-code": `{"type":"user","message":{"content":"new turn"}}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var records []localLogRecord
			input := io.MultiReader(autoLogFormatHeader(name), strings.NewReader(line+"\n"))
			err := publishLocalTranscript(t.Context(), input, io.Discard, name, localLogFilters(), func(_ context.Context, b []localLogRecord) error { records = append(records, b...); return nil })
			require.NoError(t, err)
			require.Len(t, records, 2)
			require.Contains(t, records[0].Text, "new turn")
			require.Equal(t, localLogKindComplete, records[1].Kind)
		})
	}
}

// This fixture owns the CWD, environment and client seams; never run in parallel.
func setupAutoLogTest(t *testing.T) (string, agent.Agent, *session.StateStore, *session.State) {
	t.Helper()
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.WriteFile(t, root, "file.txt", "initial")
	testutil.GitAdd(t, root, "file.txt")
	testutil.GitCommit(t, root, "initial")
	t.Chdir(root)
	root, rootErr := paths.WorktreeRoot(t.Context())
	require.NoError(t, rootErr)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widget.git")
	testutil.WriteFile(t, root, ".entire/settings.json", `{"enabled":true}`)
	t.Setenv("ENTIRE_TEST_PI_SESSION_DIR", t.TempDir())
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	// Fake claims are consumed only for local scope identity; no real auth API is contacted.
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://auth.entire.io","sub":"test-account","aud":"https://auth.entire.io"}`))
	t.Setenv("ENTIRE_TOKEN", base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))+"."+claims+".c2ln")
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	ag, err := agent.Get(agent.AgentNamePi)
	require.NoError(t, err)
	transcriptStore, err := agent.OpenSessionStore(ag, root)
	require.NoError(t, err)
	require.NoError(t, transcriptStore.WriteFile("fixture.jsonl", []byte("{\"type\":\"session\",\"version\":3}\n"), 0o600))
	states, err := session.NewStateStore(t.Context())
	require.NoError(t, err)
	state := &session.State{SessionID: "auto-test-session", AgentType: ag.Type(), WorktreePath: root, TranscriptPath: filepath.Join(transcriptStore.Dir(), "fixture.jsonl"), StartedAt: time.Now(), Phase: session.PhaseActive}
	require.NoError(t, states.Save(t.Context(), state))
	return root, ag, states, state
}

func TestAutoLogHookDeduplicatesAndScopesSession(t *testing.T) {
	root, ag, states, state := setupAutoLogTest(t)
	previous := spawnAutoLogProcess
	t.Cleanup(func() { spawnAutoLogProcess = previous })
	calls := 0
	spawnAutoLogProcess = func(dir string, args ...string) {
		calls++
		require.Equal(t, root, dir)
		require.Equal(t, []string{autoLogCommand, state.SessionID}, args)
	}
	event := &agent.Event{Type: agent.TurnStart, SessionID: state.SessionID}
	maybeStreamTrailSession(t.Context(), ag, event)
	require.Equal(t, 1, calls)
	common, name, err := autoLogFiles(t.Context(), state.SessionID)
	require.NoError(t, err)
	release, err := tryAutoLogLock(t.Context(), common, name)
	require.NoError(t, err)
	maybeStreamTrailSession(t.Context(), ag, event)
	require.Equal(t, 1, calls, "duplicate worker while lease is held")
	release()
	require.NoError(t, saveAutoLogCursor(common, name, autoLogCursor{RetryAfter: time.Now().Add(time.Minute)}))
	maybeStreamTrailSession(t.Context(), ag, event)
	require.Equal(t, 1, calls, "failed lookups must back off")
	require.NoError(t, saveAutoLogCursor(common, name, autoLogCursor{}))
	state.WorktreePath = t.TempDir()
	require.NoError(t, states.Save(t.Context(), state))
	maybeStreamTrailSession(t.Context(), ag, event)
	require.Equal(t, 1, calls, "another worktree must not be captured")
	event.SessionID = "not-tracked"
	maybeStreamTrailSession(t.Context(), ag, event)
	require.Equal(t, 1, calls, "no most-recent-session fallback")
}

func TestAutoLogHookDoesNotSpawnWithoutLocalIdentity(t *testing.T) {
	_, ag, _, state := setupAutoLogTest(t)
	t.Setenv("ENTIRE_TOKEN", "")
	old := spawnAutoLogProcess
	t.Cleanup(func() { spawnAutoLogProcess = old })
	spawnAutoLogProcess = func(string, ...string) { t.Fatal("logged-out session spawned a worker") }
	maybeStreamTrailSession(t.Context(), ag, &agent.Event{Type: agent.TurnStart, SessionID: state.SessionID})
}

func TestAutoLogLinkRequiresIdentifiedCaller(t *testing.T) {
	_, _, _, state := setupAutoLogTest(t)
	for _, key := range agent.CallerSessionEnvVars() {
		t.Setenv(key, "")
	}
	old := spawnAutoLogProcess
	t.Cleanup(func() { spawnAutoLogProcess = old })
	calls := 0
	spawnAutoLogProcess = func(_ string, args ...string) { calls++; require.Equal(t, state.SessionID, args[1]) }
	cmd := newTrailCmdForMode(true)
	cmd.SetContext(t.Context())
	branch, err := GetCurrentBranch(t.Context())
	require.NoError(t, err)
	maybeStreamCallingTrailSession(cmd, branch)
	require.Zero(t, calls, "a most-recent worktree session is not an identified caller")
	t.Setenv("PI_SESSION_ID", state.SessionID)
	maybeStreamCallingTrailSession(cmd, branch)
	require.Equal(t, 1, calls)
	maybeStreamCallingTrailSession(cmd, "other-branch")
	require.Equal(t, 1, calls)
}

func TestAutoLogProductionWiringWithMockAPI(t *testing.T) {
	_, _, states, state := setupAutoLogTest(t)
	branch, err := GetCurrentBranch(t.Context())
	require.NoError(t, err)
	var linked atomic.Bool
	linked.Store(true)
	uploaded := make(chan []localLogRecord, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/gh/acme/widget":
			work := api.TrailResource{ID: projectTrailTestChange, Branch: branch, Number: 7}
			if linked.Load() {
				work.Parent = &api.TrailParentReference{ID: projectTrailTestID, ProjectID: projectTrailTestProject, Host: "gh", Project: "acme", Path: projectTrailTestPath}
			}
			if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{work}}); err != nil {
				t.Error(err)
			}
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, projectTrailTestPath+"/logs/"):
			var body struct {
				Records []localLogRecord `json:"records"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			uploaded <- body.Records
			if err := json.NewEncoder(w).Encode(map[string]any{"id": filepath.Base(r.URL.Path), "cursor": body.Records[len(body.Records)-1].Sequence}); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	oldRepo, oldLog := newTrailAPIClient, newProjectLogClient
	t.Cleanup(func() { newTrailAPIClient, newProjectLogClient = oldRepo, oldLog })
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("test", server.URL), "repo", nil
	}
	newProjectLogClient = func(context.Context, bool) (*api.Client, error) {
		return api.NewClientWithBaseURL("test", server.URL), nil
	}
	file, err := os.OpenFile(state.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	write := func(text string) { t.Helper(); _, err := file.WriteString(text + "\n"); require.NoError(t, err) }
	write(`{"type":"message","id":"p1","parentId":null,"timestamp":"2026-10-12T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"password=fake-secret"}]}}`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- runAutoTrailSessionLogs(ctx, state.SessionID) }()
	defer func() { cancel(); <-exited }()
	select {
	case records := <-uploaded:
		require.Equal(t, localLogKindMessage, records[0].Kind)
		require.NotContains(t, records[0].Text, "fake-secret")
		require.Contains(t, records[0].Text, "[REDACTED]")
	case err := <-done:
		t.Fatalf("publisher exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("automatic transcript did not reach mocked project BFF")
	}
	// Same branch, removed parent: local branch checks alone cannot catch this.
	linked.Store(false)
	write(`{"type":"message","id":"p2","parentId":"p1","timestamp":"2026-10-12T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"must not reach old trail"}]}}`)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("publisher did not stop after unlink")
	}
	select {
	case b := <-uploaded:
		t.Fatalf("uploaded after unlink: %+v", b)
	default:
	}
	// The real session remains active; streaming failures do not end the agent.
	current, err := states.Load(t.Context(), state.SessionID)
	require.NoError(t, err)
	require.False(t, current.IsEnded())
}
