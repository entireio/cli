package goose

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestIdentity(t *testing.T) {
	t.Parallel()

	a := NewGooseAgent()
	if got := a.Name(); got != agent.AgentNameGoose {
		t.Errorf("Name() = %q, want %q", got, agent.AgentNameGoose)
	}
	if got := a.Type(); got != agent.AgentTypeGoose {
		t.Errorf("Type() = %q, want %q", got, agent.AgentTypeGoose)
	}
	if a.Description() == "" {
		t.Error("Description() must not be empty")
	}
	if dirs := a.ProtectedDirs(); len(dirs) != 1 || dirs[0] != ".agents" {
		t.Errorf("ProtectedDirs() = %v, want [.agents]", dirs)
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	t.Parallel()

	got, err := agent.Get(agent.AgentNameGoose)
	if err != nil {
		t.Fatalf("agent.Get(goose): %v", err)
	}
	if got.Name() != agent.AgentNameGoose {
		t.Errorf("registry returned %q", got.Name())
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}

	// --session-id requires --resume, so both must always be present.
	got := a.FormatResumeCommand("20260819_1")
	if !strings.Contains(got, "--resume") || !strings.Contains(got, "--session-id 20260819_1") {
		t.Errorf("FormatResumeCommand = %q, want it to pair --resume with --session-id", got)
	}

	if got := a.FormatResumeCommand("  "); got != "goose session --resume" {
		t.Errorf("blank session ID = %q, want bare resume", got)
	}
}

func TestDetectPresence(t *testing.T) {
	// Not parallel: the subtests use t.Chdir, which is process-global.
	a := &GooseAgent{}

	t.Run("absent", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		found, err := a.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence: %v", err)
		}
		if found {
			t.Error("expected no Goose presence in an empty dir")
		}
	})

	t.Run("present", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.MkdirAll(filepath.Join(dir, pluginsDirName), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		found, err := a.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence: %v", err)
		}
		if !found {
			t.Error("expected Goose presence when .agents/plugins exists")
		}
	})
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	got := a.ResolveSessionFile("/cache", "20260819_1")
	want := filepath.Join("/cache", "20260819_1.json")
	if got != want {
		t.Errorf("ResolveSessionFile = %q, want %q", got, want)
	}
}

func TestGetSessionDir_TestOverride(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	a := &GooseAgent{}
	t.Setenv("ENTIRE_TEST_GOOSE_PROJECT_DIR", "/custom/dir")
	got, err := a.GetSessionDir("/repo")
	if err != nil {
		t.Fatalf("GetSessionDir: %v", err)
	}
	if got != "/custom/dir" {
		t.Errorf("GetSessionDir = %q, want the override", got)
	}
}

func TestReadSession(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}

	t.Run("missing ref", func(t *testing.T) {
		t.Parallel()
		if _, err := a.ReadSession(&agent.HookInput{}); err == nil {
			t.Error("expected an error when no session ref is provided")
		}
	})

	t.Run("reads native data and files", func(t *testing.T) {
		t.Parallel()
		path := writeTranscript(t)
		session, err := a.ReadSession(&agent.HookInput{SessionID: "20260819_1", SessionRef: path})
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if session.AgentName != agent.AgentNameGoose {
			t.Errorf("AgentName = %q", session.AgentName)
		}
		if len(session.NativeData) == 0 {
			t.Error("NativeData is empty")
		}
		if len(session.ModifiedFiles) != 2 {
			t.Errorf("ModifiedFiles = %v, want 2 entries", session.ModifiedFiles)
		}
	})
}

func TestWriteSession(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}

	t.Run("rejects nil and empty", func(t *testing.T) {
		t.Parallel()
		if err := a.WriteSession(context.Background(), nil); err == nil {
			t.Error("expected an error for a nil session")
		}
		if err := a.WriteSession(context.Background(), &agent.AgentSession{}); err == nil {
			t.Error("expected an error for empty session data")
		}
	})

	t.Run("imports via the goose CLI", func(t *testing.T) {
		// Not parallel: swaps a package-level indirection point.
		var gotPath string
		original := runGooseImportFn
		runGooseImportFn = func(_ context.Context, path string) error {
			gotPath = path
			return nil
		}
		defer func() { runGooseImportFn = original }()

		err := a.WriteSession(context.Background(), &agent.AgentSession{
			SessionID:  "20260819_1",
			NativeData: []byte(sampleExport),
		})
		if err != nil {
			t.Fatalf("WriteSession: %v", err)
		}
		if gotPath == "" {
			t.Fatal("expected goose session import to be invoked")
		}
		// The temp file is cleaned up after the import returns.
		if _, statErr := os.Stat(gotPath); !os.IsNotExist(statErr) {
			t.Error("expected the temp export file to be removed after import")
		}
	})

	t.Run("propagates import failure", func(t *testing.T) {
		original := runGooseImportFn
		runGooseImportFn = func(_ context.Context, _ string) error {
			return errors.New("boom")
		}
		defer func() { runGooseImportFn = original }()

		err := a.WriteSession(context.Background(), &agent.AgentSession{
			SessionID:  "s",
			NativeData: []byte(sampleExport),
		})
		if err == nil {
			t.Error("expected the import failure to propagate")
		}
	})
}

func TestChunkAndReassemble_RoundTrip(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	ctx := context.Background()

	data, err := os.ReadFile(realExportFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// A size small enough to force several chunks.
	chunks, err := a.ChunkTranscript(ctx, data, 2000)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected the fixture to split into multiple chunks, got %d", len(chunks))
	}

	merged, err := a.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript: %v", err)
	}

	before, err := CountMessages(data)
	if err != nil {
		t.Fatalf("CountMessages(before): %v", err)
	}
	after, err := CountMessages(merged)
	if err != nil {
		t.Fatalf("CountMessages(after): %v", err)
	}
	if before != after {
		t.Errorf("round-trip changed message count: %d -> %d", before, after)
	}

	// Every top-level key must survive the round trip, including ones this
	// package does not model.
	var origFields, mergedFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &origFields); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(merged, &mergedFields); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	for key := range origFields {
		if _, ok := mergedFields[key]; !ok {
			t.Errorf("top-level key %q was lost in the chunk/reassemble round trip", key)
		}
	}
}

func TestChunkTranscript_EmptyConversationIsSingleChunk(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	content := []byte(`{"id":"s","conversation":[]}`)
	chunks, err := a.ChunkTranscript(context.Background(), content, 10)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) != 1 || string(chunks[0]) != string(content) {
		t.Errorf("expected the input returned unchanged, got %d chunks", len(chunks))
	}
}

func TestReassembleTranscript_NoChunks(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	if _, err := a.ReassembleTranscript(nil); err == nil {
		t.Error("expected an error when reassembling zero chunks")
	}
}

func TestReadTranscript_MissingFile(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	if _, err := a.ReadTranscript(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected an error reading a missing transcript")
	}
}
