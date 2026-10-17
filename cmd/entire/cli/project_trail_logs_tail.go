package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// autoLogReader follows one pinned, confined transcript, keeping EOF temporary
// until the owning session ends. Unlike a pipe, it is cancellable while idle.
// File replacement, truncation, or a rewrite of captured bytes is a terminal gap.
// Input is bounded independently of the converter, including its drain-on-error
// path, so a failed automatic capture cannot leave an infinite drain process.
type autoLogReader struct {
	ctx         context.Context
	file        *os.File
	initial     os.FileInfo
	offset      int64
	captured    []byte
	skipPartial bool
	stat        func() (os.FileInfo, error)
	check       func() (bool, error)
	save        func(int64) error
	interval    time.Duration
}

func autoLogPartialLine(file *os.File, offset int64) bool {
	if offset == 0 {
		return false
	}
	var last [1]byte
	_, err := file.ReadAt(last[:], offset-1)
	return err != nil || last[0] != '\n'
}

func (r *autoLogReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, fmt.Errorf("automatic stream cancelled: %w", err)
		}
		ended, err := r.check()
		if err != nil {
			return 0, err
		}
		info, err := r.stat()
		if err != nil || !os.SameFile(r.initial, info) || !info.Mode().IsRegular() || info.Size() < r.offset {
			return 0, errors.New("automatic stream transcript replaced or truncated")
		}
		if len(r.captured) > 0 {
			previous := make([]byte, len(r.captured))
			n, err := r.file.ReadAt(previous, r.offset-int64(len(r.captured)))
			if err != nil || n != len(previous) || !bytes.Equal(previous, r.captured) {
				return 0, errors.New("automatic stream transcript history changed")
			}
		}
		if info.Size() > r.offset {
			remaining := localTranscriptMaxBytes - len(r.captured)
			if remaining == 0 {
				return 0, errors.New("automatic stream capture limit reached; next lifecycle hook starts a new stream")
			}
			size := min(len(p), remaining, int(info.Size()-r.offset))
			n, err := r.file.ReadAt(p[:size], r.offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("read automatic transcript: %w", err)
			}
			if n > 0 {
				// At-most-once capture across a crash. A new stream discloses the gap;
				// persisting before upload avoids replaying old work into a new scope.
				if err := r.save(r.offset + int64(n)); err != nil {
					return 0, err
				}
				r.offset += int64(n)
				r.captured = append(r.captured, p[:n]...)
				if r.skipPartial {
					end := bytes.IndexByte(p[:n], '\n')
					if end < 0 {
						continue
					}
					r.skipPartial = false
					n = copy(p, p[end+1:n])
					if n == 0 {
						continue
					}
				}
				return n, nil
			}
		}
		if ended {
			return 0, io.EOF
		}
		timer := time.NewTimer(r.interval)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return 0, fmt.Errorf("automatic stream cancelled: %w", r.ctx.Err())
		case <-timer.C:
		}
	}
}
