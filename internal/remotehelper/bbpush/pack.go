package bbpush

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // the pack trailer of a SHA-1 repository
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

const packHeaderSize = 12

// packSpec is the pack of a push: every object that pack-objects sends
// under the blob filter, then one meta 0x03 entry per large blob.
type packSpec struct {
	revs      []byte
	threshold int64
	ofsDelta  bool
	progress  bool
	// stderr takes the messages and progress of pack-objects.
	stderr  io.Writer
	entries [][]byte
	newHash func() hash.Hash
}

func newPackHash(objectFormat string) func() hash.Hash {
	if objectFormat == "sha256" {
		return sha256.New
	}
	return sha1.New
}

// appendPack writes the pack to body. It rewrites the header of
// pack-objects' output (version 0xe002, the count with the entries), drops
// its trailer, appends the entries and writes the trailer of the new pack.
// Only the trailer is held back, so the pack streams into body.
func appendPack(ctx context.Context, body *gitproto.RequestBody, spec packSpec) error {
	args := []string{"pack-objects", "--stdout", "--revs", "--all-progress-implied",
		"--filter=blob:limit=" + strconv.FormatInt(spec.threshold, 10)}
	if spec.ofsDelta {
		args = append(args, "--delta-base-offset")
	}
	if !spec.progress {
		args = append(args, "-q")
	}
	cmd := gitCommand(ctx, args...)
	cmd.Stdin = bytes.NewReader(spec.revs)
	cmd.Stderr = spec.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pack-objects stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pack-objects: %w", err)
	}
	werr := writePack(body, out, spec)
	if werr != nil {
		killProcess(cmd)
	}
	if err := cmd.Wait(); err != nil {
		return errors.Join(werr, fmt.Errorf("pack-objects: %w", err))
	}
	return werr
}

func writePack(body *gitproto.RequestBody, packOut io.Reader, spec packSpec) error {
	h := spec.newHash()
	var header [packHeaderSize]byte
	if _, err := io.ReadFull(packOut, header[:]); err != nil {
		return fmt.Errorf("read pack header: %w", err)
	}
	if string(header[:4]) != "PACK" || binary.BigEndian.Uint32(header[4:8]) != 2 {
		return fmt.Errorf("pack-objects wrote header %x", header)
	}
	count := uint64(binary.BigEndian.Uint32(header[8:])) + uint64(len(spec.entries))
	if count > 1<<32-1 {
		return fmt.Errorf("pack of %d objects", count)
	}
	binary.BigEndian.PutUint32(header[4:8], bbchunk.PackVersionExternal)
	binary.BigEndian.PutUint32(header[8:], uint32(count))
	h.Write(header[:])
	body.Append(header[:])

	objects := &tailReader{r: packOut, n: h.Size()}
	if _, err := body.ReadFrom(io.TeeReader(objects, h)); err != nil {
		return fmt.Errorf("read pack: %w", err)
	}
	for _, e := range spec.entries {
		h.Write(e)
		body.Append(e)
	}
	body.Append(h.Sum(nil))
	return nil
}

// tailReader passes r through except its last n bytes: the trailer of the
// pack that pack-objects wrote.
type tailReader struct {
	r    io.Reader
	n    int
	held []byte
	err  error
}

func (t *tailReader) Read(p []byte) (int, error) {
	for len(t.held) <= t.n {
		if t.err != nil {
			if errors.Is(t.err, io.EOF) && len(t.held) < t.n {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, t.err
		}
		if cap(t.held)-len(t.held) < 32<<10 {
			grown := make([]byte, len(t.held), len(t.held)+64<<10)
			copy(grown, t.held)
			t.held = grown
		}
		var m int
		m, t.err = t.r.Read(t.held[len(t.held):cap(t.held)])
		t.held = t.held[:len(t.held)+m]
	}
	k := copy(p, t.held[:len(t.held)-t.n])
	t.held = t.held[:copy(t.held, t.held[k:])]
	return k, nil
}
