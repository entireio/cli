package strategy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
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

func TestCheckOuterPushV1(t *testing.T) {
	t.Parallel()
	const v1 = "refs/heads/entire/checkpoints/v1"
	verified := plumbing.NewHash(testSHA1)
	for _, tc := range []struct {
		name     string
		refs     []PrePushRef
		verified plumbing.Hash
		wantErr  bool
	}{
		{name: "no ref list (old hook script)", refs: nil, verified: plumbing.ZeroHash},
		{name: "push without v1", refs: []PrePushRef{{"refs/heads/main", testSHA2, "refs/heads/main", zeroSHA}}, verified: plumbing.ZeroHash},
		{name: "v1 at the verified tip", refs: []PrePushRef{{v1, testSHA1, v1, zeroSHA}}, verified: verified},
		{name: "v1 at a pre-rewrite commit", refs: []PrePushRef{{v1, testSHA2, v1, zeroSHA}}, verified: verified, wantErr: true},
		{name: "v1 while held for the scan", refs: []PrePushRef{{v1, testSHA1, v1, zeroSHA}}, verified: plumbing.ZeroHash, wantErr: true},
		{name: "v1 pushed under another name", refs: []PrePushRef{{v1, testSHA2, "refs/heads/backup", zeroSHA}}, verified: verified, wantErr: true},
		{name: "another branch pushed onto v1", refs: []PrePushRef{{"refs/heads/main", testSHA2, v1, zeroSHA}}, verified: verified, wantErr: true},
		{name: "v1 the remote already has", refs: []PrePushRef{{v1, testSHA2, v1, testSHA2}}, verified: verified},
		{name: "deleting remote v1", refs: []PrePushRef{{"(delete)", zeroSHA, v1, zeroSHA}}, verified: plumbing.ZeroHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.refs != nil {
				ctx = WithPrePushRefs(ctx, tc.refs)
			}
			err := checkOuterPushV1(ctx, tc.verified)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrOuterPushCarriesUnverifiedV1)
			} else {
				require.NoError(t, err)
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
