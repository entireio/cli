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
			"(delete) " + zeroSHA + " refs/entire/checkpoints/ab/cdef " + testSHA2 + "\n"))
	require.NoError(t, err)
	require.Equal(t, []PrePushRef{
		{LocalRef: v1, LocalSHA: testSHA1, RemoteRef: v1, RemoteSHA: zeroSHA},
		{LocalRef: "(delete)", LocalSHA: zeroSHA, RemoteRef: "refs/entire/checkpoints/ab/cdef", RemoteSHA: testSHA2},
	}, refs, "only lines that touch checkpoint refs are kept")
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
	require.Len(t, refs, 1)
	require.Equal(t, testSHA2, refs[0].LocalSHA)
}

// One line with an oversized ref name must not make the whole list unknown. A
// checkpoint ref on that line still counts, and the lines after it are still
// read.
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
			want:  PrePushRef{LocalRef: v1, LocalSHA: testSHA1, RemoteSHA: zeroSHA},
		},
		{
			name:  "an oversized local name pushed onto v1",
			input: huge + " " + testSHA1 + " " + v1 + " " + zeroSHA + "\n",
			want:  PrePushRef{LocalSHA: testSHA1, RemoteRef: v1, RemoteSHA: zeroSHA},
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
			want:  PrePushRef{LocalSHA: testSHA1, RemoteSHA: zeroSHA},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refs, err := ParsePrePushRefs(strings.NewReader(tc.input))
			require.NoError(t, err)
			require.Len(t, refs, 1)
			got := refs[0]
			require.Equal(t, tc.want.LocalSHA, got.LocalSHA)
			require.Equal(t, tc.want.RemoteSHA, got.RemoteSHA)
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

// prePushRefsRecorded reports whether ctx carries a ref list, empty or not.
func prePushRefsRecorded(ctx context.Context) bool {
	_, ok := ctx.Value(prePushRefsKey{}).([]PrePushRef)
	return ok
}

// A ref list that cannot be read leaves the refs unknown, as an old hook does,
// rather than failing the user's push over it.
func TestWithPrePushRefsFrom_UnreadableListIsUnknown(t *testing.T) {
	t.Parallel()
	require.False(t, prePushRefsRecorded(WithPrePushRefsFrom(context.Background(), failingReader{})))
	require.True(t, prePushRefsRecorded(WithPrePushRefsFrom(context.Background(),
		strings.NewReader("refs/heads/main "+testSHA1+" refs/heads/main "+testSHA2+"\n"))))
}
