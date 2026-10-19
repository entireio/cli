package cli

import "testing"

// Every provider's display and stored spellings must round-trip, or what
// `auth status` shows stops being what `grant add` accepts.
func TestProviderIdentity_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, provider, stored, display string
	}{
		{"github username", "github", "alice", "alice"},
		{"github username that looks minted", "github", "github-foo", "github-foo"},
		{"google minted handle", "google", "google-100164574874856813796", "100164574874856813796"},
		{"provider case is ignored", "Google", "google-1001", "1001"},
		{"unknown provider passes through", "gitlab", "gitlab-bob", "gitlab-bob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id := identityFor(tt.provider)
			if got := id.displayHandle(tt.stored); got != tt.display {
				t.Errorf("displayHandle(%q) = %q, want %q", tt.stored, got, tt.display)
			}
			if got := id.storedHandle(tt.display); got != tt.stored {
				t.Errorf("storedHandle(%q) = %q, want %q", tt.display, got, tt.stored)
			}
		})
	}
}

// A value copied from --json carries the stored spelling; typing it back must
// not mint the prefix a second time, and a prefix typed in any case is
// rebuilt in the canonical spelling the server minted.
func TestProviderIdentity_StoredFormIsAcceptedAsTyped(t *testing.T) {
	t.Parallel()
	for _, typed := range []string{"google-1001", "Google-1001", "GOOGLE-1001"} {
		if got := identityFor(providerGoogle).storedHandle(typed); got != "google-1001" {
			t.Errorf("storedHandle(%q) = %q, want google-1001", typed, got)
		}
	}
}

// Google documents the subject id as case-sensitive, so neither direction may
// fold it: two ids differing only in case are two different accounts.
func TestProviderIdentity_SubjectIDCaseIsPreserved(t *testing.T) {
	t.Parallel()
	id := identityFor(providerGoogle)
	if got := id.storedHandle("AbC1001"); got != "google-AbC1001" {
		t.Errorf("storedHandle(AbC1001) = %q, want google-AbC1001", got)
	}
	if got := id.storedHandle("Google-AbC1001"); got != "google-AbC1001" {
		t.Errorf("storedHandle(Google-AbC1001) = %q, want google-AbC1001", got)
	}
	if got := id.displayHandle("google-AbC1001"); got != "AbC1001" {
		t.Errorf("displayHandle(google-AbC1001) = %q, want AbC1001", got)
	}
}

func TestDisplayGranteeName(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"google:google-1001", "google:1001"},
		{"github:alice", "github:alice"},
		{"github:github-foo", "github:github-foo"},
		{"acme", "acme"},                                             // org or team name
		{"01HZX0000000000000000000AB", "01HZX0000000000000000000AB"}, // ULID
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := displayGranteeName(tt.in); got != tt.want {
				t.Errorf("displayGranteeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A subject id that itself begins with the prefix must not lose a layer on
// display: `google-1001` shown for `google-google-1001` is the stored form of
// another account, and pasting it back would grant to that account instead.
func TestProviderIdentity_PrefixedSubjectIDStaysUnambiguous(t *testing.T) {
	t.Parallel()
	id := identityFor(providerGoogle)
	for _, stored := range []string{"google-google-1001", "google-Google-1001"} {
		shown := id.displayHandle(stored)
		if shown != stored {
			t.Errorf("displayHandle(%q) = %q, want it shown in its stored form", stored, shown)
		}
		if back := id.storedHandle(shown); back != stored {
			t.Errorf("storedHandle(displayHandle(%q)) = %q, want the same account back", stored, back)
		}
	}
	// The ordinary case still de-duplicates, and still round-trips.
	if shown := id.displayHandle("google-1001"); shown != "1001" || id.storedHandle(shown) != "google-1001" {
		t.Errorf("google-1001 displayed as %q, want 1001 round-tripping to google-1001", shown)
	}
}
