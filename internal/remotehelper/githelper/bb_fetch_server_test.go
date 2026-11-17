package githelper

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbchunk/bbchunktest"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// Switch values of entiredb's ENTIRE_BB_CLIENT_CHUNKING for the fetch path,
// and "old" for a node that predates the fetch spec.
const (
	fetchOff       = "off"
	fetchAccept    = "accept"
	fetchAdvertise = "advertise"
	fetchOld       = "old"
)

// fakeFetch is the fetch side of fakeBB: entiredb's client-chunked fetch.
// The upload-pack advertisement gets entire-bb-fetch when the switch is
// advertise and the request has X-Entire-BB. A fetch command with the
// argument gets every blob with a recipe as a stored meta 0x02 entry at the
// end of a 0xe002 pack. GET bb/xorbs/<hash> serves permanent xorbs whole.
type fakeFetch struct {
	// advertiseSwitch decides the advertisement, and postSwitch the answer to
	// the argument and to a download. They differ on a cluster that is half
	// way through a rollout.
	advertiseSwitch string
	postSwitch      string
	// recipes holds the recipe of each blob that a client-chunked push
	// stored, by object ID in hex.
	recipes map[string]*clientEntry
	// failGet, when set, gives the status to answer a download with, or 0.
	failGet func(h xethash.Hash) int
	// truncate cuts each response with recipes in the middle of its pack.
	truncate bool

	// bbHeaders counts the upload-pack advertisements requested with
	// X-Entire-BB, and advertised those with entire-bb-fetch.
	bbHeaders  int
	advertised int
	// posts holds each fetch command, and withArg the number that sent the
	// argument.
	posts   []*gitproto.V2Request
	withArg int
	// refused counts the refusals of the argument.
	refused int
	// sentRecipes counts the 0x02 entries sent.
	sentRecipes int
	gets        []xethash.Hash
	getBytes    int64
}

func (f *fakeBB) setFetch(advertiseSwitch, postSwitch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetch.advertiseSwitch, f.fetch.postSwitch = advertiseSwitch, postSwitch
}

func (f *fakeBB) serveUploadAdvertisement(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	f.backend.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	f.mu.Lock()
	header := r.Header.Get(transport.HeaderBB) == "1"
	if header {
		f.fetch.bbHeaders++
	}
	advertise := header && f.fetch.advertiseSwitch == fetchAdvertise && r.Header.Get("Git-Protocol") == "version=2"
	if advertise {
		f.fetch.advertised++
	}
	f.mu.Unlock()
	if advertise && rec.Code == http.StatusOK {
		var err error
		if body, err = addFetchFeature(body, featureBBFetch); err != nil {
			f.t.Errorf("fake BB: upload-pack advertisement: %v", err)
		}
	}
	for k, v := range rec.Header() {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(rec.Code)
	f.write(w, body)
}

// addFetchFeature adds feature to the fetch= line of a v2 advertisement.
func addFetchFeature(body []byte, feature string) ([]byte, error) {
	r := bytes.NewReader(body)
	var out bytes.Buffer
	found := false
	for r.Len() > 0 {
		n, p, err := pktline.ReadLine(r)
		if err != nil {
			return nil, err
		}
		switch {
		case n == pktline.Flush:
			_ = pktline.WriteFlush(&out)
		case bytes.HasPrefix(p, []byte("fetch=")):
			found = true
			pkt(&out, strings.TrimSuffix(string(p), "\n")+" "+feature+"\n")
		default:
			pkt(&out, string(p))
		}
	}
	if !found {
		return nil, errors.New("no fetch= capability")
	}
	return out.Bytes(), nil
}

func (f *fakeBB) serveUploadPack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Git-Protocol") != "version=2" {
		f.forward(w, r, body)
		return
	}
	req, err := gitproto.ParseV2Request(body)
	if err != nil {
		f.t.Errorf("fake BB: upload-pack request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	command := gitproto.V2Command(body)
	withArg := req.HasArgument(featureBBFetch)
	if command != "fetch" {
		if withArg {
			f.t.Errorf("fake BB: %s in a %s request", featureBBFetch, command)
		}
		f.forward(w, r, body)
		return
	}
	f.mu.Lock()
	f.fetch.posts = append(f.fetch.posts, req)
	postSwitch := f.fetch.postSwitch
	if withArg {
		f.fetch.withArg++
		if postSwitch == fetchOff || postSwitch == fetchOld {
			f.fetch.refused++
		}
	}
	f.mu.Unlock()
	if !withArg {
		f.forward(w, r, body)
		return
	}
	switch postSwitch {
	case fetchOld:
		http.Error(w, "fatal: fetchv2: unexpected line", http.StatusInternalServerError)
		return
	case fetchOff:
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		var b bytes.Buffer
		pkt(&b, `ERR fetchv2: unexpected line: "`+featureBBFetch+`"`)
		f.write(w, b.Bytes())
		return
	}
	f.serveRecipes(w, r, req)
}

// forward sends a request with body to git http-backend.
func (f *fakeBB) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	fwd := r.Clone(r.Context())
	fwd.Body = io.NopCloser(bytes.NewReader(body))
	fwd.ContentLength = int64(len(body))
	f.backend.ServeHTTP(w, fwd)
}

// serveRecipes answers a fetch command with the argument. git upload-pack
// answers the request without the argument and without thin-pack, and the
// fake then rewrites the pack: every object whole, and the blobs with a
// recipe as 0x02 entries at the end. The pack version is 0xe002, as for
// every pack of a node that sends recipes.
func (f *fakeBB) serveRecipes(w http.ResponseWriter, r *http.Request, req *gitproto.V2Request) {
	plain := slices.DeleteFunc(slices.Clone(req.Arguments), func(a string) bool {
		return a == featureBBFetch || a == "thin-pack"
	})
	var fwdBody bytes.Buffer
	for _, c := range req.Capabilities {
		pkt(&fwdBody, c+"\n")
	}
	fwdBody.WriteString("0001")
	for _, a := range plain {
		pkt(&fwdBody, a+"\n")
	}
	_ = pktline.WriteFlush(&fwdBody)

	rec := httptest.NewRecorder()
	f.forward(rec, r, fwdBody.Bytes())
	resp := rec.Body.Bytes()
	if rec.Code == http.StatusOK {
		var err error
		if resp, err = f.rewriteFetchResponse(resp, req); err != nil {
			f.t.Errorf("fake BB: rewrite fetch response: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		if f.fetch.truncate {
			resp = resp[:len(resp)-200]
		}
		f.mu.Unlock()
	}
	for k, v := range rec.Header() {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(rec.Code)
	f.write(w, resp)
}

// rewriteFetchResponse replaces the pack of a v2 fetch response. The
// packets before the pack pass as they are.
func (f *fakeBB) rewriteFetchResponse(resp []byte, req *gitproto.V2Request) ([]byte, error) {
	sidebandAll := req.HasArgument("sideband-all")
	fr := gitproto.NewFetchResponseReader(bytes.NewReader(resp), sidebandAll)
	var out, pack bytes.Buffer
	inPack := false
	for {
		p, err := fr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case p.Kind == gitproto.FetchPackData:
			pack.Write(p.Payload)
		case p.Kind == gitproto.FetchFlush && inPack:
			newPack, err := f.rewritePack(pack.Bytes(), req)
			if err != nil {
				return nil, err
			}
			sw := gitproto.NewSidebandWriter(&out)
			if _, err := sw.Write(newPack); err != nil {
				return nil, err
			}
			if err := sw.Flush(); err != nil {
				return nil, err
			}
			out.Write(p.Wire)
		case p.Kind == gitproto.FetchProgress && inPack:
		default:
			out.Write(p.Wire)
		}
		if p.Kind == gitproto.FetchHeader && p.Section == gitproto.SectionPackfile {
			inPack = true
		}
	}
	return out.Bytes(), nil
}

// fakeObject is one object of a pack, with its delta resolved.
type fakeObject struct {
	typ  int
	data []byte
}

func (f *fakeBB) rewritePack(pack []byte, req *gitproto.V2Request) ([]byte, error) {
	newHash := sha1.New
	if format, _ := req.Capability("object-format"); format == "sha256" {
		newHash = sha256.New
	}
	objects, err := readPackObjects(pack, newHash)
	if err != nil {
		return nil, err
	}
	var out, tail bytes.Buffer
	out.WriteString("PACK")
	out.Write(binary.BigEndian.AppendUint32(nil, bbchunk.PackVersionExternal))
	out.Write(binary.BigEndian.AppendUint32(nil, uint32(len(objects))))
	sent := 0
	for _, o := range objects {
		id := objectID(newHash, o)
		if o.typ == 3 {
			f.mu.Lock()
			e, ok := f.fetch.recipes[id]
			f.mu.Unlock()
			if ok {
				entry, err := f.storedEntry(e)
				if err != nil {
					return nil, fmt.Errorf("blob %s: %w", id, err)
				}
				tail.Write(entry)
				sent++
				continue
			}
		}
		out.Write(bbchunk.AppendObjectHeader(nil, byte(o.typ), int64(len(o.data))))
		zw := zlib.NewWriter(&out)
		if _, err := zw.Write(o.data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	out.Write(tail.Bytes())
	sum := newHash()
	sum.Write(out.Bytes())
	out.Write(sum.Sum(nil))
	f.mu.Lock()
	f.fetch.sentRecipes += sent
	f.mu.Unlock()
	return out.Bytes(), nil
}

// storedEntry returns the stored meta 0x02 entry of a pushed recipe, with
// the byte range of each term from the stored footer.
func (f *fakeBB) storedEntry(e *clientEntry) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &bbchunk.ChunkedPayload{OID: e.oid, FileHash: e.fileHash, Xorbs: e.xorbs}
	for _, t := range e.terms {
		x, ok := f.xorbs[e.xorbs[t.Xorb]]
		if !ok || !x.permanent {
			return nil, fmt.Errorf("xorb %s is not permanent", e.xorbs[t.Xorb])
		}
		var first uint32
		if t.ChunkStart > 0 {
			first = x.storedEnds[t.ChunkStart-1]
		}
		c.Terms = append(c.Terms, bbchunk.ChunkedTerm{
			Xorb: uint32(t.Xorb), ChunkStart: uint32(t.ChunkStart), ChunkEnd: uint32(t.ChunkEnd),
			UnpackedBytes: t.UnpackedBytes,
			FirstByte:     uint64(first), LastByte: uint64(x.storedEnds[t.ChunkEnd-1]) - 1,
		})
	}
	return bbchunktest.AppendChunkedEntry(nil, e.size, c)
}

func objectID(newHash func() hash.Hash, o fakeObject) string {
	names := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
	h := newHash()
	fmt.Fprintf(h, "%s %d\x00", names[o.typ], len(o.data))
	h.Write(o.data)
	return hex.EncodeToString(h.Sum(nil))
}

// readPackObjects reads every object of a version-2 pack and resolves its
// deltas. A REF_DELTA base must be in the pack: the fake asks git for packs
// without thin-pack.
func readPackObjects(pack []byte, newHash func() hash.Hash) ([]fakeObject, error) {
	oidSize := newHash().Size()
	if len(pack) < 12+oidSize || string(pack[:4]) != "PACK" || binary.BigEndian.Uint32(pack[4:8]) != 2 {
		return nil, errors.New("not a version 2 pack")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	body := pack[:len(pack)-oidSize]
	objects := make([]fakeObject, count)
	byOffset := map[int]int{}
	byID := map[string]int{}
	off := 12
	for i := range count {
		start := off
		typ, _, n, err := readObjectHeader(body[off:])
		if err != nil {
			return nil, err
		}
		off += n
		base := -1
		switch typ {
		case 6:
			c := body[off]
			rel := int(c & 0x7f)
			for off++; c&0x80 != 0; off++ {
				c = body[off]
				rel = (rel+1)<<7 | int(c&0x7f)
			}
			idx, ok := byOffset[start-rel]
			if !ok {
				return nil, fmt.Errorf("object %d: OFS_DELTA base at %d", i, start-rel)
			}
			base = idx
		case 7:
			idx, ok := byID[hex.EncodeToString(body[off:off+oidSize])]
			if !ok {
				return nil, fmt.Errorf("object %d: REF_DELTA base not in the pack", i)
			}
			base = idx
			off += oidSize
		}
		br := bytes.NewReader(body[off:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		off = len(body) - br.Len()
		if base >= 0 {
			if data, err = applyDelta(objects[base].data, data); err != nil {
				return nil, fmt.Errorf("object %d: %w", i, err)
			}
			typ = objects[base].typ
		}
		objects[i] = fakeObject{typ: typ, data: data}
		byOffset[start] = i
		byID[objectID(newHash, objects[i])] = i
	}
	if off != len(body) {
		return nil, fmt.Errorf("%d bytes after the last object", len(body)-off)
	}
	return objects, nil
}

// applyDelta applies a git delta to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	size := func() (int, error) {
		v, n := binary.Uvarint(delta)
		if n <= 0 {
			return 0, errors.New("bad delta size")
		}
		delta = delta[n:]
		return int(v), nil
	}
	srcSize, err := size()
	if err != nil {
		return nil, err
	}
	dstSize, err := size()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base is %d bytes, want %d", len(base), srcSize)
	}
	out := make([]byte, 0, dstSize)
	for len(delta) > 0 {
		cmd := delta[0]
		delta = delta[1:]
		switch {
		case cmd&0x80 != 0:
			var offset, n int
			for i := range 4 {
				if cmd&(1<<i) != 0 {
					offset |= int(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			for i := range 3 {
				if cmd&(0x10<<i) != 0 {
					n |= int(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			if n == 0 {
				n = 0x10000
			}
			out = append(out, base[offset:offset+n]...)
		case cmd != 0:
			out = append(out, delta[:cmd]...)
			delta = delta[cmd:]
		default:
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta makes %d bytes, want %d", len(out), dstSize)
	}
	return out, nil
}

// serveDownload serves a permanent xorb whole, as entiredb's
// GET bb/xorbs/<hash> does. It never serves a staged xorb.
func (f *fakeBB) serveDownload(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Range") != "" {
		f.t.Errorf("fake BB: download with Range %q", r.Header.Get("Range"))
	}
	f.mu.Lock()
	postSwitch, failGet := f.fetch.postSwitch, f.fetch.failGet
	f.mu.Unlock()
	if postSwitch != fetchAccept && postSwitch != fetchAdvertise {
		http.Error(w, "large objects are not enabled", http.StatusNotFound)
		return
	}
	h, err := parseTextHash(strings.TrimPrefix(r.URL.Path, f.repoPath+"/bb/xorbs/"))
	if err != nil {
		http.Error(w, "bad xorb hash", http.StatusBadRequest)
		return
	}
	if failGet != nil {
		if status := failGet(h); status != 0 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "download refused by the test", status)
			return
		}
	}
	f.mu.Lock()
	x, ok := f.xorbs[h]
	var complete []byte
	if ok && x.permanent {
		footer := &xorb.Footer{XorbHash: h, ChunkHashes: x.hashes, ChunkBoundaryOffsets: x.storedEnds}
		var end uint32
		for _, c := range x.chunks {
			end += uint32(len(c))
			footer.UnpackedChunkOffsets = append(footer.UnpackedChunkOffsets, end)
		}
		complete = xorb.AppendFooter(slices.Clone(x.network), footer)
		f.fetch.gets = append(f.fetch.gets, h)
		f.fetch.getBytes += int64(len(complete))
	}
	f.mu.Unlock()
	if complete == nil {
		http.Error(w, "xorb not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(complete)))
	w.Header().Set("ETag", `"`+h.String()+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	f.write(w, complete)
}

// fetchCounts is a snapshot of the fetch side of the fake.
type fetchCounts struct {
	bbHeaders, advertised, posts, withArg, refused, sentRecipes, gets int
	getBytes                                                          int64
}

func (f *fakeBB) fetchCounts() fetchCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fetchCounts{
		bbHeaders: f.fetch.bbHeaders, advertised: f.fetch.advertised, posts: len(f.fetch.posts),
		withArg: f.fetch.withArg, refused: f.fetch.refused, sentRecipes: f.fetch.sentRecipes,
		gets: len(f.fetch.gets), getBytes: f.fetch.getBytes,
	}
}
