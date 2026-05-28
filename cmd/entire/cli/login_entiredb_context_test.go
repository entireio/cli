package cli

import (
	"testing"
)

func TestDefaultEntiredbContextName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		issuer  string
		handle  string
		want    string
		wantErr bool
	}{
		{
			name:   "host + handle",
			issuer: "https://us.auth.entire.io",
			handle: "alex",
			want:   "alex@us.auth.entire.io",
		},
		{
			name:   "trailing slash on issuer is normalised by caller",
			issuer: "https://eu.auth.entire.io",
			handle: "paul",
			want:   "paul@eu.auth.entire.io",
		},
		{
			name:   "empty handle falls back to bare host",
			issuer: "https://entire.io",
			handle: "",
			want:   "entire.io",
		},
		{
			name:    "issuer without host is rejected",
			issuer:  "not a url",
			handle:  "alex",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := defaultEntiredbContextName(tc.issuer, tc.handle)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
