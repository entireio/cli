package review_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review"
)

func TestReadSingleKey_EmptyReaderReturnsDefault(t *testing.T) {
	t.Parallel()
	got, err := review.ReadSingleKey(strings.NewReader(""), review.KeyChoice{
		Default: 'Y', Allowed: "YsnA",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 'Y' {
		t.Errorf("got %q, want Y (default on EOF)", got)
	}
}

func TestReadSingleKey_NilReaderReturnsDefault(t *testing.T) {
	t.Parallel()
	got, err := review.ReadSingleKey(nil, review.KeyChoice{
		Default: 'N', Allowed: "YN",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 'N' {
		t.Errorf("got %q, want N", got)
	}
}

func TestReadSingleKey_UnknownInputRePrompts(t *testing.T) {
	t.Parallel()
	got, err := review.ReadSingleKey(strings.NewReader("xY"), review.KeyChoice{
		Default: 'N', Allowed: "YN",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 'Y' {
		t.Errorf("got %q, want Y", got)
	}
}

func TestReadSingleKey_NormalizesCase(t *testing.T) {
	t.Parallel()
	got, err := review.ReadSingleKey(strings.NewReader("a"), review.KeyChoice{
		Default: 'N', Allowed: "YnA",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 'A' {
		t.Errorf("got %q, want A (case-normalized from 'a')", got)
	}
}

func TestReadSingleKey_PlainEnterReturnsDefault(t *testing.T) {
	t.Parallel()
	got, err := review.ReadSingleKey(strings.NewReader("\n"), review.KeyChoice{
		Default: 'Y', Allowed: "YsnA",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 'Y' {
		t.Errorf("got %q, want Y", got)
	}
}

func TestReadSingleKey_SkipsLeadingWhitespace(t *testing.T) {
	t.Parallel()
	got, err := review.ReadSingleKey(strings.NewReader("\t  s"), review.KeyChoice{
		Default: 'Y', Allowed: "YsnA",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 's' {
		t.Errorf("got %q, want s", got)
	}
}
