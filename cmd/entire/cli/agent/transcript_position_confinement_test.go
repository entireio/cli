package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/require"
)

// TestGetTranscriptPositionUnderHome_RejectsSwappedSymlink is the same
// validate-once-read-later attack TestReadTranscriptFileUnderHome_RejectsSwappedSymlink
// closes for transcript CONTENT, but for the bare position GetTranscriptPosition
// reports. That count is published into a checkpoint as
// checkpoint_transcript_start, so an unconfined read of a swapped transcript is
// both a newline-count oracle on the victim file (the position leaks len(victim's
// lines)) and an offset-poisoning primitive (a huge reported position suppresses
// every later condensation of the real transcript, since offsets only move
// forward). This test uses the real ClaudeCodeAgent — the exact
// ConfinedTranscriptAnalyzer production call sites dispatch to — not a mock, so
// it exercises the real GetTranscriptPositionUnderHome -> ExtractModifiedFilesFromBytes
// path end to end.
func TestGetTranscriptPositionUnderHome_RejectsSwappedSymlink(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	projectsDir := filepath.Join(home, "projects", "adopt-test")
	require.NoError(t, os.MkdirAll(projectsDir, 0o750))
	transcriptPath := filepath.Join(projectsDir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user"}`+"\n"), 0o600))

	// Validation (session_adopt's recordedHomeAcceptsTranscript) passed against
	// the regular file above. Swap it for a symlink to a victim file with a
	// distinctive line count, as an attacker racing the validate/read window
	// would.
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "id_ed25519")
	require.NoError(t, os.WriteFile(victim, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o600))
	require.NoError(t, os.Remove(transcriptPath))
	if err := os.Symlink(victim, transcriptPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	ag := claudecode.NewClaudeCodeAgent()
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	require.True(t, ok)

	pos, err := agent.GetTranscriptPositionUnderHome(analyzer, transcriptPath, home)
	require.Error(t, err)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	require.Zero(t, pos, "a refused read must not leak the victim's line count through pos on a non-nil error")
}

// TestGetTranscriptPositionUnderHome_ReadsConfinedFile is the positive case: a
// transcript genuinely beneath the recorded home still reports its position
// normally through the confined path.
func TestGetTranscriptPositionUnderHome_ReadsConfinedFile(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	projectsDir := filepath.Join(home, "projects", "adopt-test")
	require.NoError(t, os.MkdirAll(projectsDir, 0o750))
	transcriptPath := filepath.Join(projectsDir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user"}`+"\n"+`{"type":"assistant"}`+"\n"), 0o600))

	ag := claudecode.NewClaudeCodeAgent()
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	require.True(t, ok)

	pos, err := agent.GetTranscriptPositionUnderHome(analyzer, transcriptPath, home)
	require.NoError(t, err)
	require.Equal(t, 2, pos)
}

// TestGetTranscriptPositionUnderHome_EmptyHomeMatchesGetTranscriptPosition pins
// behaviour identical to the plain GetTranscriptPosition for every session
// recorded before AgentHome existed — the empty-home case must not regress.
func TestGetTranscriptPositionUnderHome_EmptyHomeMatchesGetTranscriptPosition(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600))

	ag := claudecode.NewClaudeCodeAgent()
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	require.True(t, ok)

	want, wantErr := analyzer.GetTranscriptPosition(path)
	require.NoError(t, wantErr)

	got, err := agent.GetTranscriptPositionUnderHome(analyzer, path, "")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestGetTranscriptPositionUnderHome_MissingFileReturnsZero matches
// GetTranscriptPosition's documented "returns 0 if the file doesn't exist"
// contract through the confined path too.
func TestGetTranscriptPositionUnderHome_MissingFileReturnsZero(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	missing := filepath.Join(home, "projects", "adopt-test", "gone.jsonl")

	ag := claudecode.NewClaudeCodeAgent()
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	require.True(t, ok)

	pos, err := agent.GetTranscriptPositionUnderHome(analyzer, missing, home)
	require.NoError(t, err)
	require.Zero(t, pos)
}

// TestGetTranscriptPositionUnderHome_ToleratesContentLevelExtractError pins
// the tolerance GetTranscriptPositionUnderHome regressed on: Pi's bare
// GetTranscriptPosition (pijsonl.CountLines) never attempts to parse a
// message, so a corrupt or oversized line was never a reason for it to fail.
// Routing position through ConfinedTranscriptAnalyzer.ExtractModifiedFilesFromBytes
// introduced exactly that failure mode — a bufio.Scanner token-too-long error
// on one oversized line aborts message extraction, and discarding the
// still-valid line count alongside that error turned a tolerated hiccup into
// hasNewTranscriptWork silently reporting no work (no checkpoint, with real
// new content sitting on disk). The transcript below has one ordinary line
// plus one line far past pijsonl.MaxScannerLine (10MB): CountLines reports 2
// regardless, ForEachActiveMessage's scan errors on the second line, and
// GetTranscriptPositionUnderHome must still report 2 — matching exactly what
// the unconfined analyzer.GetTranscriptPosition reports for the same bytes.
func TestGetTranscriptPositionUnderHome_ToleratesContentLevelExtractError(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	transcriptPath := filepath.Join(home, "session.jsonl")
	oversizedLine := `{"type":"message","id":"b","message":{"role":"assistant","content":"` +
		strings.Repeat("a", 11*1024*1024) + `"}}`
	data := `{"type":"message","id":"a","message":{"role":"user","content":"hi"}}` + "\n" +
		oversizedLine + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(data), 0o600))

	ag := pi.NewPiAgent()
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	require.True(t, ok)

	wantPos, wantErr := analyzer.GetTranscriptPosition(transcriptPath)
	require.NoError(t, wantErr)
	require.Equal(t, 2, wantPos, "sanity: the unconfined, never-regressed call must see both lines")

	gotPos, err := agent.GetTranscriptPositionUnderHome(analyzer, transcriptPath, home)
	require.NoError(t, err, "a content-level parse hiccup must not surface as a position error")
	require.Equal(t, wantPos, gotPos, "must match the unconfined call's tolerant count, not silently report 0")
}

// TestGetTranscriptPositionUnderHome_EmptyHomeSkipsConfinedParse proves the
// empty-agentHome branch takes the cheap analyzer.GetTranscriptPosition path
// rather than paying for a full read-and-parse through
// ExtractModifiedFilesFromBytes. A spy analyzer records which method was
// called; ReadTranscriptFileUnderHome's own degrade-to-unconfined-read
// behavior for agentHome == "" would make the two paths value-equal, so
// value equality alone (already pinned by the EmptyHomeMatchesGetTranscriptPosition
// test above) cannot distinguish them — this test checks which method ran.
func TestGetTranscriptPositionUnderHome_EmptyHomeSkipsConfinedParse(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600))

	spy := &positionSpyAnalyzer{
		ClaudeCodeAgent: &claudecode.ClaudeCodeAgent{},
		position:        7,
	}
	pos, err := agent.GetTranscriptPositionUnderHome(spy, path, "")
	require.NoError(t, err)
	require.Equal(t, 7, pos)
	require.True(t, spy.plainCalled, "empty agentHome must call the plain GetTranscriptPosition")
	require.False(t, spy.confinedCalled, "empty agentHome must not pay for ExtractModifiedFilesFromBytes")
}

func TestGetTranscriptPositionUnderHome_SkipsMessageExtraction(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("malformed\n\nunterminated"), 0o600))
	spy := &positionSpyAnalyzer{ClaudeCodeAgent: &claudecode.ClaudeCodeAgent{}}
	pos, err := agent.GetTranscriptPositionUnderHome(spy, path, home)
	require.NoError(t, err)
	require.Equal(t, 3, pos)
	require.False(t, spy.plainCalled)
	require.False(t, spy.confinedCalled)
}

func TestCountTranscriptLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
		want int
	}{
		{"empty", "", 0},
		{"blank", "\n\n", 2},
		{"unterminated", "invalid\nlast", 2},
		{"large_line", strings.Repeat("a", 100*1024) + "\nlast", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pos, err := agent.CountTranscriptLines(iotest.DataErrReader(strings.NewReader(tc.data)))
			require.NoError(t, err)
			require.Equal(t, tc.want, pos)
		})
	}
	pos, err := agent.CountTranscriptLines(iotest.ErrReader(os.ErrPermission))
	require.ErrorIs(t, err, os.ErrPermission)
	require.Zero(t, pos)
}

// positionSpyAnalyzer embeds the real *claudecode.ClaudeCodeAgent (for every
// agent.Agent/TranscriptAnalyzer method this test doesn't care about) and
// overrides only GetTranscriptPosition and ExtractModifiedFilesFromBytes, to
// record which of the two GetTranscriptPositionUnderHome actually calls.
type positionSpyAnalyzer struct {
	*claudecode.ClaudeCodeAgent

	position       int
	plainCalled    bool
	confinedCalled bool
}

func (s *positionSpyAnalyzer) GetTranscriptPosition(string) (int, error) {
	s.plainCalled = true
	return s.position, nil
}

// ExtractModifiedFilesFromBytes always returns nil files: this spy only
// records whether it was called and reports the position, matching the only
// two things GetTranscriptPositionUnderHome's confined branch looks at.
//
//nolint:unparam // files is intentionally always nil, see above
func (s *positionSpyAnalyzer) ExtractModifiedFilesFromBytes([]byte, int) ([]string, int, error) {
	s.confinedCalled = true
	return nil, s.position, nil
}
