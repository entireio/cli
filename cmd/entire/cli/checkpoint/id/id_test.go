package id

import (
	"testing"
)

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
