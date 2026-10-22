package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// CumulativeObservation is one reading of a session's running totals.
type CumulativeObservation struct {
	// Total and Children are what CumulativeTokenSource returns.
	Total    *TokenUsage
	Children map[string]*TokenUsage
	// Cursor lets the next reading continue from this one; nil for agents
	// that cannot resume.
	Cursor json.RawMessage
	// Restarted reports that the previous cursor no longer described a prefix
	// of the transcript, so the transcript was rewritten (a resume restoring an
	// older copy) and Total was read again from its start. A Total below an
	// earlier one is a rewrite only then; otherwise it is a stale read.
	Restarted bool
}

// ResumableCumulativeTokenSource is a CumulativeTokenSource that can continue
// from where an earlier reading stopped, so a session's running total costs the
// bytes appended since then rather than a parse of the whole transcript. The
// totals it returns are the ones CumulativeTokenUsage returns.
//
// cursor is an earlier observation's Cursor, or nil to start from the
// beginning. The agent owns its encoding. A cursor that does not describe a
// prefix of the current transcript must not be trusted: the agent reads from
// the beginning and sets Restarted.
type ResumableCumulativeTokenSource interface {
	CumulativeTokenSource

	ResumeCumulativeTokenUsage(ctx context.Context, in CumulativeTokenInput, cursor json.RawMessage) (CumulativeObservation, error)
}

// ObserveCumulativeTokenUsage reads a session's running totals, continuing from
// cursor when the agent can resume. ok is false for an agent with no
// cumulative source.
func ObserveCumulativeTokenUsage(ctx context.Context, ag Agent, in CumulativeTokenInput, cursor json.RawMessage) (obs CumulativeObservation, ok bool, err error) {
	if resumable, isResumable := AsResumableCumulativeTokenSource(ag); isResumable {
		obs, err = resumable.ResumeCumulativeTokenUsage(ctx, in, cursor)
		if err != nil {
			return CumulativeObservation{}, true, fmt.Errorf("resume cumulative token usage: %w", err)
		}
		return obs, true, nil
	}
	src, isSource := AsCumulativeTokenSource(ag)
	if !isSource {
		return CumulativeObservation{}, false, nil
	}
	total, children, err := src.CumulativeTokenUsage(ctx, in)
	if err != nil {
		return CumulativeObservation{}, true, fmt.Errorf("cumulative token usage: %w", err)
	}
	return CumulativeObservation{Total: total, Children: children}, true, nil
}

// cursorCheckBytes is how much of the consumed prefix a TranscriptCursor
// fingerprints. A rewrite that keeps these bytes and the length is not
// detected; a transcript rewritten by a resume or compaction does not.
const cursorCheckBytes = 512

// TranscriptCursor marks how far a resumable reader got through an
// append-only JSONL transcript: Offset is the end of the last complete line it
// consumed, and Check fingerprints the bytes just before it so a transcript
// rewritten in place is not mistaken for one that grew.
type TranscriptCursor struct {
	Offset int    `json:"offset"`
	Check  string `json:"check,omitempty"`
}

// NewTranscriptCursor returns the cursor for data consumed up to offset.
func NewTranscriptCursor(data []byte, offset int) TranscriptCursor {
	return TranscriptCursor{Offset: offset, Check: prefixCheck(data, offset)}
}

// Resumes reports whether data still begins with what c consumed.
func (c TranscriptCursor) Resumes(data []byte) bool {
	return c.Offset > 0 && c.Offset <= len(data) && prefixCheck(data, c.Offset) == c.Check
}

// CompleteLinesEnd returns the end of the last complete line in data at or
// after from: a trailing line still being written is left for the next call.
func CompleteLinesEnd(data []byte, from int) int {
	if i := bytes.LastIndexByte(data[from:], '\n'); i >= 0 {
		return from + i + 1
	}
	return from
}

func prefixCheck(data []byte, offset int) string {
	start := max(offset-cursorCheckBytes, 0)
	sum := sha256.Sum256(data[start:offset])
	return hex.EncodeToString(sum[:8])
}
