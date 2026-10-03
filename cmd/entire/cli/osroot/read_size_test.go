package osroot

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadAllWithSize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content string
		size          int64
	}{
		{"unchanged", "transcript", 10},
		{"grown", "transcript grew after stat", 10},
		{"shrunk", "small", 10},
		{"empty", "", 0},
		{"grown from empty", "new content", 0},
		{"unknown size", "transcript", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := readAllWithSize(iotest.OneByteReader(strings.NewReader(tc.content)), tc.size)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.content {
				t.Fatalf("got %q, want %q", got, tc.content)
			}
		})
	}
}

func TestReadAllWithSize_PropagatesReadErrors(t *testing.T) {
	t.Parallel()
	readErr := errors.New("failed read")
	for _, size := range []int64{0, 10} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			t.Parallel()
			_, err := readAllWithSize(iotest.ErrReader(readErr), size)
			if !errors.Is(err, readErr) {
				t.Fatalf("got %v, want %v", err, readErr)
			}
		})
	}
}
