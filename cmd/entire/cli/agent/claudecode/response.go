package claudecode

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type responseEnvelope struct {
	Type           string  `json:"type"`
	Subtype        string  `json:"subtype"`
	IsError        bool    `json:"is_error"`
	APIErrorStatus *int    `json:"api_error_status"`
	Result         *string `json:"result"`
}

// parseGenerateTextResponse extracts the raw text payload and envelope metadata
// from Claude CLI JSON output. Claude may return either a single object or an array of events.
// The returned envelope allows callers to check IsError and APIErrorStatus.
func parseGenerateTextResponse(stdout []byte) (string, *responseEnvelope, error) {
	var response responseEnvelope
	if err := json.Unmarshal(stdout, &response); err == nil {
		if response.Result != nil {
			return *response.Result, &response, nil
		}
		// is_error:true with null result is a structured CLI failure; callers
		// need the envelope (IsError, APIErrorStatus) for classification.
		if response.IsError {
			return "", &response, nil
		}
	}

	var responses []responseEnvelope
	if err := json.Unmarshal(stdout, &responses); err != nil {
		return "", nil, fmt.Errorf("unsupported Claude CLI JSON response: %w", err)
	}

	for i := len(responses) - 1; i >= 0; i-- {
		if responses[i].Type != streamEventTypeResult {
			continue
		}
		if responses[i].Result != nil {
			return *responses[i].Result, &responses[i], nil
		}
		// Mirror the object-path behavior: is_error:true with null result is
		// a structured failure whose envelope (IsError, APIErrorStatus) must
		// reach classifyEnvelopeError.
		if responses[i].IsError {
			return "", &responses[i], nil
		}
	}

	return "", nil, errors.New("unsupported Claude CLI JSON response: missing result item")
}

// streamEventTypeResult is the Claude CLI stream-event type that marks the
// terminal envelope of a generation. Used to identify the final event during
// scanning and to assert in tests.
const streamEventTypeResult = "result"

// streamEventTypeStreamEvent is the Claude CLI event type for wrapper events
// that carry inner Anthropic API stream events (message_start, content_block_delta, etc.).
const streamEventTypeStreamEvent = "stream_event"

// streamBufferMax bounds a single NDJSON line. Stream events can be large
// (init carries the full tool list; deltas can carry long thinking chunks)
// so we lift the default scanner limit substantially.
//

const streamBufferMax = 4 * 1024 * 1024 // 4 MiB

// streamEvent represents one decoded line from the stream-json NDJSON output.
// Fields are populated based on the event Type/Subtype.
//

type streamEvent struct {
	Type    string           `json:"type"`
	Subtype string           `json:"subtype"`
	Status  string           `json:"status"` // e.g. "requesting" for type=system,subtype=status
	Event   streamInnerEvent `json:"event"`  // for type=stream_event

	// Fields populated for type=result.
	IsError        bool          `json:"is_error"`
	APIErrorStatus *int          `json:"api_error_status"`
	Result         *string       `json:"result"`
	DurationMs     int           `json:"duration_ms"`
	TTFTms         int           `json:"ttft_ms,omitempty"` // time-to-first-token; on outer stream_event envelope
	Usage          *messageUsage `json:"usage"`
}

// streamInnerEvent holds the nested "event" payload for type=stream_event.
//

type streamInnerEvent struct {
	Type    string         `json:"type"` // "message_start" | "content_block_delta" | "message_delta" | ...
	Delta   *streamDelta   `json:"delta,omitempty"`
	Message *streamMessage `json:"message,omitempty"`
}

// streamDelta carries the content-block delta payload.
//

type streamDelta struct {
	Type     string `json:"type"` // "text_delta" | "thinking_delta" | ...
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// streamMessage is the partial-message payload on message_start. Only the
// usage field is currently consumed by callers; other fields are ignored.
//

type streamMessage struct {
	Usage *messageUsage `json:"usage,omitempty"`
}

// streamClaudeResponse reads NDJSON-encoded events from r, invokes onEvent
// for every successfully decoded event, and returns the final result event
// once the stream ends. Malformed lines are skipped to keep the stream
// resilient against single-line corruption; the count is returned so callers
// can log schema drift even on otherwise-successful runs.
//

func streamClaudeResponse(r io.Reader, onEvent func(streamEvent)) (*streamEvent, int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), streamBufferMax)
	var final *streamEvent
	var malformedLines int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			malformedLines++
			continue // best-effort: skip and keep streaming
		}
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Type == streamEventTypeResult {
			captured := ev
			final = &captured
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, malformedLines, fmt.Errorf("reading claude stream: %w", err)
	}
	if final == nil {
		if malformedLines > 0 {
			return nil, malformedLines, fmt.Errorf("claude stream ended without a result event (%d malformed lines skipped)", malformedLines)
		}
		return nil, 0, errors.New("claude stream ended without a result event")
	}
	return final, malformedLines, nil
}
