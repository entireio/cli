package cli

import (
	"context"
	"strings"
	"testing"
)

func TestDocRef_DeterministicAndOpaque(t *testing.T) {
	t.Parallel()
	a := docRef("01JRESULT0000000000000000")
	if a != docRef("01JRESULT0000000000000000") {
		t.Error("docRef must be deterministic")
	}
	if len(a) != 16 {
		t.Errorf("len = %d, want 16", len(a))
	}
	if strings.Contains(a, "01JRESULT") {
		t.Error("docRef must not embed the raw id")
	}
}

func TestNewSearchID_IsULIDShaped(t *testing.T) {
	t.Parallel()
	id := newSearchID()
	if len(id) != 26 {
		t.Fatalf("search id %q length = %d, want 26", id, len(id))
	}
	if id == newSearchID() {
		t.Error("consecutive search ids must differ")
	}
}

func TestParseExplainSearchHit(t *testing.T) {
	t.Parallel()
	const validULID = "01JXK9RSTQ4B7NW2VYFCH6M3DZ"
	tests := []struct {
		name, token, wantID string
		wantRank            int
	}{
		{"empty", "", "", 0},
		{"ulid only", validULID, validULID, 0},
		{"ulid with rank", validULID + ":3", validULID, 3},
		{"junk dropped entirely", "not-a-ulid:3", "", 0},
		{"short id dropped", "01JXK9:1", "", 0},
		{"bad rank keeps id", validULID + ":abc", validULID, 0},
		{"zero rank dropped", validULID + ":0", validULID, 0},
		{"negative rank dropped", validULID + ":-2", validULID, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotID, gotRank := parseExplainSearchHit(tc.token)
			if gotID != tc.wantID || gotRank != tc.wantRank {
				t.Errorf("parseExplainSearchHit(%q) = (%q, %d), want (%q, %d)", tc.token, gotID, gotRank, tc.wantID, tc.wantRank)
			}
		})
	}
}

func TestExplainSearchHitContextRoundTrip(t *testing.T) {
	t.Parallel()
	const validULID = "01JXK9RSTQ4B7NW2VYFCH6M3DZ"

	ctx := withExplainSearchHit(context.Background(), validULID+":2")
	searchID, rank := explainSearchHitFrom(ctx)
	if searchID != validULID || rank != 2 {
		t.Errorf("round trip = (%q, %d), want (%q, 2)", searchID, rank, validULID)
	}

	// Bare context carries nothing.
	searchID, rank = explainSearchHitFrom(context.Background())
	if searchID != "" || rank != 0 {
		t.Errorf("bare context = (%q, %d), want empty", searchID, rank)
	}

	// Invalid tokens never enter the context.
	searchID, rank = explainSearchHitFrom(withExplainSearchHit(context.Background(), "junk"))
	if searchID != "" || rank != 0 {
		t.Errorf("junk token = (%q, %d), want empty", searchID, rank)
	}
}
