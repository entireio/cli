package githelper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbchunk/bbchunktest"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

func TestArgumentRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, want string
		ok               bool
	}{
		{"in-band refusal", pktLine(`ERR fetchv2: unexpected line: "entire-bb-fetch"`), `fetchv2: unexpected line: "entire-bb-fetch"`, true},
		{"refusal with a newline", pktLine("ERR unexpected entire-bb-fetch\n") + "0000", "unexpected entire-bb-fetch", true},
		{"control bytes are sanitized", pktLine("ERR \x1b]0;entire-bb-fetch"), "^[]0;entire-bb-fetch", true},
		{"another error", pktLine("ERR upload-pack: not our ref"), "", false},
		{"a response", pktLine("acknowledgments\n") + pktLine("NAK\n") + "0000", "", false},
		{"a flush", "0000", "", false},
		{"short", "00", "", false},
		{"truncated packet", pktLine("ERR entire-bb-fetch")[:10], "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := bufio.NewReader(strings.NewReader(tc.body))
			got, ok := argumentRefusal(r)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
			rest, err := io.ReadAll(r)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(rest), "the check consumes nothing")
		})
	}
}

// With the kill switch, the helper sends no X-Entire-BB and no argument. It
// hides entire-bb-fetch from git in every case, as it hides ref-in-want.
func TestHandleStatelessConnect_KillSwitchHidesFetchFeature(t *testing.T) {
	// No t.Parallel(): t.Setenv. The kill switch also keeps the helper away
	// from the cache of the repository that runs the test.
	t.Setenv(helperChunkingEnv, "0")
	advertisement := pktLine("version 2\n") + pktLine("ls-refs=unborn\n") +
		pktLine("fetch=shallow entire-bb-fetch filter\n") + "0000"
	wantAdvertisement := pktLine("version 2\n") + pktLine("ls-refs=unborn\n") +
		pktLine("fetch=shallow filter\n") + "0000"
	request := pktLine("command=fetch\n") + "0001" + pktLine("want "+testHeadSHA+"\n") + pktLine("done\n") + "0000"

	var headers []string
	var post []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			headers = append(headers, r.Header.Get(transport.HeaderBB))
			fmt.Fprint(w, advertisement)
		case http.MethodPost:
			post, _ = io.ReadAll(r.Body) //nolint:errcheck // test
			fmt.Fprint(w, pktLine("packfile\n")+"0000")
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	require.NoError(t, handleStatelessConnect(context.Background(), testTransport(server), serviceUploadPack, strings.NewReader(request), &stdout))
	require.Equal(t, []string{""}, headers, "no X-Entire-BB")
	require.Equal(t, request, string(post), "no argument")
	require.Equal(t, "\n"+wantAdvertisement+pktLine("packfile\n")+"0000"+"0002", stdout.String())
}

// chunkedResponse returns a v2 fetch response whose pack holds one recipe:
// a 100 KiB blob in one xorb. With bandError, the packfile section ends in
// the middle of the pack with that band-3 message.
func chunkedResponse(t *testing.T, bandError string) []byte {
	t.Helper()
	blob := splitMix64Bytes(100<<10, 11)
	var b xorb.Builder
	var nodes []xethash.Node
	require.NoError(t, bbchunk.Split(t.Context(), bytes.NewReader(blob), func(c bbchunk.Chunk) error {
		nodes = append(nodes, xethash.Node{Hash: c.Hash, Size: uint64(len(c.Data))})
		_, err := b.Add(c.Data, xorb.Auto)
		return err
	}))
	x, err := bbchunk.Seal(&b)
	require.NoError(t, err)
	first, last, err := x.Footer.StoredRange(0, len(x.Footer.ChunkHashes))
	require.NoError(t, err)
	oid := sha1.New()
	fmt.Fprintf(oid, "blob %d\x00", len(blob))
	oid.Write(blob)
	entry, err := bbchunktest.AppendChunkedEntry(nil, int64(len(blob)), &bbchunk.ChunkedPayload{
		OID: oid.Sum(nil), FileHash: xethash.File(nodes), Xorbs: []xethash.Hash{x.Hash},
		Terms: []bbchunk.ChunkedTerm{{ChunkEnd: uint32(len(x.Footer.ChunkHashes)), UnpackedBytes: uint64(len(blob)), FirstByte: first, LastByte: last}},
	})
	require.NoError(t, err)
	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, bbchunk.PackVersionExternal)
	pack = binary.BigEndian.AppendUint32(pack, 1)
	pack = append(pack, entry...)
	sum := sha1.Sum(pack)
	pack = append(pack, sum[:]...)

	var resp bytes.Buffer
	resp.WriteString(pktLine("packfile\n"))
	sw := gitproto.NewSidebandWriter(&resp)
	if bandError != "" {
		pack = pack[:len(pack)/2]
	}
	_, err = sw.Write(pack)
	require.NoError(t, err)
	require.NoError(t, sw.Flush())
	if bandError != "" {
		require.NoError(t, sw.Error([]byte(bandError)))
	}
	resp.WriteString("0000")
	return resp.Bytes()
}

// A fetch that fails outside the recipe path records no fallback and sends
// no report: a response that breaks off, a band-3 error of the server, and
// a cancelled fetch. A failed download records the fallback, reports it on
// band 3 once, and returns a short error.
func TestChunkedFetchFallsBackOnlyForRecipeFailures(t *testing.T) {
	// No t.Parallel(): t.Setenv keeps the fallback record in a test cache.
	resp := chunkedResponse(t, "")
	request := pktLine("command=fetch\n") + "0001" + pktLine("want "+testHeadSHA+"\n") + pktLine("done\n") + "0000"
	for _, tc := range []struct {
		name     string
		body     []byte
		cancel   bool
		fallback bool
	}{
		{name: "response breaks off", body: resp[:len(resp)/2]},
		{name: "server error on band 3", body: chunkedResponse(t, "upload-pack: out of memory")},
		{name: "cancelled", body: resp, cancel: true},
		{name: "download fails", body: resp, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tc.cancel {
						cancel()
					}
					http.Error(w, "xorb not found", http.StatusNotFound)
					return
				}
				_, _ = w.Write(tc.body) //nolint:errcheck // test
			}))
			defer server.Close()
			p := testTransport(server)
			store, err := bbcache.Open(t.TempDir(), server.URL, "/et/owner/repo")
			require.NoError(t, err)
			record, err := bbcache.OpenFallbackRecord(server.URL, "/et/owner/repo")
			require.NoError(t, err)
			c := &chunkedFetch{t: p, store: store, client: bbclient.New(p), record: record}

			var stdout bytes.Buffer
			err = c.fetch(ctx, []byte(request), &stdout)
			require.Error(t, err)
			_, recorded := record.Get()
			require.Equal(t, tc.fallback, recorded, "fallback record")
			require.Equal(t, tc.fallback, c.off)
			if !tc.fallback {
				require.NotErrorIs(t, err, errLargeFileFetch)
				require.NotContains(t, stdout.String(), "large file download failed")
				return
			}
			require.ErrorIs(t, err, errLargeFileFetch)
			require.Equal(t, 1, strings.Count(stdout.String(), "HTTP 404"), "the detail goes to git once")
			require.Contains(t, stdout.String(), "Clones and fetches of this repository get whole files for the next 24 hours")
			require.Contains(t, stdout.String(), helperChunkingEnv+"=0")
			require.NotContains(t, err.Error(), "404", "the returned error does not repeat the report")
		})
	}
}
