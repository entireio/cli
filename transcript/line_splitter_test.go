package transcript

import (
	"slices"
	"strings"
	"testing"
)

// The splitter hands out each line completed, its newline included, across
// chunk boundaries; holds a trailing partial line for flush; and drops what
// is left once fn asks it to stop.
func TestLineSplitter(t *testing.T) {
	t.Parallel()
	const text = "one\ntwo\n\nthree\nfour"
	for _, size := range []int{1, 2, 5, len(text)} {
		var s lineSplitter
		var got []string
		for i := 0; i < len(text); i += size {
			s.feed([]byte(text[i:min(i+size, len(text))]), func(line []byte) bool {
				got = append(got, string(line))
				return true
			})
		}
		if want := []string{"one\n", "two\n", "\n", "three\n"}; !slices.Equal(got, want) {
			t.Errorf("chunks of %d: lines %q, want %q", size, got, want)
		}
		if tail := s.flush(); string(tail) != "four" {
			t.Errorf("chunks of %d: flush = %q, want %q", size, tail, "four")
		}
		if tail := s.flush(); tail != nil {
			t.Errorf("chunks of %d: a second flush = %q, want nil", size, tail)
		}
	}

	var s lineSplitter
	var got []string
	s.feed([]byte("a\nb\nc"), func(line []byte) bool {
		got = append(got, string(line))
		return false
	})
	if !slices.Equal(got, []string{"a\n"}) {
		t.Errorf("stopped after the first line: lines %q", got)
	}
	if tail := s.flush(); tail != nil {
		t.Errorf("stopped: flush = %q, want nil (the rest is dropped)", tail)
	}
}

// A line longer than lineSplitterKeep that arrived in pieces (a rollout line
// with an inline image) does not pin its buffer for the reader's lifetime; a
// short one keeps its buffer for reuse.
func TestLineSplitterReleasesALongLine(t *testing.T) {
	t.Parallel()
	var s lineSplitter
	long := strings.Repeat("x", 2*lineSplitterKeep)
	s.feed([]byte(long[:len(long)/2]), func([]byte) bool { return true })
	s.feed([]byte(long[len(long)/2:]+"\n"), func([]byte) bool { return true })
	if c := cap(s.pending); c > lineSplitterKeep {
		t.Fatalf("pending keeps %d bytes after the long line was read", c)
	}
	s.feed([]byte("sh"), func([]byte) bool { return true })
	s.feed([]byte("ort\n"), func([]byte) bool { return true })
	if c := cap(s.pending); c == 0 {
		t.Fatal("pending let a short line's buffer go")
	}
}
