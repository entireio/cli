package transcript

import "bytes"

// lineSplitter splits a growing transcript into complete lines for the
// streaming readers (JSONLAdapter, PiAdapter, CodexRolloutAdapter,
// CodexItemRolloutDecider), holding a trailing partial line until a later
// chunk completes it, or until flush.
type lineSplitter struct {
	pending []byte // bytes after the last newline fed; see lineSplitterKeep
}

// lineSplitterKeep is the most capacity feed keeps in pending once a line it
// held is read: a longer line's buffer (a rollout line carrying an inline
// image can be several MB) is let go rather than kept for the reader's
// lifetime.
const lineSplitterKeep = 1 << 20

// feed calls fn with each line chunk completes, its newline included. The
// line may alias the splitter's buffer: fn must not keep it. fn returns
// false to stop reading; the rest of chunk and any held bytes are dropped.
func (s *lineSplitter) feed(chunk []byte, fn func(line []byte) bool) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.pending = append(s.pending, chunk...)
			return
		}
		line := chunk[:i+1]
		if len(s.pending) > 0 {
			s.pending = append(s.pending, line...)
			line = s.pending
		}
		more := fn(line)
		if cap(s.pending) > lineSplitterKeep {
			s.pending = nil
		} else {
			s.pending = s.pending[:0]
		}
		if !more {
			s.pending = nil
			return
		}
		chunk = chunk[i+1:]
	}
}

// flush returns the held partial line, nil if there is none, and forgets it.
func (s *lineSplitter) flush() []byte {
	line := s.pending
	s.pending = nil
	if len(line) == 0 {
		return nil
	}
	return line
}
