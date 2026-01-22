package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	// MaxChunkSize is the maximum size for a single transcript chunk.
	// GitHub has a 100MB limit per blob, so we use 50MB to be safe.
	MaxChunkSize = 50 * 1024 * 1024 // 50MB

	// ChunkSuffix is the format for chunk file suffixes (e.g., ".001", ".002")
	ChunkSuffix = ".%03d"
)

// ChunkTranscript splits a transcript into chunks using the appropriate agent.
// If agentType is empty or the agent doesn't implement TranscriptChunker,
// falls back to JSONL (line-based) chunking.
func ChunkTranscript(content []byte, agentType string) ([][]byte, error) {
	if len(content) <= MaxChunkSize {
		return [][]byte{content}, nil
	}

	// Try to get the agent by type
	if agentType != "" {
		ag, err := GetByAgentType(agentType)
		if err == nil {
			if chunker, ok := ag.(TranscriptChunker); ok {
				chunks, chunkErr := chunker.ChunkTranscript(content, MaxChunkSize)
				if chunkErr != nil {
					return nil, fmt.Errorf("agent chunking failed: %w", chunkErr)
				}
				return chunks, nil
			}
		}
	}

	// Fall back to JSONL chunking (default)
	return ChunkJSONL(content, MaxChunkSize)
}

// ReassembleTranscript combines chunks back into a single transcript.
// If agentType is empty or the agent doesn't implement TranscriptChunker,
// falls back to JSONL (line-based) reassembly.
func ReassembleTranscript(chunks [][]byte, agentType string) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	if len(chunks) == 1 {
		return chunks[0], nil
	}

	// Try to get the agent by type
	if agentType != "" {
		ag, err := GetByAgentType(agentType)
		if err == nil {
			if chunker, ok := ag.(TranscriptChunker); ok {
				result, reassembleErr := chunker.ReassembleTranscript(chunks)
				if reassembleErr != nil {
					return nil, fmt.Errorf("agent reassembly failed: %w", reassembleErr)
				}
				return result, nil
			}
		}
	}

	// Fall back to JSONL reassembly (default)
	return ReassembleJSONL(chunks), nil
}

// ChunkJSONL splits JSONL content at line boundaries.
// This is the default chunking for agents using JSONL format (like Claude Code).
func ChunkJSONL(content []byte, maxSize int) ([][]byte, error) {
	lines := strings.Split(string(content), "\n")
	var chunks [][]byte
	var currentChunk strings.Builder

	for i, line := range lines {
		// Check if adding this line would exceed the chunk size
		lineWithNewline := line + "\n"

		// Check if a single line exceeds maxSize - this is an error since we can't split JSONL lines
		if len(lineWithNewline) > maxSize {
			return nil, fmt.Errorf("JSONL line %d exceeds maximum chunk size (%d bytes > %d bytes); cannot split a single JSON object", i+1, len(lineWithNewline), maxSize)
		}

		if currentChunk.Len()+len(lineWithNewline) > maxSize && currentChunk.Len() > 0 {
			// Save current chunk and start a new one
			chunks = append(chunks, []byte(strings.TrimSuffix(currentChunk.String(), "\n")))
			currentChunk.Reset()
		}
		currentChunk.WriteString(lineWithNewline)
	}

	// Add the last chunk if it has content
	if currentChunk.Len() > 0 {
		chunks = append(chunks, []byte(strings.TrimSuffix(currentChunk.String(), "\n")))
	}

	return chunks, nil
}

// ReassembleJSONL concatenates JSONL chunks with newlines.
func ReassembleJSONL(chunks [][]byte) []byte {
	var result strings.Builder
	for i, chunk := range chunks {
		result.Write(chunk)
		if i < len(chunks)-1 {
			result.WriteString("\n")
		}
	}
	return []byte(result.String())
}

// ChunkFileName returns the filename for a chunk at the given index.
// Index 0 returns the base filename, index 1+ returns with chunk suffix.
func ChunkFileName(baseName string, index int) string {
	if index == 0 {
		return baseName
	}
	return baseName + fmt.Sprintf(ChunkSuffix, index)
}

// ParseChunkIndex extracts the chunk index from a filename.
// Returns 0 for the base file (no suffix), or the chunk number for suffixed files.
// Returns -1 if the filename doesn't match the expected pattern.
func ParseChunkIndex(filename, baseName string) int {
	if filename == baseName {
		return 0
	}

	if !strings.HasPrefix(filename, baseName+".") {
		return -1
	}

	suffix := strings.TrimPrefix(filename, baseName+".")
	var index int
	if _, err := fmt.Sscanf(suffix, "%03d", &index); err != nil {
		return -1
	}
	return index
}

// SortChunkFiles sorts chunk filenames in order (base file first, then numbered chunks).
func SortChunkFiles(files []string, baseName string) []string {
	sorted := make([]string, len(files))
	copy(sorted, files)

	sort.Slice(sorted, func(i, j int) bool {
		idxI := ParseChunkIndex(sorted[i], baseName)
		idxJ := ParseChunkIndex(sorted[j], baseName)
		return idxI < idxJ
	})

	return sorted
}

// geminiTranscriptDetect is used for detecting Gemini JSON format.
type geminiTranscriptDetect struct {
	Messages []interface{} `json:"messages"`
}

// DetectAgentTypeFromContent detects the agent type from transcript content.
// Returns "Gemini CLI" if it appears to be Gemini JSON format, empty string otherwise.
// This is used when the agent type is unknown but we need to chunk/reassemble correctly.
func DetectAgentTypeFromContent(content []byte) string {
	// Quick check: Gemini JSON starts with { and has a messages array
	trimmed := strings.TrimSpace(string(content))
	if !strings.HasPrefix(trimmed, "{") {
		return ""
	}

	// Try to parse as Gemini JSON format (object with messages array)
	var transcript geminiTranscriptDetect
	if err := json.Unmarshal(content, &transcript); err != nil {
		return ""
	}

	// Must have at least one message to be considered Gemini format
	if len(transcript.Messages) > 0 {
		return "Gemini CLI"
	}

	return ""
}
