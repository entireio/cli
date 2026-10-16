package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// SkippedLine describes one line Decode could not read.
type SkippedLine struct {
	Line   int // 1-based physical line number
	Reason string
}

// SkippedLinesError reports the lines Decode skipped.
type SkippedLinesError struct {
	Skipped []SkippedLine
}

func (e *SkippedLinesError) Error() string {
	if len(e.Skipped) == 0 {
		return "transcript: skipped 0 lines"
	}
	first := e.Skipped[0]
	return fmt.Sprintf("transcript: skipped %d line(s); first: line %d: %s", len(e.Skipped), first.Line, first.Reason)
}

type wireLine struct {
	V            *int            `json:"v"`
	Type         *string         `json:"type"`
	Agent        string          `json:"agent"`
	CLIVersion   string          `json:"cli_version"`
	TS           json.RawMessage `json:"ts"`
	ID           string          `json:"id"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	Content      json.RawMessage `json:"content"`
}

type wireBlock struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Text   *string         `json:"text"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Result *toolResultJSON `json:"result"`
}

// Decode reads Entire Transcript Format bytes into typed lines.
//
// Like go/parser.ParseFile, Decode returns a usable partial result together
// with the error: every readable line is returned, and if any line was
// skipped the error is a *SkippedLinesError listing each one. Callers choose
// their policy:
//
//	lines, err := transcript.Decode(b)
//	// strict:   if err != nil { return err }
//	// tolerant: log err; use lines
//
// A line is skipped when it is not a JSON object, has no "v", has a "v" other
// than 1, has no "type", or has a known field of the wrong JSON type ("ts" is
// the exception: a non-string "ts" is ignored). Blank lines are ignored.
// Unknown fields are ignored, and lines with an unknown "type" are returned as
// they are.
func Decode(b []byte) ([]Line, error) {
	var lines []Line
	var skipped []SkippedLine
	for i, raw := range bytes.Split(b, []byte{'\n'}) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		line, reason := decodeLine(raw)
		if reason != "" {
			skipped = append(skipped, SkippedLine{Line: i + 1, Reason: reason})
			continue
		}
		lines = append(lines, line)
	}
	if len(skipped) > 0 {
		return lines, &SkippedLinesError{Skipped: skipped}
	}
	return lines, nil
}

func decodeLine(raw []byte) (Line, string) {
	if raw[0] != '{' {
		return Line{}, "not a JSON object"
	}
	var w wireLine
	if err := json.Unmarshal(raw, &w); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return Line{}, "invalid field: " + typeErr.Field
		}
		return Line{}, "not a JSON object"
	}
	switch {
	case w.V == nil:
		return Line{}, "missing v"
	case *w.V != 1:
		return Line{}, fmt.Sprintf("unsupported v: %d", *w.V)
	case w.Type == nil || *w.Type == "":
		return Line{}, "missing type"
	}
	var ts string
	if json.Unmarshal(w.TS, &ts) != nil {
		ts = ""
	}
	return Line{
		Version:      *w.V,
		Type:         *w.Type,
		Agent:        w.Agent,
		CLIVersion:   w.CLIVersion,
		Timestamp:    ts,
		ID:           w.ID,
		InputTokens:  w.InputTokens,
		OutputTokens: w.OutputTokens,
		Blocks:       decodeContent(w.Content),
	}, ""
}

func decodeContent(content json.RawMessage) []Block {
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return nil
	}
	switch content[0] {
	case '"':
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return nil
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{BlockText, s}); err != nil {
			return nil
		}
		return []Block{{Type: BlockText, Text: s, Raw: bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})}}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(content, &items); err != nil {
			return nil
		}
		blocks := make([]Block, len(items))
		for i, item := range items {
			blocks[i] = decodeBlock(item)
		}
		return blocks
	default:
		return nil
	}
}

func decodeBlock(raw json.RawMessage) Block {
	var w wireBlock
	if err := json.Unmarshal(raw, &w); err != nil {
		return Block{Raw: raw}
	}
	b := Block{
		Type:  w.Type,
		ID:    w.ID,
		Name:  w.Name,
		Input: w.Input,
		Raw:   raw,
	}
	if w.Text != nil {
		b.Text = *w.Text
		if b.Type == "" {
			b.Type = BlockText
		}
	}
	if w.Result != nil {
		b.Result = &ToolResult{
			Output:     w.Result.Output,
			Status:     w.Result.Status,
			MatchCount: w.Result.MatchCount,
		}
		if f := w.Result.File; f != nil {
			b.Result.File = &ToolResultFile{FilePath: f.FilePath, NumLines: f.NumLines}
		}
	}
	return b
}

// Parse decodes a transcript in any supported format into typed lines.
//
// Input that is already in the Entire Transcript Format is passed to Decode
// and o is ignored; a caller holding a checkpoint boundary from
// ConvertWithBoundary slices the result with lines[boundary:]. Any other input
// is converted with Convert and the result decoded. A Convert error is
// returned as is.
func Parse(raw []byte, o Options) ([]Line, error) {
	if isEntireFormat(raw) {
		return Decode(raw)
	}
	converted, err := Convert(raw, o)
	if err != nil {
		return nil, err
	}
	return Decode(converted)
}

func isEntireFormat(raw []byte) bool {
	for len(raw) > 0 {
		var l []byte
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			l, raw = raw[:i], raw[i+1:]
		} else {
			l, raw = raw, nil
		}
		l = bytes.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		if l[0] != '{' {
			return false
		}
		var head struct {
			V    *int   `json:"v"`
			Type string `json:"type"`
		}
		if json.Unmarshal(l, &head) != nil || head.V == nil {
			return false
		}
		return head.Type != ""
	}
	return false
}
