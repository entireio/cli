package cli

import (
	"io"
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
