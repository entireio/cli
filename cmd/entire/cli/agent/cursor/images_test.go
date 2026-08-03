package cursor

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes returns a minimal byte slice with a valid PNG magic header, padded so
// it is unambiguously an image.
func pngBytes(payload string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), []byte(payload)...)
}

func jpegBytes(payload string) []byte {
	return append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(payload)...)
}

// webpBytes returns a minimal RIFF/WEBP container (RIFF....WEBP) padded past the
// header so the store query's magic-byte filter matches it.
func webpBytes(payload string) []byte {
	return append([]byte("RIFF____WEBP"), []byte(payload)...)
}

// buildStoreDB writes a Cursor-style store.db at path with a blobs(id, data)
// table populated from the given blobs. It shells out to sqlite3 (the same
// binary the code under test uses).
func buildStoreDB(t *testing.T, path string, blobs map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var sb strings.Builder
	sb.WriteString("CREATE TABLE blobs(id TEXT PRIMARY KEY, data BLOB);\n")
	for id, data := range blobs {
		sb.WriteString("INSERT INTO blobs(id,data) VALUES('" + id + "', x'" + hex.EncodeToString(data) + "');\n")
	}
	cmd := exec.CommandContext(context.Background(), "sqlite3", path, sb.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build store.db: %v: %s", err, out)
	}
}

// setupChatsDir creates <chats>/<workspace>/<sessionID>/store.db and points the
// test override env at <chats>. Returns the transcript path whose base name is
// the session id.
func setupChatsDir(t *testing.T, sessionID string, blobs map[string][]byte) string {
	t.Helper()
	chats := t.TempDir()
	dbPath := filepath.Join(chats, "workspace-hash", sessionID, "store.db")
	buildStoreDB(t, dbPath, blobs)
	t.Setenv(cursorChatsDirEnv, chats)
	// Transcript path can be anywhere; only its base name (the session id) matters.
	return filepath.Join(t.TempDir(), sessionID+".jsonl")
}

func requireSqlite3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed; skipping cursor store.db test")
	}
}

func TestSidecarImages_CapturesImageBlobs(t *testing.T) {
	requireSqlite3(t)

	img := pngBytes("cursor-sidecar-image-payload-aaaaaaaaaaaaaaaaaaaa")
	transcriptPath := setupChatsDir(t, "sess-img", map[string][]byte{
		"img1": img,
		"txt1": []byte("this is just some message text, not an image at all"),
	})

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 image asset, got %d", len(assets))
	}
	if assets[0].MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png", assets[0].MediaType)
	}
	if string(assets[0].Data) != string(img) {
		t.Error("captured bytes do not match the stored image blob")
	}
	if !strings.HasPrefix(assets[0].Name, "img-") || !strings.HasSuffix(assets[0].Name, ".png") {
		t.Errorf("asset name %q is not img-<hash>.png", assets[0].Name)
	}
}

func TestSidecarImages_MixedImageTypes(t *testing.T) {
	requireSqlite3(t)

	transcriptPath := setupChatsDir(t, "sess-mixed", map[string][]byte{
		"a": pngBytes(strings.Repeat("p", 40)),
		"b": jpegBytes(strings.Repeat("j", 40)),
		"c": []byte("not an image"),
	})

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 image assets, got %d", len(assets))
	}
	types := map[string]bool{}
	for _, a := range assets {
		types[a.MediaType] = true
	}
	if !types["image/png"] || !types["image/jpeg"] {
		t.Errorf("expected png and jpeg, got %v", types)
	}
}

func TestSidecarImages_CapturesWebp(t *testing.T) {
	requireSqlite3(t)

	img := webpBytes(strings.Repeat("w", 40))
	transcriptPath := setupChatsDir(t, "sess-webp", map[string][]byte{"w1": img})

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 webp asset (end-to-end through the SQL magic filter), got %d", len(assets))
	}
	if assets[0].MediaType != "image/webp" || !strings.HasSuffix(assets[0].Name, ".webp") {
		t.Errorf("got %q / %q, want image/webp / *.webp", assets[0].MediaType, assets[0].Name)
	}
}

// A store whose schema is not the expected blobs(data) shape (a future/older
// Cursor version) must be a silent no-op, not an error that would log a warning
// on every checkpoint.
func TestSidecarImages_UnknownSchemaIsNoOp(t *testing.T) {
	requireSqlite3(t)

	chats := t.TempDir()
	dbPath := filepath.Join(chats, "workspace-hash", "sess-schema", "store.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No `blobs` table at all — a different schema shape.
	cmd := exec.CommandContext(context.Background(), "sqlite3", dbPath,
		"CREATE TABLE messages(id TEXT, body TEXT); INSERT INTO messages VALUES('a','hi');")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build store.db: %v: %s", err, out)
	}
	t.Setenv(cursorChatsDirEnv, chats)
	transcriptPath := filepath.Join(t.TempDir(), "sess-schema.jsonl")

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("unexpected error for unrecognized schema (should be a silent no-op): %v", err)
	}
	if len(assets) != 0 {
		t.Fatalf("expected no assets from an unrecognized schema, got %d", len(assets))
	}
}

func TestSidecarImages_DedupsIdenticalImages(t *testing.T) {
	requireSqlite3(t)

	img := pngBytes(strings.Repeat("dedup", 20))
	transcriptPath := setupChatsDir(t, "sess-dup", map[string][]byte{
		"one": img,
		"two": img, // identical content under a different blob id
	})

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected identical images deduped to 1, got %d", len(assets))
	}
}

func TestSidecarImages_TextOnlyStoreReturnsNothing(t *testing.T) {
	requireSqlite3(t)

	transcriptPath := setupChatsDir(t, "sess-text", map[string][]byte{
		"m1": []byte("first message"),
		"m2": []byte("second message"),
	})

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if len(assets) != 0 {
		t.Fatalf("expected no assets from a text-only store, got %d", len(assets))
	}
}

func TestSidecarImages_NoStoreDBIsNoOp(t *testing.T) {
	// Point at an empty chats dir: no store.db for any session.
	t.Setenv(cursorChatsDirEnv, t.TempDir())
	transcriptPath := filepath.Join(t.TempDir(), "missing-session.jsonl")

	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), transcriptPath)
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if assets != nil {
		t.Fatalf("expected nil assets when no store.db exists, got %d", len(assets))
	}
}

func TestSidecarImages_EmptySessionRefIsNoOp(t *testing.T) {
	assets, err := (&CursorAgent{}).SidecarImages(context.Background(), "")
	if err != nil {
		t.Fatalf("SidecarImages: %v", err)
	}
	if assets != nil {
		t.Fatal("expected nil assets for empty session ref")
	}
}

func TestSessionIDFromTranscriptPath(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/home/u/.cursor/projects/p/agent-transcripts/abc-123.jsonl":         "abc-123",
		"/home/u/.cursor/projects/p/agent-transcripts/abc-123/abc-123.jsonl": "abc-123",
		"":           "",
		"bare.jsonl": "bare",
	}
	for in, want := range cases {
		if got := sessionIDFromTranscriptPath(in); got != want {
			t.Errorf("sessionIDFromTranscriptPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectImageType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		data      []byte
		mediaType string
		ext       string
	}{
		{"png", pngBytes("x"), "image/png", "png"},
		{"jpeg", jpegBytes("x"), "image/jpeg", "jpg"},
		{"gif89", []byte("GIF89a...."), "image/gif", "gif"},
		{"gif87", []byte("GIF87a...."), "image/gif", "gif"},
		{"webp", append([]byte("RIFF____WEBP"), []byte("data")...), "image/webp", "webp"},
		{"text", []byte("hello world not an image"), "", ""},
		{"tooShort", []byte{0x89, 0x50}, "", ""},
	}
	for _, tc := range cases {
		mt, ext := detectImageType(tc.data)
		if mt != tc.mediaType || ext != tc.ext {
			t.Errorf("%s: detectImageType = (%q,%q), want (%q,%q)", tc.name, mt, ext, tc.mediaType, tc.ext)
		}
	}
}
