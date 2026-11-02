// Package geminilegacy reads Gemini CLI transcripts stored in checkpoints
// written before Gemini CLI support was removed.
//
// Entire no longer captures Gemini sessions, but checkpoints tagged
// agent.AgentTypeGemini still exist in users' metadata branches. Their
// transcripts are a single JSON document ({"messages":[...]}) rather than
// JSONL, so explain, summaries, and chunk reassembly need this parser to read
// them. Nothing here writes a Gemini transcript.
package geminilegacy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message type constants for Gemini transcripts.
const (
	MessageTypeUser   = "user"
	MessageTypeGemini = "gemini"
)

// Transcript is the top-level structure of a Gemini session file.
type Transcript struct {
	Messages []Message `json:"messages"`
}

// Message is a single message in the transcript.
type Message struct {
	ID        string     `json:"id,omitempty"` // UUID for the message
	Type      string     `json:"type"`         // MessageTypeUser or MessageTypeGemini
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
}

// UnmarshalJSON handles both string and array content formats in Gemini transcripts.
// User messages use: "content": [{"text": "..."}] (array of objects)
// Gemini messages use: "content": "response text" (string)
func (m *Message) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid infinite recursion
	type Alias Message
	aux := &struct {
		*Alias

		Content json.RawMessage `json:"content,omitempty"`
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	if len(aux.Content) == 0 || string(aux.Content) == "null" {
		m.Content = ""
		return nil
	}

	// Try string first (most common for gemini messages)
	var strContent string
	if err := json.Unmarshal(aux.Content, &strContent); err == nil {
		m.Content = strContent
		return nil
	}

	// Try array of objects with "text" fields (user messages)
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(aux.Content, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		m.Content = strings.Join(texts, "\n")
		return nil
	}

	// Unknown format - leave content empty
	return nil
}

// ToolCall is a tool call in a gemini message.
type ToolCall struct {
	ID     string                 `json:"id"`
	Name   string                 `json:"name"`
	Args   map[string]interface{} `json:"args"`
	Status string                 `json:"status,omitempty"`
}

// ParseTranscript parses raw JSON content into a transcript structure.
func ParseTranscript(data []byte) (*Transcript, error) {
	var transcript Transcript
	if err := json.Unmarshal(data, &transcript); err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	return &transcript, nil
}

// SliceFromMessage returns a Gemini transcript scoped to messages starting from
// startMessageIndex. This is the Gemini equivalent of transcript.SliceFromLine —
// for Gemini's single JSON blob, scoping is done by message index rather than line offset.
// Returns the original data if startMessageIndex <= 0.
// Returns nil, nil if startMessageIndex exceeds the number of messages.
func SliceFromMessage(data []byte, startMessageIndex int) ([]byte, error) {
	if len(data) == 0 || startMessageIndex <= 0 {
		return data, nil
	}

	t, err := ParseTranscript(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for slicing: %w", err)
	}

	if startMessageIndex >= len(t.Messages) {
		return nil, nil
	}

	out, err := json.Marshal(&Transcript{Messages: t.Messages[startMessageIndex:]})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal scoped transcript: %w", err)
	}
	return out, nil
}

// ReassembleChunks merges Gemini JSON chunks by combining their message arrays.
// Transcripts over the chunk size limit were stored as several
// {"messages":[...]} documents; concatenating them as JSONL would produce
// invalid JSON. Messages are carried as raw JSON, not decoded into Message,
// so fields this package does not model survive reassembly byte for byte.
func ReassembleChunks(chunks [][]byte) ([]byte, error) {
	allMessages := []json.RawMessage{}

	for _, chunk := range chunks {
		var transcript struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(chunk, &transcript); err != nil {
			return nil, fmt.Errorf("failed to unmarshal chunk: %w", err)
		}
		allMessages = append(allMessages, transcript.Messages...)
	}

	result, err := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
	}{Messages: allMessages})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal reassembled transcript: %w", err)
	}
	return result, nil
}
