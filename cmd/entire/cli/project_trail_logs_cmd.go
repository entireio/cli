package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/redact"
)

// Unlike project reads, logs deliberately go through the web/BFF coordinator.
var newProjectLogClient = NewAuthenticatedAPIClient

const (
	localLogFormatTranscript = "transcript"
	localLogKindText         = "text"
	localLogKindMessage      = "message"
	localLogKindGap          = "gap"
	localLogKindComplete     = "complete"
)

func supportedLocalTranscriptAgent(name string) bool {
	return name == "claude-code" || name == "codex" || name == "pi"
}

func localLogFilters() []*regexp.Regexp {
	return []*regexp.Regexp{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`), regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|secret|password)["']?\s*[:=]\s*(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;]+)`)}
}

type localLogRecord struct {
	Sequence int64  `json:"sequence"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

func newProjectTrailLogsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "logs", Short: "Explicitly publish local agent output or watch a published stream"}
	var share bool
	var redactions []string
	var format, transcriptAgent string
	publish := &cobra.Command{Use: "publish [<trail>]", Short: "Publish selected agent transcript updates to the Trail audience (opt-in)", Args: cobra.MaximumNArgs(1),
		Long: "Read selected agent JSONL from stdin and publish it using the existing transcript converter. Specify --transcript-agent claude-code, codex, or pi. Use --format text for plain terminal lines. Transcript capture is bounded to 1 MiB / 4096 input lines; invalid input, limits, or branch changes stop capture with a gap. Images and hidden/runtime blocks are omitted. Requires --share-with-trail: everyone who can read the Trail can read these logs. Prototype: logs are held only in DO memory for at most one hour and may disappear at any time, even after an acknowledgement. The Trail buffer is limited to 4 MiB / 4096 records across 32 streams. Keep a local copy. Configured local secret/PII scanners and common credential patterns are applied; use --redact for additional patterns. The checkpoint-only OpenAI Privacy Filter is not run. Redaction is not a guarantee: do not pipe secrets or private transcripts. Input buffering is bounded; dropped/oversized records produce explicit gap records. No agent is launched or managed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !share {
				return errors.New("publishing shares logs with the Trail audience; pass --share-with-trail to opt in")
			}
			if format != localLogFormatTranscript && format != localLogKindText {
				return errors.New("--format must be transcript or text")
			}
			if format == localLogFormatTranscript && !supportedLocalTranscriptAgent(transcriptAgent) {
				return errors.New("--transcript-agent must be claude-code, codex, or pi for JSONL transcript input")
			}
			filters := localLogFilters()
			for _, raw := range redactions {
				r, err := regexp.Compile(raw)
				if err != nil {
					return fmt.Errorf("invalid --redact expression: %w", err)
				}
				filters = append(filters, r)
			}
			if err := strategy.EnsureRedactionConfigured(cmd.Context()); err != nil {
				return fmt.Errorf("configure log redaction: %w", err)
			}
			target, err := resolveProjectLogTarget(cmd, projectTrailSelector(args))
			if err != nil {
				return err
			}
			streamID := ulid.Make().String()
			path := target.path() + "/logs/" + streamID
			fmt.Fprintf(cmd.ErrOrStderr(), "Publishing selected input to Trail %s, log stream %s (visible to the Trail audience; volatile prototype buffer, keep a local copy).\n", target.TrailID, streamID)
			insecure, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // registered on the trail root
			send := func(ctx context.Context, records []localLogRecord) error {
				return appendLocalLogBatch(ctx, path, streamID, records, func(ctx context.Context) (*api.Client, error) { return newProjectLogClient(ctx, insecure) })
			}
			if format == localLogFormatTranscript {
				return publishLocalTranscript(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), transcriptAgent, filters, send)
			}
			return publishLocalLogInput(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), filters, send)
		}}
	addProjectTrailSelectorFlags(publish)
	publish.Flags().StringVar(&format, "format", localLogFormatTranscript, "Input format: transcript (agent JSONL) or text (plain lines)")
	publish.Flags().StringVar(&transcriptAgent, "transcript-agent", "", "Selected transcript format: claude-code, codex, or pi; no automatic discovery")
	publish.Flags().BoolVar(&share, "share-with-trail", false, "Confirm that selected input may be shared with everyone who can read this Trail")
	publish.Flags().StringArrayVar(&redactions, "redact", nil, "Additional regular expression to replace with [REDACTED] before upload (repeatable)")
	var streamID string
	watch := &cobra.Command{Use: "watch [<trail>]", Short: "Watch one local agent log stream", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := ulid.ParseStrict(streamID); err != nil {
			return errors.New("--stream must be a log stream ULID")
		}
		target, err := resolveProjectLogTarget(cmd, projectTrailSelector(args))
		if err != nil {
			return err
		}
		insecure, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // registered on the trail root
		return watchLocalLogs(cmd.Context(), target.path()+"/logs/"+streamID+"/watch", cmd.OutOrStdout(), func(ctx context.Context) (*api.Client, error) { return newProjectLogClient(ctx, insecure) })
	}}
	addProjectTrailSelectorFlags(watch)
	watch.Flags().StringVar(&streamID, "stream", "", "Log stream ID printed by publish")
	cmd.AddCommand(publish, watch)
	return cmd
}

// Explicit IDs/numbers use admission, never the editor-backed detail payload.
// Optional branch/change discovery still uses the shared project context resolver.
func resolveProjectLogTarget(cmd *cobra.Command, selector string) (*projectTrailTarget, error) {
	_, change := parseProjectTrailChangeSelector(selector)
	if selector == "" || trailBranchFlag(cmd) != "" || change {
		return resolveProjectTrail(cmd, selector)
	}
	if err := validateProjectTrailSelector(selector); err != nil {
		return nil, err
	}
	target, err := resolveProjectTrailCollection(cmd)
	if err != nil {
		return nil, err
	}
	target.TrailID = selector
	if !looksLikeULID(selector) {
		if err := resolveProjectLogNumber(cmd.Context(), target); err != nil {
			return nil, err
		}
	}
	return target, nil
}

func resolveProjectLogNumber(ctx context.Context, target *projectTrailTarget) error {
	ctx, cancel := context.WithTimeout(ctx, projectTrailListTimeout)
	defer cancel()
	var identity struct {
		TrailID   string `json:"trail_id"`
		ProjectID string `json:"project_id"`
		Number    int    `json:"number"`
	}
	if _, err := target.Client.ProjectTrailRequest(ctx, http.MethodGet, target.path()+"/logs/access", nil, nil, &identity); err != nil {
		return fmt.Errorf("resolve log Trail number: %w", err)
	}
	if _, err := ulid.ParseStrict(identity.TrailID); err != nil || identity.ProjectID != target.ProjectID || strconv.Itoa(identity.Number) != target.TrailID {
		return errors.New("log admission identity does not match the requested Trail")
	}
	target.TrailID, target.Number = identity.TrailID, identity.Number
	return nil
}

// A fixed non-identity field scans the whole line using the configured local
// scanners. JSONLBytes preserves fail-closed scanner errors; it does not invoke
// OPF or shell out. Never fall back to raw content after a scan error.
func redactLocalLogLine(text string) (string, error) {
	line := struct {
		Output string `json:"output"`
	}{Output: text}
	encoded, err := json.Marshal(line)
	if err != nil {
		return "", fmt.Errorf("encode log for redaction: %w", err)
	}
	clean, err := redact.JSONLBytes(encoded)
	if err != nil {
		return "", fmt.Errorf("scan log: %w", err)
	}
	if err := json.Unmarshal(clean.Bytes(), &line); err != nil {
		return "", fmt.Errorf("decode scanned log: %w", err)
	}
	return line.Output, nil
}

// The reader never waits for network IO. An unavailable sender causes explicit
// loss reporting while stdin continues draining, rather than blocking an agent
// writing a pipe. Memory is bounded to 64 records plus one 16-record batch.
func publishLocalLogInput(ctx context.Context, input io.Reader, diagnostics io.Writer, filters []*regexp.Regexp, send func(context.Context, []localLogRecord) error) error {
	return publishLocalLogRecords(ctx, diagnostics, send, func(records chan<- localLogRecord, dropped *atomic.Int64) error {
		reader := bufio.NewReaderSize(input, 8192)
		oversized := false
		for {
			line, err := reader.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				oversized = true
				continue
			}
			if oversized || !utf8.Valid(line) || bytes.ContainsRune(line, '\x00') {
				dropped.Add(1)
				oversized = false
			} else if len(line) > 0 {
				text := tuiutil.SanitizeTerminalText(strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"))
				text, scanErr := redactLocalLogLine(text)
				if scanErr != nil {
					dropped.Add(1)
				} else {
					for _, filter := range filters {
						text = filter.ReplaceAllString(text, "[REDACTED]")
					}
					if len(text) > 4096 {
						dropped.Add(1)
					} else {
						select {
						case records <- localLogRecord{Kind: localLogKindText, Text: text}:
						default:
							dropped.Add(1)
						}
					}
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("read log line: %w", err)
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	})
}

func publishLocalLogRecords(ctx context.Context, diagnostics io.Writer, send func(context.Context, []localLogRecord) error, produce func(chan<- localLogRecord, *atomic.Int64) error) error {
	records := make(chan localLogRecord, 64)
	var dropped atomic.Int64
	readError := make(chan error, 1)
	go func() {
		defer close(records)
		if err := produce(records, &dropped); err != nil {
			readError <- err
		}
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	next := int64(1)
	batch := make([]localLogRecord, 0, 16)
	batchBytes := 0
	var uploadErr error
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if uploadErr == nil {
			if err := send(ctx, batch); err != nil {
				uploadErr = err
				fmt.Fprintln(diagnostics, "Log upload stopped; remaining input will be drained but NOT uploaded:", err)
			}
		}
		batch = batch[:0]
		batchBytes = 0
	}
	add := func(record localLogRecord) {
		record.Sequence = next
		next++
		// JSON escaping (for example '<' -> '\\u003c') can exceed the wire
		// limit even when the unencoded text fits the DO's byte quota.
		encoded, err := json.Marshal(map[string]any{"records": append(batch, record)})
		if err != nil {
			uploadErr = fmt.Errorf("encode log batch: %w", err)
			return
		}
		if len(batch) > 0 && (len(encoded) > 96*1024 || batchBytes+len(record.Text)+32 > 65536) {
			flush()
		}
		batch = append(batch, record)
		batchBytes += len(record.Text) + 32
		if len(batch) >= 15 {
			flush()
		}
	}
	gaps := func() {
		if n := dropped.Swap(0); n > 0 {
			fmt.Fprintf(diagnostics, "Log capture gap: %d records dropped.\n", n)
			add(localLogRecord{Kind: localLogKindGap, Text: fmt.Sprintf("%d records dropped (local buffer, size, encoding, or redaction failure)", n)})
		}
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("publish local logs: %w", ctx.Err())
		case record, ok := <-records:
			gaps()
			if !ok {
				select {
				case err := <-readError:
					add(localLogRecord{Kind: localLogKindGap, Text: "local input read failed"})
					flush()
					return fmt.Errorf("read local logs: %w", err)
				default:
				}
				add(localLogRecord{Kind: localLogKindComplete, Text: ""})
				flush()
				return uploadErr
			}
			add(record)
		case <-ticker.C:
			gaps()
			flush()
		}
	}
}

func appendLocalLogBatch(ctx context.Context, path, streamID string, records []localLogRecord, client func(context.Context) (*api.Client, error)) error {
	var last error
	for attempt := range 3 {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c, err := client(cctx)
		if err == nil {
			var resp *http.Response
			resp, err = c.Put(cctx, path, map[string]any{"records": records})
			if err == nil {
				var ack struct {
					ID     string `json:"id"`
					Cursor int64  `json:"cursor"`
				}
				switch resp.StatusCode {
				case http.StatusOK:
					err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&ack)
					if err == nil && (ack.ID != streamID || ack.Cursor != records[len(records)-1].Sequence) {
						err = errors.New("invalid log acknowledgement")
					}
				case http.StatusConflict:
					err = errors.New("log buffer lost or sequence conflict; start a new publishing stream")
				default:
					err = fmt.Errorf("log append refused (HTTP %d)", resp.StatusCode)
				}
				_ = resp.Body.Close()
				cancel()
				if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout {
					return err
				}
			}
		}
		cancel()
		if err == nil {
			return nil
		}
		last = err
		timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("append local logs: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("log batch unacknowledged after retries: %w", last)
}

func watchLocalLogs(ctx context.Context, path string, out io.Writer, client func(context.Context) (*api.Client, error)) error {
	cursor := ""
	var sequence int64
	backoff := 500 * time.Millisecond
	retry := func() error {
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		backoff = min(backoff*2, 30*time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	for ctx.Err() == nil {
		c, err := client(ctx)
		if err != nil {
			return err
		}
		resp, err := c.GetStream(ctx, path, http.Header{"Accept": []string{"text/event-stream"}, "Last-Event-ID": []string{cursor}})
		if err != nil {
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				if err := retry(); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("log watch refused (HTTP %d)", resp.StatusCode)
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 16384), 65536)
		event, id, data := "", "", ""
		done := false
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				switch event {
				case "log":
					var record localLogRecord
					if err := json.Unmarshal([]byte(data), &record); err != nil {
						_ = resp.Body.Close()
						return fmt.Errorf("decode log record: %w", err)
					}
					if record.Sequence < 1 || id != strconv.FormatInt(record.Sequence, 10) ||
						(record.Kind != localLogKindText && record.Kind != localLogKindMessage && record.Kind != localLogKindGap && record.Kind != localLogKindComplete) || len(record.Text) > 8192 {
						_ = resp.Body.Close()
						return errors.New("invalid log record or cursor")
					}
					if record.Sequence <= sequence {
						break
					}
					if record.Sequence != sequence+1 {
						_ = resp.Body.Close()
						return errors.New("log replay gap")
					}
					if record.Kind != localLogKindComplete {
						if _, err := fmt.Fprintln(out, tuiutil.SanitizeTerminalText(record.Text)); err != nil {
							_ = resp.Body.Close()
							return fmt.Errorf("write log output: %w", err)
						}
					}
					cursor, sequence = id, record.Sequence
					backoff = 500 * time.Millisecond
				case "end":
					done = true
				case "reset_required":
					_ = resp.Body.Close()
					return errors.New("log buffer lost or expired; start a new publishing stream")
				case "forbidden":
					_ = resp.Body.Close()
					return errors.New("log watch stopped: access revoked")
				}
				event, id, data = "", "", ""
			}
		}
		err = scanner.Err()
		_ = resp.Body.Close()
		if done {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("watch local logs: %w", ctx.Err())
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("read log frame: %w", err)
		}
		if err := retry(); err != nil {
			return err
		}
	}
	return fmt.Errorf("watch local logs: %w", ctx.Err())
}
