package claudecode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

var fastCaptureConfig = captureConfig{
	pollInterval:   10 * time.Millisecond,
	quietWindow:    500 * time.Millisecond,
	maxWait:        2 * time.Second,
	staleThreshold: time.Minute,
}

func BenchmarkCaptureTranscriptModernReady(b *testing.B) {
	for _, tt := range []struct {
		name string
		size int
		tail bool
	}{
		{name: "64KiB", size: 64 << 10},
		{name: "4MiB", size: 4 << 20},
		{name: "70MiB", size: 70 << 20},
		{name: "70MiBTail", size: 70 << 20, tail: true},
	} {
		b.Run(tt.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "transcript.jsonl")
			data := benchmarkTranscript(tt.size)
			require.NoError(b, os.WriteFile(path, data, 0o600))
			response := testFinalAssistantMessage
			startPosition := 0
			if tt.tail {
				startPosition = bytes.Count(data, []byte{'\n'}) - 1
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				snapshot, err := (&ClaudeCodeAgent{}).CaptureTranscript(context.Background(), agent.TranscriptCaptureRequest{
					SessionRef:    path,
					StartPosition: startPosition,
					FinalResponse: &response,
				})
				if err != nil {
					b.Fatal(err)
				}
				if len(snapshot.Data) != len(data) {
					b.Fatalf("captured %d bytes, want %d", len(snapshot.Data), len(data))
				}
			}
		})
	}
}

func BenchmarkCaptureTranscriptLegacyReady(b *testing.B) {
	for _, tt := range []struct {
		name string
		size int
	}{
		{name: "64KiB", size: 64 << 10},
		{name: "4MiB", size: 4 << 20},
		{name: "70MiB", size: 70 << 20},
	} {
		b.Run(tt.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "transcript.jsonl")
			data := benchmarkTranscript(tt.size)
			require.NoError(b, os.WriteFile(path, data, 0o600))
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := (&ClaudeCodeAgent{}).CaptureTranscript(context.Background(), agent.TranscriptCaptureRequest{
					SessionRef: path,
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkAnalyzeTranscriptTurn(b *testing.B) {
	for _, tt := range []struct {
		name string
		size int
		tail bool
	}{
		{name: "64KiB", size: 64 << 10},
		{name: "4MiB", size: 4 << 20},
		{name: "70MiB", size: 70 << 20},
		{name: "70MiBTail", size: 70 << 20, tail: true},
	} {
		b.Run(tt.name, func(b *testing.B) {
			data := benchmarkTranscript(tt.size)
			startPosition := 0
			if tt.tail {
				startPosition = bytes.Count(data, []byte{'\n'}) - 1
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := (&ClaudeCodeAgent{}).AnalyzeTranscriptTurn(data, startPosition, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkTranscript(size int) []byte {
	content := bytes.Repeat([]byte("x"), 4<<10)
	line := append([]byte(`{"type":"user","message":{"content":"`), content...)
	line = append(line, []byte(`"}}`+"\n")...)
	finalLine := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	data := make([]byte, 0, size+len(line))
	for len(data)+len(line)+len(finalLine) <= size {
		data = append(data, line...)
	}
	return append(data, finalLine...)
}

func TestCaptureTranscript_LegacyStableUsesQuietWindow(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	want := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	require.NoError(t, os.WriteFile(path, want, 0o600))

	snapshot, err := (&ClaudeCodeAgent{}).CaptureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef: path,
	})
	require.NoError(t, err)
	require.Equal(t, want, snapshot.Data)
	require.Equal(t, 1, snapshot.Position)
}

func TestCaptureTranscript_ModernReadyUsesSemanticEvidenceWithoutPollingDelay(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	want := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	require.NoError(t, os.WriteFile(path, want, 0o600))
	config := fastCaptureConfig
	config.pollInterval = time.Hour
	config.maxWait = 250 * time.Millisecond
	response := testFinalAssistantMessage

	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, config)
	require.NoError(t, err)
	require.Equal(t, want, snapshot.Data)
}

func TestPrepareTranscript_RemainsBestEffortForNonStopCallers(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	require.NoError(t, (&ClaudeCodeAgent{}).PrepareTranscript(context.Background(), missing))
}

func TestPrepareTranscript_RetainsLegacyQuietWindow(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"user"}` + "\n")
	final := appendCopy(initial, []byte(`{"type":"assistant"}`+"\n"))
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return appendToFile(path, final[len(initial):])
	})
	require.NoError(t, (&ClaudeCodeAgent{}).PrepareTranscript(context.Background(), path))
	select {
	case err := <-writerDone:
		require.NoError(t, err)
	default:
		t.Fatal("PrepareTranscript returned before the pending append")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, final, data)
}

func TestCaptureTranscript_IgnoresMatchingResponseBeforeTurnStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	initial := []byte(
		`{"type":"user","message":{"content":"first"}}` + "\n" +
			`{"type":"assistant","message":{"content":"Done."}}` + "\n" +
			`{"type":"user","message":{"content":"second"}}` + "\n",
	)
	final := appendCopy(initial, []byte(`{"type":"assistant","message":{"content":"Done."}}`+"\n"))
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return appendToFile(path, final[len(initial):])
	})
	response := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 2,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.Equal(t, 4, snapshot.Position)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_WaitsForCompleteFinalJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"user","message":{"content":"fix"}}` + "\n" +
		`{"type":"assistant","message":{"content":"Do`)
	final := []byte(`{"type":"user","message":{"content":"fix"}}` + "\n" +
		`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return appendToFile(path, []byte(`ne."}}`+"\n"))
	})
	response := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 1,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.Equal(t, 2, snapshot.Position)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_TracksContinuedGrowth(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"user","message":{"content":"fix"}}` + "\n")
	intermediate := appendCopy(initial, []byte(`{"type":"assistant","message":{"content":"Working"}}`+"\n"))
	final := appendCopy(intermediate, []byte(`{"type":"assistant","message":{"content":"Done."}}`+"\n"))
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 60*time.Millisecond, func() error {
		if err := appendToFile(path, intermediate[len(initial):]); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
		return appendToFile(path, final[len(intermediate):])
	})
	response := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 1,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.Equal(t, 3, snapshot.Position)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_LegacyQuietWindowSurvivesMidWritePause(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"user","message":{"content":"fix"}}` + "\n")
	final := appendCopy(initial, []byte(`{"type":"assistant","message":{"content":"Done."}}`+"\n"))
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return appendToFile(path, final[len(initial):])
	})
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 1,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.Equal(t, 2, snapshot.Position)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_DetectsTruncation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"user","message":{"content":"a much longer old prompt"}}` + "\n" +
		`{"type":"assistant","message":{"content":"Not yet"}}` + "\n")
	final := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return os.WriteFile(path, final, 0o600)
	})
	response := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.Equal(t, 1, snapshot.Position)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_DetectsReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	replacement := filepath.Join(dir, "replacement.jsonl")
	initial := []byte(`{"type":"assistant","message":{"content":"Not yet"}}` + "\n")
	final := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	require.NoError(t, os.WriteFile(path, initial, 0o600))
	require.NoError(t, os.WriteFile(replacement, final, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return os.Rename(replacement, path)
	})
	response := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.Equal(t, 1, snapshot.Position)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_DetectsSameSizeRewriteWhenModificationTimeChanges(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"assistant","message":{"content":"Wait."}}` + "\n")
	final := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")
	require.Len(t, final, len(initial))
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		if err := os.WriteFile(path, final, 0o600); err != nil {
			return err
		}
		modified := time.Now().Add(time.Second)
		return os.Chtimes(path, modified, modified)
	})
	response := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.NoError(t, <-writerDone)
}

func TestCaptureTranscript_AcceptsCompleteFinalJSONWithoutNewline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	want := []byte(`{"type":"assistant","message":{"content":"Done."}}`)
	require.NoError(t, os.WriteFile(path, want, 0o600))
	response := testFinalAssistantMessage

	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, want, snapshot.Data)
	require.Equal(t, 1, snapshot.Position)
}

func TestCaptureTranscript_NormalizesFinalAssistantMessageLikeClaude(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	want := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"  Fixed the parser.  "},{"type":"tool_use","name":"ignored"},{"type":"text","text":"Tests pass.  "}]}}`)
	require.NoError(t, os.WriteFile(path, want, 0o600))
	response := "Fixed the parser.  \nTests pass."

	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, want, snapshot.Data)
	require.Equal(t, 1, snapshot.Position)
}

func TestCaptureTranscript_MissingAndStaleFilesFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "missing.jsonl")
		snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
			SessionRef: path,
		}, fastCaptureConfig)
		require.ErrorIs(t, err, agent.ErrTranscriptNotReady)
		require.Empty(t, snapshot.Data)
		require.Zero(t, snapshot.Position)
	})

	t.Run("stale", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "stale.jsonl")
		require.NoError(t, os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600))
		staleTime := time.Now().Add(-2 * fastCaptureConfig.staleThreshold)
		require.NoError(t, os.Chtimes(path, staleTime, staleTime))

		snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
			SessionRef: path,
		}, fastCaptureConfig)
		require.ErrorIs(t, err, agent.ErrTranscriptNotReady)
		require.Empty(t, snapshot.Data)
		require.Zero(t, snapshot.Position)
	})
}

func TestCaptureTranscript_ModernMarkerTimeoutFailsClosed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"assistant","message":{"content":"Different"}}`+"\n"), 0o600))
	config := fastCaptureConfig
	config.maxWait = 150 * time.Millisecond
	response := testFinalAssistantMessage

	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, config)
	require.ErrorIs(t, err, agent.ErrTranscriptNotReady)
	require.Empty(t, snapshot.Data)
	require.Zero(t, snapshot.Position)
}

func TestCaptureTranscript_DoesNotRereadUnchangedRejectedSnapshot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"assistant","message":{"content":"Different"}}`+"\n"), 0o600))
	config := fastCaptureConfig
	config.maxWait = 150 * time.Millisecond
	readAttempts := 0
	config.readTranscript = func(
		ctx context.Context,
		path string,
		fingerprint transcriptFingerprint,
	) ([]byte, error) {
		readAttempts++
		return readObservedTranscript(ctx, path, fingerprint)
	}
	response := testFinalAssistantMessage

	_, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, config)
	require.ErrorIs(t, err, agent.ErrTranscriptNotReady)
	require.Equal(t, 1, readAttempts)
}

func TestCaptureTranscript_CancellationFailsClosed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"assistant","message":{"content":"Different"}}`+"\n"), 0o600))
	response := testFinalAssistantMessage
	ctx, cancel := context.WithCancel(context.Background())
	cancelDone := writeAfter(t, 100*time.Millisecond, func() error {
		cancel()
		return nil
	})

	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(ctx, agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 0,
		FinalResponse: &response,
	}, fastCaptureConfig)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, snapshot.Data)
	require.Zero(t, snapshot.Position)
	require.NoError(t, <-cancelDone)
}

func TestValidateTranscriptSnapshot_CancellationFailsClosed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := []byte(`{"type":"assistant","message":{"content":"Done."}}` + "\n")

	position, assistant, err := validateTranscriptSnapshot(ctx, data, 0, true)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, position)
	require.Empty(t, assistant)
}

func TestCaptureTranscript_EmptyFinalResponseUsesLegacyQuietWindow(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	initial := []byte(`{"type":"user","message":{"content":"fix"}}` + "\n")
	final := appendCopy(initial, []byte(`{"type":"assistant","message":{"content":"Done."}}`+"\n"))
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	writerDone := writeAfter(t, 100*time.Millisecond, func() error {
		return appendToFile(path, final[len(initial):])
	})
	empty := ""
	snapshot, err := (&ClaudeCodeAgent{}).captureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    path,
		StartPosition: 1,
		FinalResponse: &empty,
	}, fastCaptureConfig)
	require.NoError(t, err)
	require.Equal(t, final, snapshot.Data)
	require.NoError(t, <-writerDone)
}

func writeAfter(t *testing.T, delay time.Duration, write func() error) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		done <- write()
	}()
	return done
}

func appendCopy(prefix, suffix []byte) []byte {
	result := make([]byte, 0, len(prefix)+len(suffix))
	result = append(result, prefix...)
	return append(result, suffix...)
}

func appendToFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.Write(data)
	return err
}
