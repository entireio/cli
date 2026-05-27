package versioninfo

import "testing"

func TestDescribe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		info    buildInfo
		want    string
	}{
		{
			name:    "release passes through clean",
			version: "v0.6.3",
			info:    buildInfo{pseudoVersion: "v0.6.3", modified: false},
			want:    "v0.6.3",
		},
		{
			name:    "nightly passes through clean",
			version: "v0.6.3-nightly.202605270736.c94e9573",
			info:    buildInfo{pseudoVersion: "v0.6.3-...", modified: false},
			want:    "v0.6.3-nightly.202605270736.c94e9573",
		},
		{
			name:    "ldflags version gains dirty marker when build tree modified",
			version: "v0.6.3-nightly.202605270736.c94e9573-dev-15d80761",
			info:    buildInfo{pseudoVersion: "v0.6.3-...+dirty", modified: true},
			want:    "v0.6.3-nightly.202605270736.c94e9573-dev-15d80761+dirty",
		},
		{
			name:    "dirty marker not duplicated",
			version: "v0.6.3+dirty",
			info:    buildInfo{modified: true},
			want:    "v0.6.3+dirty",
		},
		{
			name:    "dev falls back to embedded pseudo-version",
			version: "dev",
			info:    buildInfo{pseudoVersion: "v0.6.3-0.20260527133156-15d80761c74b", modified: false},
			want:    "v0.6.3-0.20260527133156-15d80761c74b",
		},
		{
			name:    "dev falls back to dirty pseudo-version verbatim",
			version: "dev",
			info:    buildInfo{pseudoVersion: "v0.6.3-0.20260527133156-15d80761c74b+dirty", modified: true},
			want:    "v0.6.3-0.20260527133156-15d80761c74b+dirty",
		},
		{
			name:    "dev with no pseudo-version stays bare when clean",
			version: "dev",
			info:    buildInfo{pseudoVersion: "(devel)", modified: false},
			want:    "dev",
		},
		{
			name:    "dev with no pseudo-version gains dirty marker",
			version: "dev",
			info:    buildInfo{pseudoVersion: "(devel)", modified: true},
			want:    "dev+dirty",
		},
		{
			name:    "dev with no build info stays bare",
			version: "dev",
			info:    buildInfo{},
			want:    "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := describe(tt.version, tt.info); got != tt.want {
				t.Errorf("describe(%q, %+v) = %q, want %q", tt.version, tt.info, got, tt.want)
			}
		})
	}
}
