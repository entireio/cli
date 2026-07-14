package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testOpPostCommit = "post-commit"

func TestParseTraceEntry(t *testing.T) {
	t.Parallel()

	t.Run("valid trace entry", func(t *testing.T) {
		t.Parallel()

		line := `{"time":"2026-01-15T10:30:00.000Z","level":"DEBUG","msg":"perf","component":"perf","op":"post-commit","duration_ms":150,"error":true,"steps.load_session_ms":50,"steps.save_checkpoint_ms":80,"steps.save_checkpoint_err":true}`

		entry := parseTraceEntry(line)
		if entry == nil {
			t.Fatal("parseTraceEntry returned nil for valid trace entry")
			return
		}

		if entry.Op != testOpPostCommit {
			t.Errorf("Op = %q, want %q", entry.Op, testOpPostCommit)
		}
		if entry.DurationMs != 150 {
			t.Errorf("DurationMs = %d, want %d", entry.DurationMs, 150)
		}
		if !entry.Error {
			t.Error("Error = false, want true")
		}

		expectedTime, err := time.Parse(time.RFC3339, "2026-01-15T10:30:00.000Z")
		if err != nil {
			t.Fatalf("failed to parse expected time: %v", err)
		}
		if !entry.Time.Equal(expectedTime) {
			t.Errorf("Time = %v, want %v", entry.Time, expectedTime)
		}

		if len(entry.Steps) != 2 {
			t.Fatalf("len(Steps) = %d, want 2", len(entry.Steps))
		}

		// Steps are sorted alphabetically by name
		if entry.Steps[0].Name != "load_session" {
			t.Errorf("Steps[0].Name = %q, want %q", entry.Steps[0].Name, "load_session")
		}
		if entry.Steps[0].DurationMs != 50 {
			t.Errorf("Steps[0].DurationMs = %d, want %d", entry.Steps[0].DurationMs, 50)
		}
		if entry.Steps[0].Error {
			t.Error("Steps[0].Error = true, want false")
		}

		if entry.Steps[1].Name != "save_checkpoint" {
			t.Errorf("Steps[1].Name = %q, want %q", entry.Steps[1].Name, "save_checkpoint")
		}
		if entry.Steps[1].DurationMs != 80 {
			t.Errorf("Steps[1].DurationMs = %d, want %d", entry.Steps[1].DurationMs, 80)
		}
		if !entry.Steps[1].Error {
			t.Error("Steps[1].Error = false, want true")
		}
	})

	t.Run("non-perf entry returns nil", func(t *testing.T) {
		t.Parallel()

		line := `{"time":"2026-01-15T10:30:00.000Z","level":"INFO","msg":"hook invoked","component":"lifecycle","hook":"post-commit"}`

		entry := parseTraceEntry(line)
		if entry != nil {
			t.Errorf("parseTraceEntry returned %+v for non-perf entry, want nil", entry)
		}
	})

	t.Run("invalid JSON returns nil", func(t *testing.T) {
		t.Parallel()

		entry := parseTraceEntry("this is not json at all{{{")
		if entry != nil {
			t.Errorf("parseTraceEntry returned %+v for invalid JSON, want nil", entry)
		}
	})
}

func TestCollectTraceEntries(t *testing.T) {
	t.Parallel()

	// Fixture: 4 lines — 2 prepare-commit-msg, 1 non-perf, 1 post-commit
	fixtureLines := []string{
		`{"time":"2026-01-15T10:00:00Z","level":"DEBUG","msg":"perf","op":"prepare-commit-msg","duration_ms":100}`,
		`{"time":"2026-01-15T10:01:00Z","level":"DEBUG","msg":"perf","op":"prepare-commit-msg","duration_ms":120}`,
		`{"time":"2026-01-15T10:02:00Z","level":"INFO","msg":"hook invoked","component":"lifecycle","hook":"post-commit"}`,
		`{"time":"2026-01-15T10:03:00Z","level":"DEBUG","msg":"perf","op":"post-commit","duration_ms":200}`,
	}
	fixtureContent := strings.Join(fixtureLines, "\n") + "\n"

	writeFixture := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "trace.jsonl")
		if err := os.WriteFile(p, []byte(fixtureContent), 0o644); err != nil {
			t.Fatalf("failed to write fixture: %v", err)
		}
		return p
	}

	t.Run("last 2 entries", func(t *testing.T) {
		t.Parallel()
		logFile := writeFixture(t)

		entries, err := collectTraceEntries(logFile, 2, "")
		if err != nil {
			t.Fatalf("collectTraceEntries returned error: %v", err)
		}

		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2", len(entries))
		}

		// Newest first: post-commit (line 4), then prepare-commit-msg (line 2)
		if entries[0].Op != testOpPostCommit {
			t.Errorf("entries[0].Op = %q, want %q", entries[0].Op, testOpPostCommit)
		}
		if entries[0].DurationMs != 200 {
			t.Errorf("entries[0].DurationMs = %d, want %d", entries[0].DurationMs, 200)
		}
		if entries[1].Op != "prepare-commit-msg" {
			t.Errorf("entries[1].Op = %q, want %q", entries[1].Op, "prepare-commit-msg")
		}
		if entries[1].DurationMs != 120 {
			t.Errorf("entries[1].DurationMs = %d, want %d", entries[1].DurationMs, 120)
		}
	})

	t.Run("filter by hook type", func(t *testing.T) {
		t.Parallel()
		logFile := writeFixture(t)

		entries, err := collectTraceEntries(logFile, 10, testOpPostCommit)
		if err != nil {
			t.Fatalf("collectTraceEntries returned error: %v", err)
		}

		if len(entries) != 1 {
			t.Fatalf("got %d entries, want 1", len(entries))
		}
		if entries[0].Op != testOpPostCommit {
			t.Errorf("entries[0].Op = %q, want %q", entries[0].Op, testOpPostCommit)
		}
	})

	t.Run("file not found returns empty", func(t *testing.T) {
		t.Parallel()

		entries, err := collectTraceEntries("/nonexistent/path/trace.jsonl", 10, "")
		if err != nil {
			t.Fatalf("expected nil error for missing file, got %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected empty entries, got %d", len(entries))
		}
	})
}

func TestRenderTraceEntries(t *testing.T) {
	t.Parallel()

	entries := []traceEntry{
		{
			Op:         "post-commit",
			DurationMs: 250,
			Time:       time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
			Steps: []traceStep{
				{Name: "load_session", DurationMs: 50, Error: false},
				{Name: "save_checkpoint", DurationMs: 80, Error: true},
				{Name: "condense", DurationMs: 120, Error: false},
			},
		},
	}

	var buf bytes.Buffer
	renderTraceEntries(&buf, entries)
	out := buf.String()

	// Verify header contains op name, total duration, and timestamp
	if !strings.Contains(out, "post-commit") {
		t.Errorf("output missing op name 'post-commit':\n%s", out)
	}
	if !strings.Contains(out, "250ms") {
		t.Errorf("output missing total duration '250ms':\n%s", out)
	}
	if !strings.Contains(out, "2026-01-15T10:30:00Z") {
		t.Errorf("output missing RFC3339 timestamp:\n%s", out)
	}

	// Verify step names and durations appear
	if !strings.Contains(out, "load_session") {
		t.Errorf("output missing step name 'load_session':\n%s", out)
	}
	if !strings.Contains(out, "50ms") {
		t.Errorf("output missing step duration '50ms':\n%s", out)
	}
	if !strings.Contains(out, "save_checkpoint") {
		t.Errorf("output missing step name 'save_checkpoint':\n%s", out)
	}
	if !strings.Contains(out, "80ms") {
		t.Errorf("output missing step duration '80ms':\n%s", out)
	}
	if !strings.Contains(out, "condense") {
		t.Errorf("output missing step name 'condense':\n%s", out)
	}
	if !strings.Contains(out, "120ms") {
		t.Errorf("output missing step duration '120ms':\n%s", out)
	}

	// Verify column header
	if !strings.Contains(out, "STEP") {
		t.Errorf("output missing column header 'STEP':\n%s", out)
	}
	if !strings.Contains(out, "DURATION") {
		t.Errorf("output missing column header 'DURATION':\n%s", out)
	}

	// Verify error marker on the errored step line
	lines := strings.Split(out, "\n")
	foundErrorMarker := false
	for _, line := range lines {
		if strings.Contains(line, "save_checkpoint") && strings.Contains(line, "x") {
			foundErrorMarker = true
			break
		}
	}
	if !foundErrorMarker {
		t.Errorf("output missing error marker 'x' on save_checkpoint line:\n%s", out)
	}
}

func TestParseTraceEntry_WithSubSteps(t *testing.T) {
	t.Parallel()

	line := `{"time":"2026-01-15T10:30:00Z","level":"DEBUG","msg":"perf","op":"post-commit","duration_ms":350,"steps.process_sessions_ms":300,"steps.process_sessions.0_ms":100,"steps.process_sessions.1_ms":200,"steps.process_sessions.1_err":true,"steps.cleanup_ms":50}`

	entry := parseTraceEntry(line)
	if entry == nil {
		t.Fatal("parseTraceEntry returned nil")
		return
	}

	if len(entry.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(entry.Steps))
	}

	// Steps sorted alphabetically: cleanup, process_sessions
	if entry.Steps[0].Name != "cleanup" {
		t.Errorf("Steps[0].Name = %q, want %q", entry.Steps[0].Name, "cleanup")
	}
	if len(entry.Steps[0].SubSteps) != 0 {
		t.Errorf("Steps[0] should have no sub-steps, got %d", len(entry.Steps[0].SubSteps))
	}

	ps := entry.Steps[1]
	if ps.Name != "process_sessions" {
		t.Errorf("Steps[1].Name = %q, want %q", ps.Name, "process_sessions")
	}
	if ps.DurationMs != 300 {
		t.Errorf("Steps[1].DurationMs = %d, want %d", ps.DurationMs, 300)
	}
	if len(ps.SubSteps) != 2 {
		t.Fatalf("Steps[1] should have 2 sub-steps, got %d", len(ps.SubSteps))
	}
	if ps.SubSteps[0].Name != "process_sessions.0" {
		t.Errorf("SubSteps[0].Name = %q, want %q", ps.SubSteps[0].Name, "process_sessions.0")
	}
	if ps.SubSteps[0].DurationMs != 100 {
		t.Errorf("SubSteps[0].DurationMs = %d, want %d", ps.SubSteps[0].DurationMs, 100)
	}
	if ps.SubSteps[0].Error {
		t.Error("SubSteps[0].Error = true, want false")
	}
	if ps.SubSteps[1].Name != "process_sessions.1" {
		t.Errorf("SubSteps[1].Name = %q, want %q", ps.SubSteps[1].Name, "process_sessions.1")
	}
	if ps.SubSteps[1].DurationMs != 200 {
		t.Errorf("SubSteps[1].DurationMs = %d, want %d", ps.SubSteps[1].DurationMs, 200)
	}
	if !ps.SubSteps[1].Error {
		t.Error("SubSteps[1].Error = false, want true")
	}
}

func TestParseTraceEntry_WithNestedNamedSpans(t *testing.T) {
	t.Parallel()

	line := `{"time":"2026-01-15T10:30:00Z","level":"DEBUG","msg":"perf","op":"why","duration_ms":500,"steps.enrich_commits_ms":300,"steps.enrich_commits.why_checkpoint_info_ms":200,"steps.enrich_commits.why_checkpoint_info.why_checkpoint_read_session_ms":120,"steps.enrich_commits.why_checkpoint_info.why_checkpoint_read_session_err":true,"steps.render_ms":50}`

	entry := parseTraceEntry(line)
	if entry == nil {
		t.Fatal("parseTraceEntry returned nil")
		return
	}

	if len(entry.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(entry.Steps))
	}

	if entry.Steps[0].Name != "enrich_commits" {
		t.Errorf("Steps[0].Name = %q, want %q", entry.Steps[0].Name, "enrich_commits")
	}
	if entry.Steps[1].Name != "render" {
		t.Errorf("Steps[1].Name = %q, want %q", entry.Steps[1].Name, "render")
	}

	enrich := entry.Steps[0]
	if enrich.DurationMs != 300 {
		t.Errorf("enrich.DurationMs = %d, want %d", enrich.DurationMs, 300)
	}
	if len(enrich.SubSteps) != 1 {
		t.Fatalf("enrich should have 1 sub-step, got %d", len(enrich.SubSteps))
	}

	checkpointInfo := enrich.SubSteps[0]
	if checkpointInfo.Name != "enrich_commits.why_checkpoint_info" {
		t.Errorf("checkpointInfo.Name = %q, want %q", checkpointInfo.Name, "enrich_commits.why_checkpoint_info")
	}
	if checkpointInfo.DurationMs != 200 {
		t.Errorf("checkpointInfo.DurationMs = %d, want %d", checkpointInfo.DurationMs, 200)
	}
	if checkpointInfo.Error {
		t.Error("checkpointInfo.Error = true, want false")
	}
	if len(checkpointInfo.SubSteps) != 1 {
		t.Fatalf("checkpointInfo should have 1 sub-step, got %d", len(checkpointInfo.SubSteps))
	}

	readSession := checkpointInfo.SubSteps[0]
	if readSession.Name != "enrich_commits.why_checkpoint_info.why_checkpoint_read_session" {
		t.Errorf("readSession.Name = %q, want %q", readSession.Name, "enrich_commits.why_checkpoint_info.why_checkpoint_read_session")
	}
	if readSession.DurationMs != 120 {
		t.Errorf("readSession.DurationMs = %d, want %d", readSession.DurationMs, 120)
	}
	if !readSession.Error {
		t.Error("readSession.Error = false, want true")
	}
}

func TestParseTraceEntry_SubStepNumericOrdering(t *testing.T) {
	t.Parallel()

	// Build a trace entry with 12 sub-steps to verify numeric (not lexicographic) ordering.
	// Lexicographic sort would place .10 and .11 before .2.
	line := `{"time":"2026-01-15T10:30:00Z","level":"DEBUG","msg":"perf","op":"post-commit","duration_ms":1000,"steps.loop_ms":900,"steps.loop.0_ms":10,"steps.loop.1_ms":20,"steps.loop.2_ms":30,"steps.loop.3_ms":40,"steps.loop.4_ms":50,"steps.loop.5_ms":60,"steps.loop.6_ms":70,"steps.loop.7_ms":80,"steps.loop.8_ms":90,"steps.loop.9_ms":100,"steps.loop.10_ms":110,"steps.loop.11_ms":120}`

	entry := parseTraceEntry(line)
	if entry == nil {
		t.Fatal("parseTraceEntry returned nil")
		return
	}

	if len(entry.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(entry.Steps))
	}

	subs := entry.Steps[0].SubSteps
	if len(subs) != 12 {
		t.Fatalf("len(SubSteps) = %d, want 12", len(subs))
	}

	for i, sub := range subs {
		want := fmt.Sprintf("loop.%d", i)
		if sub.Name != want {
			t.Errorf("SubSteps[%d].Name = %q, want %q", i, sub.Name, want)
		}
		wantMs := int64((i + 1) * 10)
		if sub.DurationMs != wantMs {
			t.Errorf("SubSteps[%d].DurationMs = %d, want %d", i, sub.DurationMs, wantMs)
		}
	}
}

func TestRenderTraceEntries_WithSubSteps(t *testing.T) {
	t.Parallel()

	entries := []traceEntry{
		{
			Op:         "post-commit",
			DurationMs: 350,
			Steps: []traceStep{
				{Name: "process_sessions", DurationMs: 300, SubSteps: []traceStep{
					{Name: "process_sessions.0", DurationMs: 100},
					{Name: "process_sessions.1", DurationMs: 200, Error: true},
				}},
				{Name: "cleanup", DurationMs: 50},
			},
		},
	}

	var buf bytes.Buffer
	renderTraceEntries(&buf, entries)
	out := buf.String()

	// Parent step appears
	if !strings.Contains(out, "process_sessions") {
		t.Errorf("output missing group step 'process_sessions':\n%s", out)
	}
	if !strings.Contains(out, "300ms") {
		t.Errorf("output missing group duration '300ms':\n%s", out)
	}

	// Sub-steps with tree connectors
	if !strings.Contains(out, "├─ process_sessions.0") {
		t.Errorf("output missing sub-step with tree connector '├─ process_sessions.0':\n%s", out)
	}
	if !strings.Contains(out, "└─ process_sessions.1") {
		t.Errorf("output missing sub-step with tree connector '└─ process_sessions.1':\n%s", out)
	}

	// Error marker on sub-step
	lines := strings.Split(out, "\n")
	foundSubError := false
	for _, line := range lines {
		if strings.Contains(line, "process_sessions.1") && strings.Contains(line, "x") {
			foundSubError = true
			break
		}
	}
	if !foundSubError {
		t.Errorf("output missing error marker on process_sessions.1:\n%s", out)
	}

	// Regular step still appears
	if !strings.Contains(out, "cleanup") {
		t.Errorf("output missing regular step 'cleanup':\n%s", out)
	}
}

func TestRenderTraceEntries_WithNestedNamedSubSteps(t *testing.T) {
	t.Parallel()

	entries := []traceEntry{
		{
			Op:         "why",
			DurationMs: 500,
			Steps: []traceStep{
				{Name: "enrich_commits", DurationMs: 300, SubSteps: []traceStep{
					{Name: "enrich_commits.why_checkpoint_info", DurationMs: 200, SubSteps: []traceStep{
						{Name: "enrich_commits.why_checkpoint_info.why_checkpoint_read_session", DurationMs: 120, Error: true},
					}},
				}},
				{Name: "render", DurationMs: 50},
			},
		},
	}

	var buf bytes.Buffer
	renderTraceEntries(&buf, entries)
	out := buf.String()

	if !strings.Contains(out, "enrich_commits") {
		t.Errorf("output missing parent step 'enrich_commits':\n%s", out)
	}
	if !strings.Contains(out, "├─ enrich_commits.why_checkpoint_info") &&
		!strings.Contains(out, "└─ enrich_commits.why_checkpoint_info") {
		t.Errorf("output missing named nested sub-step with tree connector:\n%s", out)
	}

	lines := strings.Split(out, "\n")
	foundReadSession := false
	foundReadSessionError := false
	for _, line := range lines {
		if strings.Contains(line, "enrich_commits.why_checkpoint_info.why_checkpoint_read_session") {
			foundReadSession = true
			if strings.Contains(line, "x") {
				foundReadSessionError = true
			}
		}
	}
	if !foundReadSession {
		t.Errorf("output missing deeper named sub-step:\n%s", out)
	}
	if !foundReadSessionError {
		t.Errorf("output missing error marker on deeper named sub-step:\n%s", out)
	}
}

func TestTraceCmd_InvalidLastFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"zero", []string{"--last", "0"}, "--last must be at least 1, got 0"},
		{"negative", []string{"--last", "-1"}, "--last must be at least 1, got -1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := newTraceCmd()
			cmd.SetArgs(tt.args)
			cmd.SilenceUsage = true

			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestRenderTraceEntries_Empty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	renderTraceEntries(&buf, nil)
	out := buf.String()

	if !strings.Contains(out, "No trace entries found.") {
		t.Errorf("expected 'No trace entries found.' message, got:\n%s", out)
	}
}
