package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"
)

const localTranscriptMaxBytes = 1024 * 1024
const compactToolNameKey = "name"
const compactResultStatusKey = "status"

type compactLogMessage struct {
	V          int              `json:"v"`
	Agent      string           `json:"agent"`
	Model      string           `json:"model,omitempty"`
	CLIVersion string           `json:"cli_version"`
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	TS         json.RawMessage  `json:"ts"`
	Content    []map[string]any `json:"content"`
}
type sentTranscriptMessage struct {
	digest    [32]byte
	timestamp json.RawMessage
	role      string
	model     string
}

// Reuse the batch converter on a bounded, already-redacted prefix. A conversion
// runs at most every 250ms (plus EOF), emitting only changed messages. This keeps
// native tool/result joins intact without parsing agent formats a second time.
type localTranscriptConverter struct {
	name      string
	sanitizer agent.Agent
	filters   []*regexp.Regexp
	content   []byte
	lines     int
	started   time.Time
	sent      map[string]sentTranscriptMessage
}

func (c *localTranscriptConverter) append(line []byte) error {
	if c.lines >= 4096 || len(c.content)+len(line)+1 > localTranscriptMaxBytes {
		return errors.New("transcript capture limit reached; start a new publishing stream")
	}
	sanitized := agent.SanitizeTranscriptForStorage(c.sanitizer, line)
	clean, err := redact.JSONLBytes(sanitized)
	if err != nil {
		return errors.New("transcript redaction failed; capture stopped")
	}
	if len(c.content)+clean.Len()+1 > localTranscriptMaxBytes {
		return errors.New("redacted transcript capture limit reached")
	}
	c.content = append(c.content, clean.Bytes()...)
	c.content = append(c.content, '\n')
	c.lines++
	return nil
}

func (c *localTranscriptConverter) updates() ([]localLogRecord, error) {
	data, err := compact.Compact(redact.AlreadyRedacted(c.content), compact.MetadataFields{Agent: c.name, CLIVersion: versioninfo.Version})
	if err != nil {
		return nil, errors.New("transcript conversion failed; capture stopped")
	}
	if len(data) > 4*localTranscriptMaxBytes {
		return nil, errors.New("converted transcript exceeds capture limit")
	}
	model := ""
	if extractor, ok := c.sanitizer.(agent.ModelExtractor); ok {
		if extracted, extractErr := extractor.ExtractModel(c.content); extractErr == nil {
			if filtered, ok := filterTranscriptStrings(extracted, c.filters, false).(string); ok && len(filtered) <= 256 {
				model = filtered
			}
		}
	}
	var updates []localLogRecord
	seen := make(map[string]bool)
	for index, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var raw struct {
			compactLogMessage

			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, errors.New("invalid compact transcript message")
		}
		message := raw.compactLogMessage
		if err := json.Unmarshal(raw.Content, &message.Content); err != nil {
			var text string
			if err := json.Unmarshal(raw.Content, &text); err != nil {
				return nil, errors.New("invalid compact transcript content")
			}
			message.Content = []map[string]any{{"type": transcript.ContentTypeText, "text": text}}
		}
		if message.Type != transcript.TypeUser && message.Type != transcript.TypeAssistant {
			return nil, errors.New("unsupported compact transcript role")
		}
		if message.ID == "" {
			message.ID = "ordinal:" + strconv.Itoa(index)
		} else {
			message.ID = fmt.Sprintf("source:%x", sha256.Sum256([]byte(message.ID)))
		}
		if seen[message.ID] {
			return nil, errors.New("duplicate transcript message identity")
		}
		seen[message.ID] = true
		if c.sent == nil {
			c.sent = make(map[string]sentTranscriptMessage)
		}
		previous, exists := c.sent[message.ID]
		if exists && previous.role != message.Type {
			return nil, errors.New("transcript message identity changed")
		}
		if message.Type == transcript.TypeAssistant {
			message.Model = model
		}
		if exists {
			if previous.model != "" {
				message.Model = previous.model
			}
			message.TS = previous.timestamp
		} else {
			timestamp := c.started
			var raw string
			if json.Unmarshal(message.TS, &raw) == nil {
				if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
					timestamp = parsed
				}
			} else {
				var millis int64
				if json.Unmarshal(message.TS, &millis) == nil && millis > 0 {
					timestamp = time.UnixMilli(millis)
				}
			}
			message.TS, err = json.Marshal(timestamp.UTC().Format(time.RFC3339Nano))
			if err != nil {
				return nil, fmt.Errorf("encode transcript timestamp: %w", err)
			}
		}
		// Publish compact text/tool blocks, not images or hidden/runtime blocks.
		blocks := make([]map[string]any, 0, len(message.Content))
		for _, block := range message.Content {
			typ, hasType := block["type"].(string)
			if typ == transcript.ContentTypeText || (!hasType && block["text"] != nil) || typ == transcript.ContentTypeToolUse {
				filtered := map[string]any{"type": transcript.ContentTypeText}
				if typ == transcript.ContentTypeToolUse {
					filtered["type"] = typ
				}
				for _, key := range []string{transcript.ContentTypeText, compactToolNameKey, "input", "result"} {
					if value, ok := block[key]; ok {
						filtered[key] = filterTranscriptStrings(value, c.filters, key == "input")
					}
				}
				if id, ok := block["id"].(string); ok && id != "" {
					// Opaque identities stay stable even with a catch-all redaction regex.
					filtered["id"] = fmt.Sprintf("tool:%x", sha256.Sum256([]byte(id)))
				}
				blocks = append(blocks, filtered)
			}
		}
		message.Content = blocks
		encoded, err := json.Marshal(message)
		if err != nil {
			return nil, errors.New("encode compact transcript message")
		}
		digest := sha256.Sum256(encoded)
		if exists && previous.digest == digest {
			continue
		}
		if exists && message.Type == transcript.TypeUser {
			return nil, errors.New("transcript prompt changed; start a new publishing stream")
		}
		if len(encoded) > 8192 || len(blocks) > 64 {
			return nil, errors.New("transcript message exceeds live record limit; capture stopped")
		}
		// Mark delivered only in accept(), after its event entered the output queue.
		updates = append(updates, localLogRecord{Kind: localLogKindMessage, Text: string(encoded)})
	}
	for id := range c.sent {
		if !seen[id] {
			return nil, errors.New("transcript branch changed; start a new publishing stream")
		}
	}
	return updates, nil
}

func (c *localTranscriptConverter) accept(record localLogRecord) {
	var message compactLogMessage
	if json.Unmarshal([]byte(record.Text), &message) != nil {
		return
	}
	c.sent[message.ID] = sentTranscriptMessage{digest: sha256.Sum256([]byte(record.Text)), timestamp: message.TS, role: message.Type, model: message.Model}
}

func filterTranscriptStrings(value any, filters []*regexp.Regexp, redactKeys bool) any {
	switch value := value.(type) {
	case string:
		for _, filter := range filters {
			value = filter.ReplaceAllString(value, "[REDACTED]")
		}
		return value
	case []any:
		for i := range value {
			value[i] = filterTranscriptStrings(value[i], filters, redactKeys)
		}
	case map[string]any:
		filtered := make(map[string]any, len(value))
		for index, key := range slices.Sorted(maps.Keys(value)) {
			child := value[key]
			// Preserve the compact result discriminator, not user argument data.
			if !redactKeys && key == compactResultStatusKey && (child == "success" || child == "error") {
				filtered[key] = child
				continue
			}
			if redactKeys {
				original := key
				for _, filter := range filters {
					key = filter.ReplaceAllString(key, "[REDACTED]")
				}
				if key != original {
					// Redacted argument keys must not collide or change between ticks.
					key += ":" + strconv.Itoa(index)
				}
			}
			filtered[key] = filterTranscriptStrings(child, filters, redactKeys)
		}
		return filtered
	}
	return value
}

// The reader never waits for network IO or conversion. On overload or malformed
// JSON it signals a terminal capture gap, then drains stdin without publishing.
func readLocalTranscript(ctx context.Context, input io.Reader, lines chan<- []byte, failed chan<- error) {
	defer close(lines)
	reader := bufio.NewReaderSize(input, 64*1024)
	total := 0
	for {
		line, err := reader.ReadSlice('\n')
		total += len(line)
		if len(bytes.TrimSpace(line)) > 0 {
			var captureErr error
			switch {
			case total > localTranscriptMaxBytes:
				captureErr = errors.New("transcript input exceeds 1 MiB capture limit")
			case errors.Is(err, bufio.ErrBufferFull):
				captureErr = errors.New("transcript input line exceeds 64 KiB")
			case !utf8.Valid(line) || !json.Valid(line) || bytes.TrimSpace(line)[0] != '{':
				captureErr = errors.New("invalid or incomplete transcript JSONL")
			default:
				select {
				case lines <- bytes.Clone(line):
				case <-ctx.Done():
					return
				default:
					captureErr = errors.New("transcript input queue exceeded")
				}
			}
			if captureErr != nil {
				failed <- captureErr
				_, _ = io.Copy(io.Discard, reader) //nolint:errcheck // Capture failure was reported; draining is best effort.
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				failed <- errors.New("transcript input read failed")
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func publishLocalTranscript(ctx context.Context, input io.Reader, diagnostics io.Writer, name string, filters []*regexp.Regexp, send func(context.Context, []localLogRecord) error) error {
	return publishLocalTranscriptCapture(ctx, input, diagnostics, name, filters, send, func() {})
}

// A pipe must keep draining after capture failure; an automatic file tail must
// stop waiting for future appends instead. stopInput cancels only that reader,
// leaving the uploader alive long enough to report the terminal capture gap.
func publishLocalTranscriptCapture(ctx context.Context, input io.Reader, diagnostics io.Writer, name string, filters []*regexp.Regexp, send func(context.Context, []localLogRecord) error, stopInput func()) error {
	ag, err := agent.Get(agenttypes.AgentName(name))
	if err != nil {
		return fmt.Errorf("resolve transcript agent: %w", err)
	}
	return publishLocalLogRecords(ctx, diagnostics, send, func(records chan<- localLogRecord, dropped *atomic.Int64) error {
		lines := make(chan []byte, 4096)
		failed := make(chan error, 1)
		go readLocalTranscript(ctx, input, lines, failed)
		converter := localTranscriptConverter{name: name, sanitizer: ag, filters: filters, started: time.Now()}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var captureErr error
		dirty := false
		stop := func(err error) {
			if captureErr != nil {
				return
			}
			captureErr = err
			stopInput()
			select {
			case records <- localLogRecord{Kind: localLogKindGap, Text: "Transcript capture stopped; start a new publishing stream."}:
			default:
				dropped.Add(1)
			}
		}
		flush := func(wait bool) {
			if !dirty || captureErr != nil {
				return
			}
			updates, err := converter.updates()
			if err != nil {
				stop(err)
				return
			}
			dirty = false
			for _, record := range updates {
				if wait {
					select {
					case records <- record:
						converter.accept(record)
					case <-ctx.Done():
						stop(ctx.Err())
						return
					}
					continue
				}
				select {
				case records <- record:
					converter.accept(record)
				default:
					// Keep this revision pending: the next tick retries it, or replaces it
					// with a newer full message. No half-tool-result update can be published.
					dirty = true
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-failed:
				stop(err)
			case line, ok := <-lines:
				if !ok {
					select {
					case err := <-failed:
						stop(err)
					default:
					}
					flush(true)
					if captureErr == nil && converter.lines > 0 && len(converter.sent) == 0 {
						stop(errors.New("selected input contains no supported transcript messages"))
					}
					return captureErr
				}
				if captureErr == nil {
					if err := converter.append(line); err != nil {
						stop(err)
					} else {
						dirty = true
					}
				}
			case <-ticker.C:
				flush(false)
			}
		}
	})
}
