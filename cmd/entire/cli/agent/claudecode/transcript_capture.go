package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

type captureConfig struct {
	pollInterval   time.Duration
	quietWindow    time.Duration
	maxWait        time.Duration
	staleThreshold time.Duration
	readTranscript func(context.Context, string, transcriptFingerprint) ([]byte, error)
}

const (
	defaultPollInterval           = 50 * time.Millisecond
	defaultQuietWindow            = 500 * time.Millisecond
	defaultMaxWait                = 3 * time.Second
	defaultStaleThreshold         = 2 * time.Minute
	assistantContentBlockTypeText = "text"
)

var assistantRecordMarker = []byte(envelopeTypeAssistant)

func defaultCaptureConfig() captureConfig {
	return captureConfig{
		pollInterval:   defaultPollInterval,
		quietWindow:    defaultQuietWindow,
		maxWait:        defaultMaxWait,
		staleThreshold: defaultStaleThreshold,
	}
}

type transcriptFingerprint struct {
	info    os.FileInfo
	size    int64
	modTime time.Time
}

var errTranscriptChanged = errors.New("transcript changed during capture")

func (c *ClaudeCodeAgent) CaptureTranscript(ctx context.Context, request agent.TranscriptCaptureRequest) (agent.TranscriptSnapshot, error) {
	return c.captureTranscript(ctx, request, defaultCaptureConfig())
}

func (c *ClaudeCodeAgent) captureTranscript(
	ctx context.Context,
	request agent.TranscriptCaptureRequest,
	config captureConfig,
) (agent.TranscriptSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return agent.TranscriptSnapshot{}, fmt.Errorf("capture transcript canceled: %w", err)
	}
	if request.StartPosition < 0 {
		return agent.TranscriptSnapshot{}, fmt.Errorf("%w: negative turn-start position", agent.ErrTranscriptNotReady)
	}
	captureCtx, cancel := context.WithTimeout(ctx, config.maxWait)
	defer cancel()

	observed, err := fingerprintTranscript(request.SessionRef)
	if err != nil {
		return agent.TranscriptSnapshot{}, fmt.Errorf("%w: %w", agent.ErrTranscriptNotReady, err)
	}
	if err := captureCtx.Err(); err != nil {
		return agent.TranscriptSnapshot{}, captureWaitError(ctx, config.maxWait)
	}
	if time.Since(observed.modTime) > config.staleThreshold {
		return agent.TranscriptSnapshot{}, fmt.Errorf("%w: transcript is stale", agent.ErrTranscriptNotReady)
	}
	modern := request.FinalResponse != nil && *request.FinalResponse != ""

	ticker := time.NewTicker(config.pollInterval)
	defer ticker.Stop()
	// Producer evidence can validate the initial snapshot immediately. The
	// legacy path still waits for quietWindow before it can return.
	immediate := make(chan time.Time, 1)
	immediate <- time.Now()
	poll := (<-chan time.Time)(immediate)
	readTranscript := config.readTranscript
	if readTranscript == nil {
		readTranscript = readObservedTranscript
	}
	stableSince := time.Now()
	observedValid := true
	var rejected transcriptFingerprint
	hasRejected := false

	for {
		select {
		case <-captureCtx.Done():
			return agent.TranscriptSnapshot{}, captureWaitError(ctx, config.maxWait)
		case <-poll:
			poll = ticker.C
			current, statErr := fingerprintTranscript(request.SessionRef)
			if statErr != nil {
				observedValid = false
				stableSince = time.Now()
				continue
			}
			if time.Since(current.modTime) > config.staleThreshold {
				observedValid = false
				stableSince = time.Now()
				continue
			}
			if !observedValid || !sameTranscriptFingerprint(observed, current) {
				observed = current
				observedValid = true
				stableSince = time.Now()
			}
			if !modern {
				if time.Since(stableSince) < config.quietWindow {
					continue
				}
			}
			if hasRejected && sameTranscriptFingerprint(rejected, current) {
				continue
			}

			data, readErr := readTranscript(captureCtx, request.SessionRef, current)
			if readErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return agent.TranscriptSnapshot{}, fmt.Errorf("capture transcript canceled: %w", ctxErr)
				}
				if captureCtx.Err() != nil {
					return agent.TranscriptSnapshot{}, captureWaitError(ctx, config.maxWait)
				}
				if errors.Is(readErr, errTranscriptChanged) {
					observedValid = false
					if refreshed, refreshErr := fingerprintTranscript(request.SessionRef); refreshErr == nil {
						observed = refreshed
						observedValid = true
						stableSince = time.Now()
					}
					continue
				}
				return agent.TranscriptSnapshot{}, fmt.Errorf("%w: %w", agent.ErrTranscriptNotReady, readErr)
			}

			position, finalAssistant, validationErr := validateTranscriptSnapshot(captureCtx, data, request.StartPosition, modern)
			if validationErr != nil {
				if captureCtx.Err() != nil {
					return agent.TranscriptSnapshot{}, captureWaitError(ctx, config.maxWait)
				}
				rejected = current
				hasRejected = true
				continue
			}
			if request.FinalResponse != nil && *request.FinalResponse != "" && finalAssistant != *request.FinalResponse {
				rejected = current
				hasRejected = true
				continue
			}
			if captureCtx.Err() != nil {
				return agent.TranscriptSnapshot{}, captureWaitError(ctx, config.maxWait)
			}
			return agent.TranscriptSnapshot{Data: data, Position: position}, nil
		}
	}
}

func captureWaitError(ctx context.Context, maxWait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("capture transcript canceled: %w", err)
	}
	return fmt.Errorf("%w: timed out after %s", agent.ErrTranscriptNotReady, maxWait)
}

func fingerprintTranscript(path string) (transcriptFingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return transcriptFingerprint{}, fmt.Errorf("stat transcript: %w", err)
	}
	if !info.Mode().IsRegular() {
		return transcriptFingerprint{}, errors.New("not a regular file")
	}
	return transcriptFingerprint{info: info, size: info.Size(), modTime: info.ModTime()}, nil
}

func sameTranscriptFingerprint(left, right transcriptFingerprint) bool {
	return os.SameFile(left.info, right.info) && left.size == right.size && left.modTime.Equal(right.modTime)
}

func readObservedTranscript(ctx context.Context, path string, observed transcriptFingerprint) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("capture transcript canceled: %w", err)
	}
	f, err := os.Open(path) //nolint:gosec // path comes from the agent hook payload
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	before, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened transcript: %w", err)
	}
	opened := transcriptFingerprint{info: before, size: before.Size(), modTime: before.ModTime()}
	if !sameTranscriptFingerprint(observed, opened) {
		return nil, errTranscriptChanged
	}

	data := make([]byte, observed.size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, fmt.Errorf("%w: %w", errTranscriptChanged, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("capture transcript canceled: %w", err)
	}

	after, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("restat opened transcript: %w", err)
	}
	pathAfter, err := fingerprintTranscript(path)
	if err != nil {
		return nil, errTranscriptChanged
	}
	handleAfter := transcriptFingerprint{info: after, size: after.Size(), modTime: after.ModTime()}
	if !sameTranscriptFingerprint(observed, handleAfter) || !sameTranscriptFingerprint(observed, pathAfter) {
		return nil, errTranscriptChanged
	}
	return data, nil
}

func validateTranscriptSnapshot(ctx context.Context, data []byte, startPosition int, reconstructAssistant bool) (int, string, error) {
	position := 0
	latestAssistant := ""
	var finalRecord []byte
	remaining := data
	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, "", fmt.Errorf("validate transcript canceled: %w", err)
		}
		line := remaining
		if newline := bytes.IndexByte(remaining, '\n'); newline >= 0 {
			line = remaining[:newline]
			remaining = remaining[newline+1:]
		} else {
			remaining = nil
		}

		linePosition := position
		position++
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		finalRecord = trimmed
		if reconstructAssistant && linePosition >= startPosition {
			if !json.Valid(trimmed) {
				return 0, "", errors.New("parse transcript record: invalid JSON")
			}
			// Claude's transcript is producer-owned compact JSONL. Most records are
			// user/tool metadata; avoid allocating an envelope for records that cannot
			// contain the assistant type value.
			if bytes.Contains(trimmed, assistantRecordMarker) {
				var envelope struct {
					Type    string          `json:"type"`
					Message json.RawMessage `json:"message"`
				}
				if err := json.Unmarshal(trimmed, &envelope); err != nil {
					return 0, "", fmt.Errorf("parse transcript record: %w", err)
				}
				if envelope.Type == envelopeTypeAssistant {
					if text := assistantText(envelope.Message); strings.TrimSpace(text) != "" {
						latestAssistant = text
					}
				}
			}
		}
	}

	if len(finalRecord) == 0 {
		return 0, "", errors.New("transcript has no JSONL records")
	}
	if !json.Valid(finalRecord) {
		return 0, "", errors.New("parse final transcript record: invalid JSON")
	}
	if err := ctx.Err(); err != nil {
		return 0, "", fmt.Errorf("validate transcript canceled: %w", err)
	}
	return position, latestAssistant, nil
}

func assistantText(message json.RawMessage) string {
	var parsed struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(message, &parsed) != nil {
		return ""
	}

	var text string
	if json.Unmarshal(parsed.Content, &text) == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(parsed.Content, &blocks) != nil {
		return ""
	}
	var textBlocks []string
	for _, block := range blocks {
		if block.Type == assistantContentBlockTypeText {
			textBlocks = append(textBlocks, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(textBlocks, "\n"))
}
