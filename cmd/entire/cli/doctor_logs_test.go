package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLastNLines_ShortInput(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("a\nb\nc\n")
	got, err := readLastNLines(r, 5)
	if err != nil {
		t.Fatalf("readLastNLines: %v", err)
	}
	want := []string{"a\n", "b\n", "c\n"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadLastNLines_LongInputTruncated(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("a\nb\nc\nd\ne\nf\n")
	got, err := readLastNLines(r, 3)
	if err != nil {
		t.Fatalf("readLastNLines: %v", err)
	}
	want := []string{"d\n", "e\n", "f\n"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPrintTail_ZeroNCopiesAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	contents := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := printTail(&buf, path, 0); err != nil {
		t.Fatalf("printTail: %v", err)
	}
	if buf.String() != contents {
		t.Errorf("printTail copy = %q, want %q", buf.String(), contents)
	}
}

func TestPrintTail_TailsLastN(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	contents := "1\n2\n3\n4\n5\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := printTail(&buf, path, 2); err != nil {
		t.Fatalf("printTail: %v", err)
	}
	if buf.String() != "4\n5\n" {
		t.Errorf("printTail tail = %q, want \"4\\n5\\n\"", buf.String())
	}
}

func TestFollowFile_ExitsWhenContextCanceled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := followFile(ctx, &buf, path); err != nil {
		t.Fatalf("followFile: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("followFile wrote %q after cancellation", buf.String())
	}
}
