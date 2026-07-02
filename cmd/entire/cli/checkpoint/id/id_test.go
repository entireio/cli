package id

import (
	"encoding/json"
	"testing"
)

// A representative ULID (Crockford base32, 26 chars) used across tests.
const sampleULID = "01KVBJCWYA4YW6J5M9GP655HZN"

func TestCheckpointID_Methods(t *testing.T) {
	t.Run("String", func(t *testing.T) {
		id := CheckpointID("a1b2c3d4e5f6")
		if id.String() != "a1b2c3d4e5f6" {
			t.Errorf("String() = %q, want %q", id.String(), "a1b2c3d4e5f6")
		}
	})

	t.Run("IsEmpty", func(t *testing.T) {
		if !EmptyCheckpointID.IsEmpty() {
			t.Error("EmptyCheckpointID.IsEmpty() should return true")
		}
		id := CheckpointID("a1b2c3d4e5f6")
		if id.IsEmpty() {
			t.Error("non-empty CheckpointID.IsEmpty() should return false")
		}
	})

	t.Run("Path", func(t *testing.T) {
		id := CheckpointID("a1b2c3d4e5f6")
		want := "a1/b2c3d4e5f6"
		if id.Path() != want {
			t.Errorf("Path() = %q, want %q", id.Path(), want)
		}
	})
}

func TestNewCheckpointID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "valid 12-char hex",
			input:   "a1b2c3d4e5f6",
			wantErr: false,
		},
		{
			name:    "too short",
			input:   "a1b2c3",
			wantErr: true,
		},
		{
			name:    "too long",
			input:   "a1b2c3d4e5f6789012",
			wantErr: true,
		},
		{
			name:    "non-hex characters",
			input:   "a1b2c3d4e5gg",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "valid ULID",
			input:   sampleULID,
			wantErr: false,
		},
		{
			name:    "ULID with excluded letter",
			input:   "01KVBJCWYA4YW6J5M9GP655HZI", // contains I
			wantErr: true,
		},
		{
			name:    "lowercase ULID is not valid",
			input:   "01kvbjcwya4yw6j5m9gp655hzn",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := NewCheckpointID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if id.String() != tt.input {
					t.Errorf("String() = %q, want %q", id.String(), tt.input)
				}
			}
		})
	}
}

func TestGenerate(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate() returned error: %v", err)
	}
	if id.IsEmpty() {
		t.Error("Generate() returned empty ID")
	}
	if len(id.String()) != 12 {
		t.Errorf("Generate() length = %d, want 12", len(id.String()))
	}
}

func TestKindOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  Kind
	}{
		{"legacy hex", "a1b2c3d4e5f6", KindLegacy},
		{"legacy all digits", "012345678901", KindLegacy},
		{"ulid", sampleULID, KindULID},
		{"ulid all valid base32", "0123456789ABCDEFGHJKMNPQRS", KindULID},
		// Right charset/length but the timestamp overflows (first char > 7);
		// oklog/ulid rejects it where a plain char-class regex would not.
		{"ulid timestamp overflow", "8123456789ABCDEFGHJKMNPQRS", KindUnknown},
		{"uppercase hex is not legacy", "A1B2C3D4E5F6", KindUnknown},
		{"ulid wrong length", "01KVBJCWYA4YW6J5M9GP655HZ", KindUnknown},
		{"ulid with excluded I", "01KVBJCWYA4YW6J5M9GP655HZI", KindUnknown},
		{"ulid with excluded L", "01KVBJCWYA4YW6J5M9GP655HZL", KindUnknown},
		{"ulid with excluded O", "01KVBJCWYA4YW6J5M9GP655HZO", KindUnknown},
		{"ulid with excluded U", "01KVBJCWYA4YW6J5M9GP655HZU", KindUnknown},
		{"empty", "", KindUnknown},
		{"garbage", "not-an-id", KindUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := KindOf(tt.input); got != tt.want {
				t.Errorf("KindOf(%q) = %v, want %v", tt.input, got, tt.want)
			}
			if got := CheckpointID(tt.input).Kind(); got != tt.want {
				t.Errorf("CheckpointID(%q).Kind() = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCheckpointID_ShardFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Every format shards on the LAST two chars (single positional rule).
		{"legacy", "a1b2c3d4e5f6", "f6"},
		{"legacy other", "abcdef123456", "56"},
		{"ulid", sampleULID, "ZN"},
		{"ulid trailing", "0123456789ABCDEFGHJKMNPQRS", "RS"},
		{"unknown", "XYZ", "YZ"},
		// Short-string fallbacks.
		{"empty", "", ""},
		{"one char", "a", "a"},
		{"two chars", "ab", "ab"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CheckpointID(tt.input).ShardFor(); got != tt.want {
				t.Errorf("CheckpointID(%q).ShardFor() = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateAcceptsBothFormats(t *testing.T) {
	t.Parallel()
	if err := Validate("a1b2c3d4e5f6"); err != nil {
		t.Errorf("Validate(legacy hex) = %v, want nil", err)
	}
	if err := Validate(sampleULID); err != nil {
		t.Errorf("Validate(ULID) = %v, want nil", err)
	}
	if err := Validate("nope"); err == nil {
		t.Error("Validate(garbage) = nil, want error")
	}
}

func TestUnmarshalJSON_ULIDRoundTrip(t *testing.T) {
	t.Parallel()
	t.Run("ULID round-trips", func(t *testing.T) {
		t.Parallel()
		var id CheckpointID
		if err := json.Unmarshal([]byte(`"`+sampleULID+`"`), &id); err != nil {
			t.Fatalf("unmarshal ULID: %v", err)
		}
		if id.String() != sampleULID {
			t.Errorf("got %q, want %q", id, sampleULID)
		}
		out, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(out) != `"`+sampleULID+`"` {
			t.Errorf("marshal = %s, want %q", out, sampleULID)
		}
	})

	t.Run("empty string is EmptyCheckpointID", func(t *testing.T) {
		t.Parallel()
		var id CheckpointID
		if err := json.Unmarshal([]byte(`""`), &id); err != nil {
			t.Fatalf("unmarshal empty: %v", err)
		}
		if !id.IsEmpty() {
			t.Errorf("empty string should unmarshal to EmptyCheckpointID, got %q", id)
		}
	})

	t.Run("invalid string still rejected", func(t *testing.T) {
		t.Parallel()
		var id CheckpointID
		if err := json.Unmarshal([]byte(`"not-a-valid-id"`), &id); err == nil {
			t.Error("expected error unmarshalling invalid checkpoint ID, got nil")
		}
	})
}

func TestCheckpointID_Path(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Standard 12-char IDs
		{"a1b2c3d4e5f6", "a1/b2c3d4e5f6"},
		{"abcdef123456", "ab/cdef123456"},
		// Edge cases: short strings (shouldn't happen in practice, but test the fallback)
		{"", ""},
		{"a", "a"},
		{"ab", "ab"},
		{"abc", "ab/c"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := CheckpointID(tt.input).Path()
			if got != tt.want {
				t.Errorf("CheckpointID(%q).Path() = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
