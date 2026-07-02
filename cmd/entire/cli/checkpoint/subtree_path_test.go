package checkpoint

import "testing"

func TestCheckpointSubtreePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base string
		segs []string
		want string
	}{
		// Per-checkpoint-ref root (basePath == "").
		{"ref root metadata", "", []string{"metadata.json"}, "metadata.json"},
		{"ref root session meta", "", []string{"0", "metadata.json"}, "0/metadata.json"},
		// v1 branch layout (basePath has a trailing slash — path.Join cleans it).
		{"v1 root metadata", "a3/b2c4d5e6f7/", []string{"metadata.json"}, "a3/b2c4d5e6f7/metadata.json"},
		{"v1 session meta", "a3/b2c4d5e6f7/", []string{"0", "metadata.json"}, "a3/b2c4d5e6f7/0/metadata.json"},
		{"v1 task file", "a3/b2c4d5e6f7/", []string{"tasks", "tool-1", "checkpoint.json"}, "a3/b2c4d5e6f7/tasks/tool-1/checkpoint.json"},
		// A clean dir base (no trailing slash) joins identically.
		{"clean session dir", "a3/b2c4d5e6f7/0", []string{"full.jsonl"}, "a3/b2c4d5e6f7/0/full.jsonl"},
		// No segments returns the cleaned base (trailing slash stripped).
		{"base only trailing slash", "a3/b2c4d5e6f7/", nil, "a3/b2c4d5e6f7"},
		// Ref root with no segments must stay "" (not path.Join's "." cleaning).
		{"ref root base only", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := checkpointSubtreePath(tt.base, tt.segs...); got != tt.want {
				t.Errorf("checkpointSubtreePath(%q, %v) = %q, want %q", tt.base, tt.segs, got, tt.want)
			}
		})
	}
}
