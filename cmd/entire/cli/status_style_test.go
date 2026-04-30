package cli

import (
	"io"
	"strings"
	"testing"
)

func TestMetadataRow_PadsLabelToMin7(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard) // colorEnabled=false, deterministic
	got := s.metadataRow("session", "2026-04-30-c4f1")
	want := "  session  2026-04-30-c4f1\n"
	if got != want {
		t.Errorf("metadataRow short-label padding\n got: %q\nwant: %q", got, want)
	}
}

func TestMetadataRow_PadsLongerLabelsByItself(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.metadataRow("checkpoints", "3")
	// 11-char label fits without padding; layout is "  " + label + "  " + value + "\n".
	want := "  checkpoints  3\n"
	if got != want {
		t.Errorf("metadataRow long-label\n got: %q\nwant: %q", got, want)
	}
}

func TestMetadataRows_AlignsToWidestLabel(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	rows := []explainRow{
		{Label: "session", Value: "abc"},
		{Label: "checkpoints", Value: "3"},
	}
	got := s.metadataRows(rows)
	want := "  session      abc\n  checkpoints  3\n"
	if got != want {
		t.Errorf("metadataRows alignment\n got: %q\nwant: %q", got, want)
	}
}

func TestMetadataRows_EmptyLabelContinuationLine(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	rows := []explainRow{
		{Label: "causes", Value: ""},
		{Label: "", Value: "• alpha"},
		{Label: "", Value: "• beta"},
		{Label: "try", Value: "X"},
	}
	got := s.metadataRows(rows)
	want := "  causes   \n    • alpha\n    • beta\n  try      X\n"
	if got != want {
		t.Errorf("metadataRows continuation\n got: %q\nwant: %q", got, want)
	}
}

func TestIdentityBullet_NoColor(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.identityBullet("Checkpoint", "a3b2c4d5e6f7")
	want := "● Checkpoint a3b2c4d5e6f7\n"
	if got != want {
		t.Errorf("identityBullet no-color\n got: %q\nwant: %q", got, want)
	}
}

func TestIdentityBullet_EmptyIdSkipsTrailingSpace(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.identityBullet("Checkpoint abc [temporary]", "")
	want := "● Checkpoint abc [temporary]\n"
	if got != want {
		t.Errorf("identityBullet empty id\n got: %q\nwant: %q", got, want)
	}
}

func TestListIdentityBullet_NoColor(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.listIdentityBullet("a3b2c4d5e6f7", `[Task]  "refactor"`)
	want := "● a3b2c4d5e6f7  [Task]  \"refactor\"\n"
	if got != want {
		t.Errorf("listIdentityBullet\n got: %q\nwant: %q", got, want)
	}
}

func TestListIdentityBullet_NoSuffix(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.listIdentityBullet("a3b2c4d5e6f7", "")
	want := "● a3b2c4d5e6f7\n"
	if got != want {
		t.Errorf("listIdentityBullet no-suffix\n got: %q\nwant: %q", got, want)
	}
}

func TestSuccessBullet_NoColor(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.successBullet("Summary generated")
	want := "✓ Summary generated\n"
	if got != want {
		t.Errorf("successBullet no-color\n got: %q\nwant: %q", got, want)
	}
}

func TestFailureBullet_NoColor(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.failureBullet("No associated Entire checkpoint")
	want := "✗ No associated Entire checkpoint\n"
	if got != want {
		t.Errorf("failureBullet no-color\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderIdentity_BulletRowsRule(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.renderIdentity("Checkpoint", "abc123",
		[]explainRow{{Label: "session", Value: "s1"}, {Label: "tokens", Value: "1.2k"}},
	)
	if !strings.HasPrefix(got, "● Checkpoint abc123\n") {
		t.Fatalf("missing identity bullet header in:\n%s", got)
	}
	if !strings.Contains(got, "  session  s1\n") {
		t.Fatalf("missing session row in:\n%s", got)
	}
	if !strings.Contains(got, "  tokens   1.2k\n") {
		t.Fatalf("missing tokens row in:\n%s", got)
	}
	if !strings.Contains(got, strings.Repeat("─", 4)) {
		t.Fatalf("missing horizontal rule in:\n%s", got)
	}
}

func TestRenderSuccess_BulletThenRows(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.renderSuccess("Summary generated for abc",
		[]explainRow{{Label: "provider", Value: "claude-code"}},
	)
	want := "✓ Summary generated for abc\n  provider  claude-code\n"
	if got != want {
		t.Errorf("renderSuccess\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderFailure_BulletThenRows(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.renderFailure("Commit not found",
		[]explainRow{{Label: "ref", Value: "deadbeef"}},
	)
	want := "✗ Commit not found\n  ref      deadbeef\n"
	if got != want {
		t.Errorf("renderFailure\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderFailure_NoRows(t *testing.T) {
	t.Parallel()
	s := newStatusStyles(io.Discard)
	got := s.renderFailure("Operation failed", nil)
	want := "✗ Operation failed\n"
	if got != want {
		t.Errorf("renderFailure no-rows\n got: %q\nwant: %q", got, want)
	}
}
