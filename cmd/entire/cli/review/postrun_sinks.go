package review

import (
	"bytes"
	"io"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

type tuiPostRunCompleteSink struct {
	tui *TUISink
	buf *bytes.Buffer
	out io.Writer
}

func (s tuiPostRunCompleteSink) AgentEvent(_ string, _ reviewtypes.Event) {}

func (s tuiPostRunCompleteSink) RunFinished(_ reviewtypes.RunSummary) {
	if s.tui != nil {
		s.tui.PostRunComplete()
	}
	s.flushBuffer()
}

func (s tuiPostRunCompleteSink) flushBuffer() {
	if s.buf == nil || s.out == nil || s.buf.Len() == 0 {
		return
	}
	// Best-effort flush of buffered post-run output; a write error here means
	// the terminal is gone and there is nothing actionable to do.
	_, _ = s.out.Write(s.buf.Bytes()) //nolint:errcheck // best-effort terminal flush
}
