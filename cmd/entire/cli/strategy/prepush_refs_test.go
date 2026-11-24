package strategy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

const (
	testSHA1 = "1111111111111111111111111111111111111111"
	testSHA2 = "2222222222222222222222222222222222222222"
	zeroSHA  = "0000000000000000000000000000000000000000"
)

func TestParsePrePushRefs(t *testing.T) {
	t.Parallel()
	const v1 = "refs/heads/entire/checkpoints/v1"
	refs, err := ParsePrePushRefs(strings.NewReader(
		"refs/heads/main " + testSHA1 + " refs/heads/main " + testSHA2 + "\n" +
			"malformed line\n" +
			"\n" +
			v1 + " " + testSHA1 + " " + v1 + " " + zeroSHA + "\n" +
			"(delete) " + zeroSHA + " refs/entire/checkpoints/ab/cdef " + testSHA2 + "\n" +
			testSHA1 + " " + testSHA1 + " refs/heads/copy " + zeroSHA + "\n" +
			"HEAD " + testSHA2 + " refs/heads/main " + testSHA1 + "\n"))
	require.NoError(t, err)
	require.Equal(t, []PrePushRef{
		{LocalRef: "refs/heads/main", LocalSHA: testSHA1, RemoteRef: "refs/heads/main", RemoteSHA: testSHA2},
		{LocalRef: v1, LocalSHA: testSHA1, RemoteRef: v1, RemoteSHA: zeroSHA},
		{LocalRef: "(delete)", LocalSHA: zeroSHA, RemoteRef: "refs/entire/checkpoints/ab/cdef", RemoteSHA: testSHA2},
		{LocalRef: testSHA1, LocalSHA: testSHA1, RemoteRef: "refs/heads/copy", RemoteSHA: zeroSHA},
		{LocalRef: "HEAD", LocalSHA: testSHA2, RemoteRef: "refs/heads/main", RemoteSHA: testSHA1},
	}, refs, "every line is kept: any ref, branch or tag can point at a waiting checkpoint's commit")
}

// A `--mirror` of a repository with many tags sends a long ref list; the v1
// line at its end must still be seen, not cut off by a size cap.
func TestParsePrePushRefs_LongListKeepsTheLastLine(t *testing.T) {
	t.Parallel()
	const v1 = "refs/heads/entire/checkpoints/v1"
	var b strings.Builder
	for i := 0; b.Len() < 20<<20; i++ {
		fmt.Fprintf(&b, "refs/tags/t%d %s refs/tags/t%d %s\n", i, testSHA1, i, zeroSHA)
	}
	b.WriteString(v1 + " " + testSHA2 + " " + v1 + " " + zeroSHA + "\n")

	refs, err := ParsePrePushRefs(strings.NewReader(b.String()))
	require.NoError(t, err)
	require.NotEmpty(t, refs)
	last := refs[len(refs)-1]
	require.Equal(t, v1, last.LocalRef)
	require.Equal(t, testSHA2, last.LocalSHA)
}

// One line with an oversized ref name must not make the whole list unknown. A
// checkpoint ref on that line still counts, and the lines after it are still
// read. A line whose name was cut short is marked, so nothing acts on that
// name as if it were the real ref.
func TestParsePrePushRefs_OversizedRefName(t *testing.T) {
	t.Parallel()
	const v1 = "refs/heads/entire/checkpoints/v1"
	huge := "refs/heads/" + strings.Repeat("x", 70<<10)
	hugeCheckpoint := checkpoint.CheckpointRefPrefix + strings.Repeat("y", 70<<10)
	for _, tc := range []struct {
		name  string
		input string
		want  PrePushRef
	}{
		{
			name:  "v1 sent to an oversized remote name",
			input: v1 + " " + testSHA1 + " " + huge + " " + zeroSHA + "\n",
			want:  PrePushRef{LocalRef: v1, LocalSHA: testSHA1, RemoteSHA: zeroSHA, Truncated: true},
		},
		{
			name:  "an oversized local name pushed onto v1",
			input: huge + " " + testSHA1 + " " + v1 + " " + zeroSHA + "\n",
			want:  PrePushRef{LocalSHA: testSHA1, RemoteRef: v1, RemoteSHA: zeroSHA, Truncated: true},
		},
		{
			name: "an oversized line with no checkpoint ref, then v1",
			input: huge + " " + testSHA1 + " " + huge + " " + zeroSHA + "\n" +
				v1 + " " + testSHA2 + " " + v1 + " " + zeroSHA + "\n",
			want: PrePushRef{LocalRef: v1, LocalSHA: testSHA2, RemoteRef: v1, RemoteSHA: zeroSHA},
		},
		{
			name:  "an oversized name under the checkpoint ref prefix",
			input: hugeCheckpoint + " " + testSHA1 + " " + hugeCheckpoint + " " + zeroSHA + "\n",
			want:  PrePushRef{LocalSHA: testSHA1, RemoteSHA: zeroSHA, Truncated: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refs, err := ParsePrePushRefs(strings.NewReader(tc.input))
			require.NoError(t, err)
			require.NotEmpty(t, refs)
			// The line under test is the last one: lines before it must not
			// hide it.
			got := refs[len(refs)-1]
			require.Equal(t, tc.want.LocalSHA, got.LocalSHA)
			require.Equal(t, tc.want.RemoteSHA, got.RemoteSHA)
			require.Equal(t, tc.want.Truncated, got.Truncated, "a line is marked when a field was cut short")
			if tc.want.LocalRef != "" {
				require.Equal(t, tc.want.LocalRef, got.LocalRef)
			}
			if tc.want.RemoteRef != "" {
				require.Equal(t, tc.want.RemoteRef, got.RemoteRef)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stdin went away") }

// A ref list that cannot be read leaves the refs unknown, as an old hook does,
// rather than failing the user's push over it.
func TestWithPrePushRefsFrom_UnreadableListIsUnknown(t *testing.T) {
	t.Parallel()
	require.False(t, prePushRefsKnown(WithPrePushRefsFrom(context.Background(), failingReader{})))
	require.True(t, prePushRefsKnown(WithPrePushRefsFrom(context.Background(),
		strings.NewReader("refs/heads/main "+testSHA1+" refs/heads/main "+testSHA2+"\n"))))
}
