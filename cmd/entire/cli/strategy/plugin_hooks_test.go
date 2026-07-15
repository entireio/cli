package strategy

import (
	"strings"
	"testing"
)

func TestInsertCommitTrailers_BeforeCommentBlock(t *testing.T) {
	t.Parallel()
	msg := "my subject\n\nbody line\n\nEntire-Checkpoint: abc123\n# Please enter the commit message\n# with comments\n"
	out := insertCommitTrailers(msg, []string{"Plugin-Trailer: yes"})

	if !strings.Contains(out, "Plugin-Trailer: yes") {
		t.Fatalf("trailer not inserted: %q", out)
	}
	// Trailer must appear before the comment block.
	trailerIdx := strings.Index(out, "Plugin-Trailer: yes")
	commentIdx := strings.Index(out, "# Please enter")
	if trailerIdx == -1 || commentIdx == -1 || trailerIdx > commentIdx {
		t.Fatalf("trailer must precede comment block:\n%s", out)
	}
	// The built-in Entire-Checkpoint trailer must be preserved and precede the
	// plugin trailer (built-in is never displaced).
	entireIdx := strings.Index(out, "Entire-Checkpoint: abc123")
	if entireIdx == -1 || entireIdx > trailerIdx {
		t.Fatalf("Entire-Checkpoint must precede plugin trailer:\n%s", out)
	}
}

func TestInsertCommitTrailers_NoCommentBlock(t *testing.T) {
	t.Parallel()
	msg := "subject\n\nbody\n"
	out := insertCommitTrailers(msg, []string{"A: 1", "B: 2"})
	if !strings.Contains(out, "A: 1") || !strings.Contains(out, "B: 2") {
		t.Fatalf("trailers not appended: %q", out)
	}
	if strings.Index(out, "A: 1") > strings.Index(out, "B: 2") {
		t.Fatalf("trailers out of order: %q", out)
	}
}

func TestInsertCommitTrailers_DropsBlankTrailers(t *testing.T) {
	t.Parallel()
	msg := "subject\n"
	out := insertCommitTrailers(msg, []string{"   ", "\n", "Real: 1"})
	if strings.Count(out, "\n") > 3 {
		t.Fatalf("blank trailers should be dropped: %q", out)
	}
	if !strings.Contains(out, "Real: 1") {
		t.Fatalf("real trailer dropped: %q", out)
	}
}

func TestInsertCommitTrailers_EmptyReturnsUnchanged(t *testing.T) {
	t.Parallel()
	msg := "subject\n"
	if out := insertCommitTrailers(msg, nil); out != msg {
		t.Fatalf("expected unchanged message, got %q", out)
	}
}
