package generic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const configFileName = "generic.json"

// Config defines the configuration for the generic agent adapter.
// It is loaded from `.entire/generic.json` in the repository root.
type Config struct {
	// TranscriptDir is the directory where session transcripts are stored.
	// Supports ~ expansion and environment variables.
	TranscriptDir string `json:"transcript_dir"`

	// TranscriptPattern is a glob pattern for matching transcript files (e.g., "*.jsonl").
	TranscriptPattern string `json:"transcript_pattern"`

	// AgentType is the display name for the agent (e.g., "OpenClaw", "AMP").
	// Defaults to "Generic Agent" if not set.
	AgentType string `json:"agent_type"`

	// SessionIDFrom specifies how to extract session IDs from transcript files.
	// "filename" (default): use the filename without extension.
	// "field:<name>": extract from a JSON field in the first JSONL line (e.g., "field:id").
	SessionIDFrom string `json:"session_id_from"`
}

// loadConfig reads the generic agent config from .entire/generic.json at the given repo root.
func loadConfig(repoRoot string) (*Config, error) {
	path := filepath.Join(repoRoot, ".entire", configFileName)
	data, err := os.ReadFile(path) //nolint:gosec // Path constructed from repo root
	if err != nil {
		return nil, fmt.Errorf("failed to read generic agent config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse generic agent config: %w", err)
	}

	// Apply defaults
	if cfg.TranscriptPattern == "" {
		cfg.TranscriptPattern = "*.jsonl"
	}
	if cfg.SessionIDFrom == "" {
		cfg.SessionIDFrom = "filename"
	}

	// Expand ~ in transcript dir
	if len(cfg.TranscriptDir) > 0 && cfg.TranscriptDir[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			cfg.TranscriptDir = filepath.Join(home, cfg.TranscriptDir[1:])
		}
	}

	return &cfg, nil
}
