package githelper

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// Limits of entiredb's client-chunked push endpoints.
const (
	fakeMaxDedupEntries = 4096
	fakeMaxRecipe       = 1 << 20
	fakeMaxObjectSize   = 1 << 30
	fakeMaxXorbs        = 4096
)

// fakeBB wraps `git http-backend` with entiredb's client-chunked push
// endpoints. It advertises entire-bb to a client that asks for it, keeps
// uploaded xorbs in memory, answers dedup queries, and turns each meta 0x03
// entry of a pushed pack back into a whole blob before git receive-pack sees
// the pack. So real git checks the objects and the ref updates of every
// push. It refuses what entiredb refuses.
type fakeBB struct {
	t         *testing.T
	backend   http.Handler
	repoPath  string
	threshold int64
	// advertise adds entire-bb to the receive-pack advertisement of a
	// request with the X-Entire-BB header.
	advertise bool

	mu sync.Mutex
	// xorbs holds the uploaded xorbs. A push that names a xorb makes it
	// permanent.
	xorbs map[xethash.Hash]*fakeXorb
	// failPut, when set, gives the status to answer an upload with, or 0.
	failPut func(h xethash.Hash) int
	// putDone, when set, runs after the answer to each upload that the
	// server took.
	putDone func(h xethash.Hash)
	// dropPushes makes the next chunked pushes lose their first xorb and
	// refuse with "missing xorb".
	dropPushes int
	// failReceive answers receive-pack with this status when set.
	failReceive int
	// footerCap bounds the footers of one dedup answer, as entiredb does
	// with 64.
	footerCap int

	puts           []xethash.Hash
	putBytes       int64
	receives       int
	dedups         int
	chunkedEntries int
	// bbAdvertised counts the advertisements with entire-bb.
	bbAdvertised int
	// bbReceives counts the receive-pack requests that echo entire-bb.
	bbReceives int
}

type fakeXorb struct {
	chunks    [][]byte
	footer    []map[string]any
	hashes    []xethash.Hash
	permanent bool
}

func newFakeBB(t *testing.T, backend http.Handler, advertise bool) *fakeBB {
	t.Helper()
	return &fakeBB{
		t:         t,
		backend:   backend,
		repoPath:  "/et/owner/repo",
		threshold: 64 << 10,
		footerCap: 64,
		advertise: advertise,
		xorbs:     map[xethash.Hash]*fakeXorb{},
	}
}

func (f *fakeBB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == serviceReceivePack:
		f.serveAdvertisement(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, f.repoPath+"/bb/xorbs/"):
		f.servePut(w, r)
	case r.Method == http.MethodPost && r.URL.Path == f.repoPath+"/bb/dedup":
		f.serveDedup(w, r)
	case r.Method == http.MethodPost && r.URL.Path == f.repoPath+"/"+serviceReceivePack:
		f.serveReceive(w, r)
	default:
		f.backend.ServeHTTP(w, r)
	}
}

func (f *fakeBB) serveAdvertisement(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	f.backend.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	if f.advertise && rec.Code == http.StatusOK && r.Header.Get(transport.HeaderBB) == "1" {
		var err error
		if body, err = addCapability(body, "entire-bb="+strconv.FormatInt(f.threshold, 10)); err != nil {
			f.t.Errorf("fake BB: advertisement: %v", err)
		}
		f.mu.Lock()
		f.bbAdvertised++
		f.mu.Unlock()
	}
	for k, v := range rec.Header() {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(rec.Code)
	f.write(w, body)
}

// write sends the end of a response. A failure is the client's: it closed
// the connection.
func (f *fakeBB) write(w io.Writer, b []byte) {
	if _, err := w.Write(b); err != nil {
		f.t.Logf("fake BB: write response: %v", err)
	}
}

// pkt appends s to b as a pkt-line.
func pkt(b *bytes.Buffer, s string) {
	if _, err := pktline.WriteString(b, s); err != nil {
		panic(err)
	}
}

// addCapability adds capability to the first ref line of a smart-HTTP
// receive-pack advertisement.
func addCapability(body []byte, capability string) ([]byte, error) {
	r := bytes.NewReader(body)
	var out bytes.Buffer
	for {
		n, p, err := pktline.ReadLine(r)
		if err != nil {
			return nil, err
		}
		if n == pktline.Flush {
			_ = pktline.WriteFlush(&out)
			continue
		}
		if i := bytes.IndexByte(p, 0); i >= 0 {
			pkt(&out, strings.TrimSuffix(string(p), "\n")+" "+capability+"\n")
			out.Write(body[len(body)-r.Len():])
			return out.Bytes(), nil
		}
		pkt(&out, string(p))
	}
}

// parseTextHash parses a Xet hash in its canonical text form.
func parseTextHash(s string) (xethash.Hash, error) {
	h, err := xethash.Parse(s)
	if err != nil || h.String() != s {
		return xethash.Hash{}, fmt.Errorf("bad hash %q", s)
	}
	return h, nil
}

func (f *fakeBB) servePut(w http.ResponseWriter, r *http.Request) {
	h, err := parseTextHash(strings.TrimPrefix(r.URL.Path, f.repoPath+"/bb/xorbs/"))
	if err != nil {
		http.Error(w, "bad xorb hash", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, xorb.MaxNetworkSize+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > xorb.MaxNetworkSize {
		http.Error(w, "xorb too large", http.StatusRequestEntityTooLarge)
		return
	}
	f.mu.Lock()
	failPut := f.failPut
	f.mu.Unlock()
	if failPut != nil {
		if status := failPut(h); status != 0 {
			// Retry-After 0 keeps the client's retries free of sleeps.
			w.Header().Set("Retry-After", "0")
			http.Error(w, "upload refused by the test", status)
			return
		}
	}
	in, err := xorb.IngestNetwork(body, h)
	if err != nil {
		http.Error(w, "xorb hash mismatch", http.StatusUnprocessableEntity)
		return
	}
	obj, err := xorb.Decode(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	x := &fakeXorb{}
	for i, c := range in.Chunks {
		x.chunks = append(x.chunks, obj.Chunks[i].Data)
		x.hashes = append(x.hashes, c.Hash)
		x.footer = append(x.footer, map[string]any{
			"hash":         c.Hash.String(),
			"unpacked_end": c.UncompressedOffset + c.UncompressedLength,
			"stored_end":   c.SerializedOffset + c.SerializedLength,
		})
	}
	f.mu.Lock()
	f.puts = append(f.puts, h)
	f.putBytes += int64(len(body))
	status, state := http.StatusCreated, "staged"
	if old, ok := f.xorbs[h]; ok && old.permanent {
		status, state = http.StatusOK, "permanent"
	} else {
		f.xorbs[h] = x
	}
	putDone := f.putDone
	f.mu.Unlock()
	writeTestJSON(w, status, map[string]any{"xorb": h.String(), "state": state, "chunks": x.footer})
	if putDone != nil {
		putDone(h)
	}
}

func parseTextHashes(ss []string) ([]xethash.Hash, error) {
	out := make([]xethash.Hash, len(ss))
	for i, s := range ss {
		var err error
		if out[i], err = parseTextHash(s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (f *fakeBB) serveDedup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Anchors []string `json:"anchors"`
		Xorbs   []string `json:"xorbs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Anchors) > fakeMaxDedupEntries || len(req.Xorbs) > fakeMaxDedupEntries {
		http.Error(w, "too many entries", http.StatusRequestEntityTooLarge)
		return
	}
	anchors, err := parseTextHashes(req.Anchors)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	xorbs, err := parseTextHashes(req.Xorbs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dedups++
	hits := []any{}
	footers := map[string]any{}
	truncated := false
	for _, a := range anchors {
		if !bbchunk.IsAnchor(a) {
			continue
		}
		h, i, ok := f.findAnchor(a)
		if !ok {
			continue
		}
		if _, listed := footers[h.String()]; !listed && len(footers) == f.footerCap {
			truncated = true
			continue
		}
		hits = append(hits, map[string]any{"chunk": a.String(), "xorb": h.String(), "index": i})
		footers[h.String()] = map[string]any{"chunks": f.xorbs[h].footer}
	}
	states := []any{}
	for _, h := range xorbs {
		state := "missing"
		if x, ok := f.xorbs[h]; ok {
			state = "staged"
			if x.permanent {
				state = "permanent"
			}
		}
		states = append(states, map[string]any{"xorb": h.String(), "state": state})
	}
	resp := map[string]any{"anchors": hits, "xorbs": states, "footers": footers}
	if truncated {
		resp["truncated"] = true
	}
	writeTestJSON(w, http.StatusOK, resp)
}

// findAnchor returns the permanent xorb that holds chunk, and its index
// there.
func (f *fakeBB) findAnchor(chunk xethash.Hash) (xethash.Hash, int, bool) {
	for h, x := range f.xorbs {
		if !x.permanent {
			continue
		}
		if i := slices.Index(x.hashes, chunk); i >= 0 {
			return h, i, true
		}
	}
	return xethash.Hash{}, 0, false
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// receiveRequest is a parsed receive-pack request.
type receiveRequest struct {
	commands []string
	caps     []string
	options  []string
	pack     []byte
}

func parseReceiveRequest(body []byte) (*receiveRequest, error) {
	r := bytes.NewReader(body)
	req := &receiveRequest{}
	for {
		n, p, err := pktline.ReadLine(r)
		if err != nil {
			return nil, fmt.Errorf("command: %w", err)
		}
		if n == pktline.Flush {
			break
		}
		line, caps, ok := strings.Cut(strings.TrimSuffix(string(p), "\n"), "\x00")
		if ok {
			req.caps = strings.Fields(caps)
		}
		req.commands = append(req.commands, line)
	}
	if slices.Contains(req.caps, "push-options") {
		for {
			n, p, err := pktline.ReadLine(r)
			if err != nil {
				return nil, fmt.Errorf("push option: %w", err)
			}
			if n == pktline.Flush {
				break
			}
			req.options = append(req.options, string(p))
		}
	}
	req.pack = body[len(body)-r.Len():]
	return req, nil
}

func (req *receiveRequest) encode() []byte {
	var b bytes.Buffer
	for i, c := range req.commands {
		if i == 0 {
			c += "\x00" + strings.Join(req.caps, " ")
		}
		pkt(&b, c)
	}
	_ = pktline.WriteFlush(&b)
	if slices.Contains(req.caps, "push-options") {
		for _, o := range req.options {
			pkt(&b, o)
		}
		_ = pktline.WriteFlush(&b)
	}
	b.Write(req.pack)
	return b.Bytes()
}

// objectFormat returns the hash of the request's object format.
func (req *receiveRequest) objectFormat() func() hash.Hash {
	if slices.Contains(req.caps, "object-format=sha256") {
		return sha256.New
	}
	return sha1.New
}

func (f *fakeBB) serveReceive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.receives++
	failReceive := f.failReceive
	f.mu.Unlock()
	if failReceive != 0 {
		http.Error(w, "receive-pack refused by the test", failReceive)
		return
	}
	req, err := parseReceiveRequest(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if i := slices.Index(req.caps, "entire-bb"); i >= 0 {
		f.mu.Lock()
		f.bbReceives++
		f.mu.Unlock()
		req.caps = append(req.caps[:i], req.caps[i+1:]...)
		if refusal, err := f.convertPack(req); err != nil {
			f.t.Errorf("fake BB: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		} else if refusal != "" {
			f.refuse(w, req, refusal)
			return
		}
	}
	fwd := r.Clone(r.Context())
	newBody := req.encode()
	fwd.Body = io.NopCloser(bytes.NewReader(newBody))
	fwd.ContentLength = int64(len(newBody))
	f.backend.ServeHTTP(w, fwd)
}

// refuse answers a push as entiredb refuses a BB push: the reason in the
// unpack status, and "unpacker error" for every ref.
func (f *fakeBB) refuse(w http.ResponseWriter, req *receiveRequest, reason string) {
	var report bytes.Buffer
	pkt(&report, "unpack push rejected: "+reason+"\n")
	for _, c := range req.commands {
		pkt(&report, "ng "+strings.Fields(c)[2]+" unpacker error\n")
	}
	_ = pktline.WriteFlush(&report)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if !slices.Contains(req.caps, "side-band-64k") {
		f.write(w, report.Bytes())
		return
	}
	var out bytes.Buffer
	pkt(&out, "\x01"+report.String())
	_ = pktline.WriteFlush(&out)
	f.write(w, out.Bytes())
}

// convertPack replaces each meta 0x03 entry of req.pack with its blob, and
// the 0xe002 header with a version 2 header. It returns a refusal reason
// for a push that entiredb refuses, and an error for a pack that the client
// must never send.
func (f *fakeBB) convertPack(req *receiveRequest) (string, error) {
	newHash := req.objectFormat()
	oidSize := newHash().Size()
	pack := req.pack
	if len(pack) < 12+oidSize || string(pack[:4]) != "PACK" {
		return "", fmt.Errorf("pack header %x", pack[:min(len(pack), 12)])
	}
	if binary.BigEndian.Uint32(pack[4:8]) != 0xe002 {
		return "", nil
	}
	h := newHash()
	h.Write(pack[:len(pack)-oidSize])
	if !bytes.Equal(h.Sum(nil), pack[len(pack)-oidSize:]) {
		return "", errors.New("bad pack trailer")
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	objects := pack[:len(pack)-oidSize]
	off := 12
	firstChunked := -1
	var blobs [][]byte
	named := map[xethash.Hash]bool{}
	for range count {
		start := off
		typ, _, n, err := readObjectHeader(objects[off:])
		if err != nil {
			return "", err
		}
		if typ != 5 {
			if firstChunked >= 0 {
				return "", errors.New("a whole object after a chunked entry")
			}
			if off, err = skipObject(objects, off+n, typ, oidSize); err != nil {
				return "", err
			}
			continue
		}
		if firstChunked < 0 {
			firstChunked = start
		}
		e, k, err := parseClientEntry(objects[off:], oidSize, f.threshold)
		if err != nil {
			return "", fmt.Errorf("entry at %d: %w", off, err)
		}
		off += k
		for _, x := range e.xorbs {
			named[x] = true
		}
		if len(named) > fakeMaxXorbs {
			return "", fmt.Errorf("the entries name more than %d xorbs", fakeMaxXorbs)
		}
		blob, refusal := f.rebuild(e, newHash)
		if refusal != "" {
			return refusal, nil
		}
		blobs = append(blobs, blob)
	}
	if off != len(objects) {
		return "", fmt.Errorf("%d bytes after the last object", len(objects)-off)
	}
	f.mu.Lock()
	for x := range named {
		f.xorbs[x].permanent = true
	}
	f.chunkedEntries += len(blobs)
	f.mu.Unlock()
	if firstChunked < 0 {
		firstChunked = off
	}

	var out bytes.Buffer
	out.WriteString("PACK")
	out.Write(binary.BigEndian.AppendUint32(nil, 2))
	out.Write(binary.BigEndian.AppendUint32(nil, count))
	out.Write(objects[12:firstChunked])
	for _, b := range blobs {
		out.Write(objectHeader(3, int64(len(b))))
		zw := zlib.NewWriter(&out)
		if _, err := zw.Write(b); err != nil {
			return "", fmt.Errorf("compress blob: %w", err)
		}
		if err := zw.Close(); err != nil {
			return "", fmt.Errorf("compress blob: %w", err)
		}
	}
	sum := newHash()
	sum.Write(out.Bytes())
	out.Write(sum.Sum(nil))
	req.pack = out.Bytes()
	return "", nil
}

// clientEntry is a parsed meta 0x03 entry.
type clientEntry struct {
	size     int64
	oid      []byte
	fileHash xethash.Hash
	xorbs    []xethash.Hash
	terms    []bbchunk.Term
}

// entryReader reads a payload and never runs past its end.
type entryReader struct {
	p   []byte
	err error
}

func (r *entryReader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.p) < n {
		r.err = errors.New("truncated payload")
		return nil
	}
	out := r.p[:n]
	r.p = r.p[n:]
	return out
}

func (r *entryReader) hash() xethash.Hash {
	var h xethash.Hash
	copy(h[:], r.bytes(xethash.Size))
	return h
}

func (r *entryReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, k := binary.Uvarint(r.p)
	if k <= 0 {
		r.err = errors.New("bad uvarint")
		return 0
	}
	r.p = r.p[k:]
	return v
}

// parseClientEntry parses the meta 0x03 entry at the start of b with the
// checks of entiredb that need no stored xorb. A threshold of 0 skips the
// threshold check. It returns the entry and its length in b.
func parseClientEntry(b []byte, oidSize int, threshold int64) (*clientEntry, int, error) {
	typ, size, n, err := readObjectHeader(b)
	switch {
	case err != nil:
		return nil, 0, err
	case typ != 5:
		return nil, 0, fmt.Errorf("type %d", typ)
	case n >= len(b):
		return nil, 0, errors.New("no meta byte")
	case b[n] != bbchunk.MetaClientChunked:
		return nil, 0, fmt.Errorf("kind %#x in a client push", b[n])
	}
	plen, k := binary.Uvarint(b[n+1:])
	switch {
	case k <= 0:
		return nil, 0, errors.New("bad payload size")
	case plen > fakeMaxRecipe+binary.MaxVarintLen64:
		return nil, 0, fmt.Errorf("payload of %d bytes is over %d", plen, fakeMaxRecipe)
	case size > fakeMaxObjectSize:
		return nil, 0, fmt.Errorf("size %d is over the %d-byte object limit", size, fakeMaxObjectSize)
	case size < threshold:
		return nil, 0, fmt.Errorf("size %d is under the threshold %d", size, threshold)
	}
	head := n + 1 + k
	if uint64(len(b)-head) < plen {
		return nil, 0, errors.New("truncated payload")
	}
	r := &entryReader{p: b[head : head+int(plen)]}
	e := &clientEntry{size: size, oid: r.bytes(oidSize), fileHash: r.hash()}
	if err := e.readXorbs(r); err != nil {
		return nil, 0, err
	}
	if err := e.readTerms(r); err != nil {
		return nil, 0, err
	}
	if recipe := int(plen) - len(r.p); recipe > fakeMaxRecipe {
		return nil, 0, fmt.Errorf("recipe of %d bytes is over %d", recipe, fakeMaxRecipe)
	}
	inline := r.uvarint()
	switch {
	case r.err != nil:
		return nil, 0, r.err
	case inline != 0:
		return nil, 0, errors.New("inline xorbs are not supported")
	case len(r.p) != 0:
		return nil, 0, fmt.Errorf("%d bytes after the inline count", len(r.p))
	}
	return e, head + int(plen), nil
}

func (e *clientEntry) readXorbs(r *entryReader) error {
	nx := r.uvarint()
	switch {
	case r.err != nil:
		return r.err
	case nx == 0:
		return errors.New("no xorbs")
	case nx > uint64(len(r.p))/xethash.Size:
		return errors.New("truncated xorb table")
	}
	seen := map[xethash.Hash]bool{}
	for range nx {
		x := r.hash()
		if seen[x] {
			return fmt.Errorf("xorb %s is listed more than once", x)
		}
		seen[x] = true
		e.xorbs = append(e.xorbs, x)
	}
	return r.err
}

func (e *clientEntry) readTerms(r *entryReader) error {
	nt := r.uvarint()
	switch {
	case r.err != nil:
		return r.err
	case nt == 0:
		return errors.New("no terms")
	case nt > uint64(len(r.p))/4:
		return errors.New("truncated term list")
	}
	used := make([]bool, len(e.xorbs))
	var total uint64
	for range nt {
		xi, start, end, unpacked := r.uvarint(), r.uvarint(), r.uvarint(), r.uvarint()
		switch {
		case r.err != nil:
			return r.err
		case xi >= uint64(len(e.xorbs)):
			return fmt.Errorf("xorb index %d, table has %d", xi, len(e.xorbs))
		case start >= end:
			return fmt.Errorf("chunk range [%d, %d) is empty", start, end)
		case end > xorb.MaxChunks:
			return fmt.Errorf("chunk end %d is over the %d chunks of a xorb", end, xorb.MaxChunks)
		case unpacked == 0:
			return errors.New("unpacked bytes is 0")
		case unpacked > uint64(e.size)-total:
			return errors.New("terms unpack to more than the logical size")
		}
		used[xi] = true
		total += unpacked
		e.terms = append(e.terms, bbchunk.Term{Xorb: int(xi), ChunkStart: int(start), ChunkEnd: int(end), UnpackedBytes: unpacked})
	}
	if total != uint64(e.size) {
		return fmt.Errorf("terms unpack to %d bytes, logical size is %d", total, e.size)
	}
	if i := slices.Index(used, false); i >= 0 {
		return fmt.Errorf("no term uses xorb %d", i)
	}
	return nil
}

// rebuild returns the blob of an entry from the stored xorbs, or the reason
// that entiredb refuses it with.
func (f *fakeBB) rebuild(e *clientEntry, newHash func() hash.Hash) ([]byte, string) {
	oid := hex.EncodeToString(e.oid)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropPushes > 0 {
		f.dropPushes--
		delete(f.xorbs, e.xorbs[0])
		return nil, "missing xorb " + e.xorbs[0].String()
	}
	blob := make([]byte, 0, e.size)
	var nodes []xethash.Node
	for _, term := range e.terms {
		x, ok := f.xorbs[e.xorbs[term.Xorb]]
		if !ok {
			return nil, "missing xorb " + e.xorbs[term.Xorb].String()
		}
		if term.ChunkEnd > len(x.chunks) {
			return nil, fmt.Sprintf("chunk range [%d, %d) is past xorb %s", term.ChunkStart, term.ChunkEnd, e.xorbs[term.Xorb])
		}
		n := len(blob)
		for i, c := range x.chunks[term.ChunkStart:term.ChunkEnd] {
			blob = append(blob, c...)
			nodes = append(nodes, xethash.Node{Hash: x.hashes[term.ChunkStart+i], Size: uint64(len(c))})
		}
		if uint64(len(blob)-n) != term.UnpackedBytes {
			return nil, fmt.Sprintf("chunked blob %s: term unpacks to %d bytes, entry says %d", oid, len(blob)-n, term.UnpackedBytes)
		}
	}
	if xethash.File(nodes) != e.fileHash {
		return nil, "chunked blob " + oid + ": file hash does not match"
	}
	h := newHash()
	fmt.Fprintf(h, "blob %d\x00", len(blob))
	h.Write(blob)
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		return nil, "chunked blob " + oid + ": content does not match"
	}
	return blob, ""
}

// readObjectHeader reads the type and size varint of a pack entry.
func readObjectHeader(b []byte) (typ int, size int64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, 0, errors.New("truncated object header")
	}
	c := b[0]
	typ = int(c>>4) & 7
	size = int64(c & 0x0f)
	shift := 4
	n = 1
	for c&0x80 != 0 {
		if n >= len(b) || shift > 56 {
			return 0, 0, 0, errors.New("bad object header")
		}
		c = b[n]
		n++
		size |= int64(c&0x7f) << shift
		shift += 7
	}
	return typ, size, n, nil
}

func objectHeader(typ byte, size int64) []byte {
	c := typ<<4 | byte(size&0x0f)
	size >>= 4
	var out []byte
	for size > 0 {
		out = append(out, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	return append(out, c)
}

// skipObject returns the end of the zlib data of a whole or delta object
// whose header ends at off.
func skipObject(b []byte, off, typ, oidSize int) (int, error) {
	switch typ {
	case 6:
		for off < len(b) && b[off]&0x80 != 0 {
			off++
		}
		off++
	case 7:
		off += oidSize
	}
	if off > len(b) {
		return 0, errors.New("truncated delta base")
	}
	br := bytes.NewReader(b[off:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return 0, fmt.Errorf("object at %d: %w", off, err)
	}
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return 0, fmt.Errorf("object at %d: %w", off, err)
	}
	return len(b) - br.Len(), nil
}

// counts returns what the server saw so far.
func (f *fakeBB) counts() (puts int, putBytes int64, receives, chunked int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts), f.putBytes, f.receives, f.chunkedEntries
}
