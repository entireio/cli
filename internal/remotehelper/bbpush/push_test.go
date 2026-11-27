package bbpush

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// entiredb refuses a push whose entries name more than 4096 xorbs, so the
// push falls back before the post.
func TestXorbNamesLimit(t *testing.T) {
	t.Parallel()
	n := xorbNames{seen: map[xethash.Hash]bool{}}
	hashes := make([]xethash.Hash, maxPushXorbs+1)
	for i := range hashes {
		binary.LittleEndian.PutUint64(hashes[i][:], uint64(i)+1)
	}
	require.NoError(t, n.add(hashes[:maxPushXorbs]))
	require.NoError(t, n.add(hashes[:10]), "a repeated xorb counts once")
	require.ErrorContains(t, n.add(hashes[maxPushXorbs:]), "over the 4096-xorb limit")
	require.Len(t, n.list, maxPushXorbs+1)
}

// The server stores a client entry as meta 0x02, with a byte range for each
// term, under the same payload limit. An entry whose stored form could pass
// the limit falls back.
func TestAppendEntryRefusesAnOversizeStoredForm(t *testing.T) {
	t.Parallel()
	terms := bbchunk.MaxPayloadSize / 16
	r := &bbchunk.Recipe{Size: int64(terms), Xorbs: []xethash.Hash{{1}}}
	for i := range terms {
		start := i % 4000
		r.Terms = append(r.Terms, bbchunk.Term{Xorb: 0, ChunkStart: start, ChunkEnd: start + 1, UnpackedBytes: 1})
	}
	oid := strings.Repeat("ab", 20)
	e, err := bbchunk.AppendEntry(nil, bytes.Repeat([]byte{0xab}, 20), r)
	require.NoError(t, err, "the client entry itself fits")
	require.Less(t, len(e), bbchunk.MaxPayloadSize)
	_, err = appendEntry(oid, r)
	require.ErrorContains(t, err, "stored entry")

	r.Terms = r.Terms[:100]
	r.Size = 100
	_, err = appendEntry(oid, r)
	require.NoError(t, err)
}

// When the report does not arrive after the post, the status lines say
// "expecting report" for each command, as send-pack's do.
func TestFailedReportWritesExpectingReport(t *testing.T) {
	t.Parallel()
	p := &prepared{
		adv:  &advertisement{refs: []advertisedRef{{name: "refs/heads/main", oid: oidA}, {name: "refs/heads/other", oid: oidB}}},
		cmds: []command{{ref: "refs/heads/main", old: oidA, new: oidB}, {ref: "refs/heads/new", old: zero, new: oidB}},
	}
	var stdout bytes.Buffer
	cfg := &Config{Stdout: &stdout, Stderr: io.Discard}
	cause := &reportError{err: io.ErrUnexpectedEOF}
	err := p.failedReport(cfg, cause)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, "error refs/heads/main expecting report\nerror refs/heads/other no match\nerror refs/heads/new expecting report\n\n", stdout.String())

	stdout.Reset()
	postErr := errors.New("posting receive-pack: connection refused")
	require.Equal(t, postErr, p.failedReport(cfg, postErr))
	require.Empty(t, stdout.String(), "no request went out, so no status lines")
}

// Every git process of a push starts through gitCommand, so none of them
// lazily fetches a missing object.
func TestGitProcessesDoNotLazyFetch(t *testing.T) {
	t.Parallel()
	cmd := gitCommand(t.Context(), "version")
	require.Contains(t, cmd.Env, "GIT_NO_LAZY_FETCH=1")

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var sources int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		sources++
		if n := strings.Count(string(b), "exec.Command"); n > 0 {
			require.Equal(t, "git.go", f, "%s starts a process", f)
			require.Equal(t, 1, n, "only gitCommand starts a process")
		}
	}
	require.NotZero(t, sources)
}

// git's own messages go to Config.Stderr, not to the process's stderr.
func TestGitMessagesGoToStderr(t *testing.T) {
	t.Parallel()
	var lines bytes.Buffer
	err := streamLines(t.Context(), []byte("no-such-rev\n"), &lines, func(string) {}, "rev-list", "--stdin")
	require.Error(t, err)
	require.Contains(t, lines.String(), "fatal:")

	var pack bytes.Buffer
	err = appendPack(t.Context(), &gitproto.RequestBody{}, packSpec{revs: []byte("no-such-rev\n"), threshold: 1, stderr: &pack, newHash: newPackHash("sha1")})
	require.Error(t, err)
	require.Contains(t, pack.String(), "fatal:")
}

// Only an *os.File can be a terminal. Progress follows Config.Stderr.
func TestIsTerminal(t *testing.T) {
	t.Parallel()
	require.False(t, isTerminal(&bytes.Buffer{}))
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	defer f.Close()
	require.False(t, isTerminal(f))
}
