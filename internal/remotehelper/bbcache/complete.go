package bbcache

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/jmwoliver/xet-go/xethash"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// HasComplete reports whether the store holds the complete xorb h. It pins
// the file, so Trim keeps it for the fetch that asked, and marks it as used.
// It does not read the file: OpenComplete and the chunk reads check it.
func (s *Store) HasComplete(h xethash.Hash) bool {
	name := s.name(completeDir, h.String())
	info, err := osroot.LstatNoSymlinks(s.root, name)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	s.pin(name)
	s.touch(name)
	return true
}

// PutComplete stores the complete xorb h. It calls fill with a writer to a
// temp file in the store. fill writes the whole xorb and returns its footer,
// which it has checked against h. PutComplete then renames the file into
// place, pins it, and records h as permanent with the footer: the server
// serves only permanent xorbs. An error from fill comes back wrapped, and
// leaves no file.
func (s *Store) PutComplete(h xethash.Hash, fill func(io.Writer) (*bbchunk.Footer, error)) error {
	name := s.name(completeDir, h.String())
	parent, leaf, closeParent, err := osroot.OpenParentNoSymlinks(s.root, name)
	if err != nil {
		return fmt.Errorf("bbcache: store xorb %s: %w", h, err)
	}
	defer closeParent()
	tmp, tmpName, err := jsonutil.CreateTempIn(parent, leaf)
	if err != nil {
		return fmt.Errorf("bbcache: store xorb %s: %w", h, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = parent.Remove(tmpName) //nolint:errcheck // best effort; Trim removes a stale temp file
		}
	}()
	f, written, err := fillFile(tmp, fill)
	if err != nil {
		return fmt.Errorf("bbcache: store xorb %s: %w", h, err)
	}
	if f == nil || written != f.Size() {
		return fmt.Errorf("bbcache: store xorb %s: wrote %d bytes that do not match the footer", h, written)
	}
	s.pin(name)
	if err := s.rename(parent, tmpName, leaf); err != nil {
		// Windows refuses to replace a file that another process holds open.
		// That process stored the same xorb, so the xorb is in place.
		if !s.HasComplete(h) {
			return fmt.Errorf("bbcache: store xorb %s: %w", h, err)
		}
		debuglog.Printf("bbcache: store xorb %s: %v; another process stored it", h, err)
	} else {
		renamed = true
	}
	s.touch(name)
	return s.putRecord(h, Record{UploadedAt: s.now(), State: bbclient.StatePermanent, Chunks: footerChunks(f)})
}

// fillFile runs fill on a buffered writer to tmp and closes tmp. It returns
// the footer and the number of bytes written.
func fillFile(tmp *os.File, fill func(io.Writer) (*bbchunk.Footer, error)) (*bbchunk.Footer, int64, error) {
	cw := &countingWriter{w: bufio.NewWriterSize(tmp, 64<<10)}
	f, err := fill(cw)
	if err == nil {
		err = cw.w.Flush()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	return f, cw.n, err
}

type countingWriter struct {
	w *bufio.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err //nolint:wrapcheck // the caller of fill names the xorb
}

func footerChunks(f *bbchunk.Footer) []bbclient.FooterChunk {
	out := make([]bbclient.FooterChunk, f.Chunks())
	for i := range out {
		out[i] = bbclient.FooterChunk{Hash: f.ChunkHashes[i], UnpackedEnd: uint64(f.UnpackedChunkOffsets[i]), StoredEnd: uint64(f.ChunkBoundaryOffsets[i])}
	}
	return out
}

// CachedXorb is an open complete xorb of the store. Close it after use.
type CachedXorb struct {
	s      *Store
	name   string
	file   *os.File
	footer *bbchunk.Footer
	// corrupt is set when a chunk read fails its check. Close then removes
	// the file. The removal waits for Close, because some systems cannot
	// remove an open file.
	corrupt atomic.Bool
}

// OpenComplete opens the complete xorb h. It reads the footer and checks it
// against h, and it removes a file whose footer fails. It reports false for
// a missing or removed xorb. It pins the file, so Trim keeps it.
func (s *Store) OpenComplete(h xethash.Hash) (*CachedXorb, bool) {
	name := s.name(completeDir, h.String())
	file, err := osroot.OpenNoFollow(s.root, name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			debuglog.Printf("bbcache: open xorb %s: %v", h, err)
		}
		return nil, false
	}
	footer, err := readFooter(file, h)
	if err != nil {
		_ = file.Close()
		debuglog.Printf("bbcache: dropping xorb %s: %v", h, err)
		s.remove(name)
		return nil, false
	}
	s.pin(name)
	s.touch(name)
	return &CachedXorb{s: s, name: name, file: file, footer: footer}, true
}

// readFooter reads the footer at the end of file and checks it against h.
func readFooter(file *os.File, h xethash.Hash) (*bbchunk.Footer, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	return bbchunk.ReadFooter(file, info.Size(), h) //nolint:wrapcheck // the caller names the xorb
}

// Footer returns the checked footer of the xorb.
func (x *CachedXorb) Footer() *bbchunk.Footer { return x.footer }

// Term returns a reader of the data of chunks [start, end), in order. The
// reader checks each chunk against the footer. A failed check wraps
// bbchunk.ErrCorruptXorb, and Close then removes the file, so the caller
// can download the xorb again.
func (x *CachedXorb) Term(start, end int) (io.Reader, error) {
	r, err := bbchunk.NewTermReader(x.file, x.footer, start, end)
	if err != nil {
		return nil, err //nolint:wrapcheck // NewTermReader names the range
	}
	return &corruptionWatch{r: r, x: x}, nil
}

// Close closes the file. It removes the file when a chunk read found it
// corrupt.
func (x *CachedXorb) Close() error {
	err := x.file.Close()
	if x.corrupt.Load() {
		debuglog.Printf("bbcache: dropping corrupt xorb %s", x.name)
		x.s.remove(x.name)
	}
	if err != nil {
		return fmt.Errorf("bbcache: close xorb: %w", err)
	}
	return nil
}

type corruptionWatch struct {
	r io.Reader
	x *CachedXorb
}

func (c *corruptionWatch) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, bbchunk.ErrCorruptXorb) {
		c.x.corrupt.Store(true)
	}
	return n, err //nolint:wrapcheck // the reader of NewTermReader names the chunk
}
