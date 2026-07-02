// Package id provides the CheckpointID type for identifying checkpoints.
// This is a separate package to avoid import cycles between paths, trailers, and checkpoint.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	ulid "github.com/oklog/ulid/v2"
)

// CheckpointID identifies a checkpoint. It comes in two formats: a legacy
// 12-character lowercase hex ID and a 26-character Crockford base32 ULID (see
// Kind / CheckpointPattern). It links code commits to their checkpoint metadata.
//
//nolint:recvcheck // UnmarshalJSON requires pointer receiver, others use value receiver - standard pattern
type CheckpointID string

// EmptyCheckpointID represents an unset or invalid checkpoint ID.
const EmptyCheckpointID CheckpointID = ""

// Pattern is the regex pattern for a legacy checkpoint ID: exactly 12 lowercase
// hex characters. Exported for use in other packages (e.g., trailers) to avoid
// pattern duplication. It is also reused by investigate/provenance for *run IDs*,
// which are always 12-hex — do NOT widen this to include ULIDs; use
// CheckpointPattern for matching a checkpoint ID that may be either format.
const Pattern = `[0-9a-f]{12}`

// ulidPattern is the regex SHAPE of a ULID checkpoint ID: 26 Crockford base32
// characters (digits plus uppercase A-Z excluding I, L, O, U). It exists only to
// compose CheckpointPattern for extracting a candidate ID from free text; it is
// deliberately NOT exported and NOT the validator. Authoritative validation
// decodes the value via oklog/ulid (see KindOf/isULID), which additionally
// rejects e.g. a timestamp overflow the char class alone would accept.
const ulidPattern = `[0-9ABCDEFGHJKMNPQRSTVWXYZ]{26}`

// CheckpointPattern matches a checkpoint ID in free text in either format
// (legacy 12-hex or ULID). Use this — not Pattern — when scanning text such as
// the Entire-Checkpoint commit trailer for a candidate checkpoint ID, then
// validate the captured token via NewCheckpointID/Validate (CheckpointPattern is
// a loose shape, not authoritative validation).
const CheckpointPattern = `(?:` + Pattern + `|` + ulidPattern + `)`

// ShortIDLength is the standard length for truncating IDs for display purposes.
// Used for tool use IDs, session IDs, and commit hashes in logs and messages.
const ShortIDLength = 12

// checkpointIDRegex validates the legacy format: exactly 12 lowercase hex characters.
var checkpointIDRegex = regexp.MustCompile(`^` + Pattern + `$`)

// isULID reports whether s is a ULID in canonical form, decoded via oklog/ulid —
// the same library that will generate ULIDs — so validation and generation agree
// by construction. ParseStrict enforces the 26-char length, the Crockford
// alphabet, and the timestamp-overflow bound (first character must be 0-7). The
// round-trip (v.String() == s) additionally requires the canonical uppercase
// encoding: it rejects lowercase and Crockford-normalized aliases (e.g. I/L→1,
// O→0) that ParseStrict would otherwise accept but we never emit.
func isULID(s string) bool {
	v, err := ulid.ParseStrict(s)
	return err == nil && v.String() == s
}

// Kind classifies a checkpoint ID by its format: legacy 12-hex or ULID.
type Kind int

const (
	// KindUnknown is a string matching neither the legacy hex nor the ULID format.
	KindUnknown Kind = iota
	// KindLegacy is a 12-character lowercase hex ID (the format Generate emits).
	KindLegacy
	// KindULID is a 26-character Crockford base32 ULID.
	KindULID
)

// KindOf classifies a checkpoint ID string. It does not error: an unrecognized
// string is KindUnknown, which callers handle conservatively.
func KindOf(s string) Kind {
	switch {
	case checkpointIDRegex.MatchString(s):
		return KindLegacy
	case isULID(s):
		return KindULID
	default:
		return KindUnknown
	}
}

// Kind classifies this checkpoint ID.
func (id CheckpointID) Kind() Kind {
	return KindOf(string(id))
}

// ShardFor returns the two-character shard for storing this ID under a
// per-checkpoint git ref (refs/entire/checkpoints/<shard>/<id>): the LAST two
// characters of the ID, for BOTH supported formats.
//
// A single positional rule (independent of the ID's Kind) keeps ref naming
// robust for legacy and ULID IDs alike and impossible to compute inconsistently
// between callers. The suffix spreads checkpoints evenly across buckets for
// either format: a legacy hex ID is random throughout, and a ULID's leading
// characters encode its timestamp (barely varying between nearby checkpoints)
// while its trailing characters are random — so sharding on the suffix keeps the
// distribution even while the ID itself stays lexicographically sortable.
//
// This is the git-refs ref namespace only; the entire/checkpoints/v1 branch tree
// keeps its own independent first-two layout (see Path). For an ID shorter than
// two characters the whole ID is returned.
func (id CheckpointID) ShardFor() string {
	s := string(id)
	if len(s) < 2 {
		return s
	}
	return s[len(s)-2:]
}

// NewCheckpointID creates a CheckpointID from a string, validating its format.
// Returns an error unless the string is a valid checkpoint ID (12-char hex or ULID).
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
//
// Generation stays 12-hex regardless of storage backend. Emitting ULIDs is a
// separate, store-coupled change (new checkpoints get a ULID only under the
// git-refs store); this package only recognizes/validates both formats.
func Generate() (CheckpointID, error) {
	bytes := make([]byte, 6) // 6 bytes = 12 hex chars
	if _, err := rand.Read(bytes); err != nil {
		return EmptyCheckpointID, fmt.Errorf("failed to generate random checkpoint ID: %w", err)
	}
	return CheckpointID(hex.EncodeToString(bytes)), nil
}

// Validate checks if a string is a valid checkpoint ID format: either a legacy
// 12-character lowercase hex ID or a 26-character Crockford base32 ULID.
// Returns an error if invalid, nil if valid.
func Validate(s string) error {
	if KindOf(s) == KindUnknown {
		return fmt.Errorf("invalid checkpoint ID %q: must be 12 lowercase hex characters or a 26-character ULID", s)
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

// Path returns the sharded path for this checkpoint ID on entire/checkpoints/v1.
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
// Returns an error unless the JSON string is a valid checkpoint ID (12-char hex
// or ULID). Empty strings are allowed and result in EmptyCheckpointID.
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
