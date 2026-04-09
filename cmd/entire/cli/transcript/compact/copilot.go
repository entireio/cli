package compact

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

func isCopilotFormat(content []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.Type {
		case "session.start", "user.message", "assistant.message", "tool.execution_complete":
			return true
		case transcript.TypeUser, transcript.TypeAssistant, "human", transcriptTypeMessage:
			return false
		}
	}
	if scanner.Err() != nil {
		return false
	}
	return false
}

type copilotLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type copilotAssistantData struct {
	MessageID    string               `json:"messageId"`
	Content      string               `json:"content"`
	ToolReqs     []copilotToolRequest `json:"toolRequests"`
	OutputTokens int                  `json:"outputTokens"`
}

type copilotToolRequest struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

type copilotToolResultData struct {
	ToolCallID string `json:"toolCallId"`
	Success    bool   `json:"success"`
	Result     struct {
		Content         string `json:"content"`
		DetailedContent string `json:"detailedContent"`
	} `json:"result"`
}

func compactCopilot(content []byte, opts MetadataFields) ([]byte, error) {
	base := newTranscriptLine(opts)
	var result []byte
	var pending *transcriptLine

	flushPending := func() {
		if pending == nil {
			return
		}
		appendLine(&result, *pending)
		pending = nil
	}

	reader := bufio.NewReader(bytes.NewReader(content))
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading copilot line: %w", err)
		}

		if len(bytes.TrimSpace(lineBytes)) > 0 {
			var line copilotLine
			if json.Unmarshal(lineBytes, &line) == nil {
				switch line.Type {
				case "user.message":
					flushPending()
					userLine := copilotUserLine(base, line)
					if userLine != nil {
						appendLine(&result, *userLine)
					}
				case "assistant.message":
					flushPending()
					pending = copilotAssistantLine(base, line)
				case "tool.execution_complete":
					if pending != nil {
						copilotInlineToolResult(pending, line)
					}
				}
			}
		}

		if err == io.EOF {
			break
		}
	}

	flushPending()
	return result, nil
}

func copilotUserLine(base transcriptLine, line copilotLine) *transcriptLine {
	var data struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(line.Data, &data) != nil {
		return nil
	}
	if data.Content == "" {
		return nil
	}

	contentJSON, err := json.Marshal([]userTextBlock{{Text: data.Content}})
	if err != nil {
		return nil
	}

	ts, err := json.Marshal(line.Timestamp)
	if err != nil {
		return nil
	}

	out := base
	out.Type = transcript.TypeUser
	out.TS = ts
	out.Content = contentJSON
	return &out
}

func copilotAssistantLine(base transcriptLine, line copilotLine) *transcriptLine {
	var data copilotAssistantData
	if json.Unmarshal(line.Data, &data) != nil {
		return nil
	}

	content := make([]map[string]json.RawMessage, 0, 1+len(data.ToolReqs))
	if data.Content != "" {
		tb, err := json.Marshal(transcript.ContentTypeText)
		if err == nil {
			txt, err := json.Marshal(data.Content)
			if err == nil {
				content = append(content, map[string]json.RawMessage{
					"type": tb,
					"text": txt,
				})
			}
		}
	}

	for _, tr := range data.ToolReqs {
		kind, err := json.Marshal(transcript.ContentTypeToolUse)
		if err != nil {
			continue
		}
		name, err := json.Marshal(tr.Name)
		if err != nil {
			continue
		}

		block := map[string]json.RawMessage{
			"type": kind,
			"name": name,
		}
		if tr.ToolCallID != "" {
			id, err := json.Marshal(tr.ToolCallID)
			if err == nil {
				block["id"] = id
			}
		}
		if len(tr.Arguments) > 0 {
			block["input"] = tr.Arguments
		}
		content = append(content, block)
	}

	if len(content) == 0 {
		return nil
	}

	contentJSON, err := json.Marshal(content)
	if err != nil {
		return nil
	}

	ts, err := json.Marshal(line.Timestamp)
	if err != nil {
		return nil
	}

	out := base
	out.Type = transcript.TypeAssistant
	out.TS = ts
	out.ID = data.MessageID
	out.OutputTokens = data.OutputTokens
	out.Content = contentJSON
	return &out
}

func copilotInlineToolResult(pending *transcriptLine, line copilotLine) {
	var data copilotToolResultData
	if json.Unmarshal(line.Data, &data) != nil || data.ToolCallID == "" {
		return
	}

	output := data.Result.Content
	if output == "" {
		output = data.Result.DetailedContent
	}

	var blocks []map[string]json.RawMessage
	if json.Unmarshal(pending.Content, &blocks) != nil {
		return
	}

	for i := len(blocks) - 1; i >= 0; i-- {
		if unquote(blocks[i]["type"]) != transcript.ContentTypeToolUse {
			continue
		}
		if unquote(blocks[i]["id"]) != data.ToolCallID {
			continue
		}
		blocks[i]["result"] = buildToolResult(toolResultEntry{
			output:  output,
			isError: !data.Success,
		})
		break
	}

	if contentJSON, err := json.Marshal(blocks); err == nil {
		pending.Content = contentJSON
	}
}
