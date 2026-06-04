package api

import "testing"

func TestTrailResourceToMetadataUsesID(t *testing.T) {
	t.Parallel()

	metadata := (&TrailResource{ID: "trail-db-id", Branch: "feature/x"}).ToMetadata()
	if got := metadata.TrailID.String(); got != "trail-db-id" {
		t.Fatalf("metadata TrailID = %q, want stable API id", got)
	}
}
