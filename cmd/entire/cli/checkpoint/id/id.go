// Package id provides the CheckpointID type for identifying checkpoints.
// This is a separate package to avoid import cycles between paths, trailers, and checkpoint.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// CheckpointID is a 12-character hex identifier for checkpoints.
// It's used to link code commits to metadata on the entire/sessions branch.
//
//nolint:recvcheck // UnmarshalJSON requires pointer receiver, others use value receiver - standard pattern
type CheckpointID string

// EmptyCheckpointID represents an unset or invalid checkpoint ID.
const EmptyCheckpointID CheckpointID = ""

// Pattern is the regex pattern for a valid checkpoint ID: exactly 12 lowercase hex characters.
// Exported for use in other packages (e.g., trailers) to avoid pattern duplication.
const Pattern = `[0-9a-f]{12}`

// checkpointIDRegex validates the format: exactly 12 lowercase hex characters.
var checkpointIDRegex = regexp.MustCompile(`^` + Pattern + `$`)

// NewCheckpointID creates a CheckpointID from a string, validating its format.
// Returns an error if the string is not a valid 12-character hex ID.
func NewCheckpointID(s string) (CheckpointID, error) {
	if err := Validate(s); err != nil {
		return EmptyCheckpointID, err
	}
	return CheckpointID(s), nil
}

// MustCheckpointID creates a CheckpointID from a string, panicking if invalid.
// Use only when the ID is known to be valid (e.g., from trusted sources).
func MustCheckpointID(s string) CheckpointID {
	id, err := NewCheckpointID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// Generate creates a new random 12-character hex checkpoint ID.
func Generate() (CheckpointID, error) {
	bytes := make([]byte, 6) // 6 bytes = 12 hex chars
	if _, err := rand.Read(bytes); err != nil {
		return EmptyCheckpointID, fmt.Errorf("failed to generate random checkpoint ID: %w", err)
	}
	return CheckpointID(hex.EncodeToString(bytes)), nil
}

// Validate checks if a string is a valid checkpoint ID format.
// Returns an error if invalid, nil if valid.
func Validate(s string) error {
	if !checkpointIDRegex.MatchString(s) {
		return fmt.Errorf("invalid checkpoint ID %q: must be 12 lowercase hex characters", s)
	}
	return nil
}

// String returns the checkpoint ID as a string.
func (id CheckpointID) String() string {
	return string(id)
}

// IsEmpty returns true if the checkpoint ID is empty or unset.
func (id CheckpointID) IsEmpty() bool {
	return id == EmptyCheckpointID
}

// Path returns the sharded path for this checkpoint ID on entire/sessions.
// Uses first 2 characters as shard (256 buckets), remaining as folder name.
// Example: "a3b2c4d5e6f7" -> "a3/b2c4d5e6f7"
func (id CheckpointID) Path() string {
	if len(id) < 3 {
		return string(id)
	}
	return string(id[:2]) + "/" + string(id[2:])
}

// MarshalJSON implements json.Marshaler.
func (id CheckpointID) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(string(id))
	if err != nil {
		return nil, fmt.Errorf("failed to marshal checkpoint ID: %w", err)
	}
	return data, nil
}

// UnmarshalJSON implements json.Unmarshaler with validation.
// Returns an error if the JSON string is not a valid 12-character hex ID.
// Empty strings are allowed and result in EmptyCheckpointID.
func (id *CheckpointID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("failed to unmarshal checkpoint ID: %w", err)
	}
	// Allow empty strings (represents unset checkpoint ID)
	if s == "" {
		*id = EmptyCheckpointID
		return nil
	}
	if err := Validate(s); err != nil {
		return err
	}
	*id = CheckpointID(s)
	return nil
}
