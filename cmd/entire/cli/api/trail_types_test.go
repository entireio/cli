package api

import (
	"encoding/json"
	"testing"
)

// TestTrailResourceDecodesServerURL covers the wire-compatibility matrix for the
// `url` field the API added:
//   - new cli + new api: the field decodes into TrailResource.URL and is used.
//   - old cli + new api: a client struct predating the field ignores the extra
//     key without error (Go's json.Unmarshal drops unknown fields), so an older
//     CLI keeps working against a newer server.
//
// (new cli + old api is exercised by trailDisplayURL's fallback in the cli pkg.)
func TestTrailResourceDecodesServerURL(t *testing.T) {
	t.Parallel()

	// Shape a newer server would emit: includes `url`.
	payload := []byte(`{"id":"t1","number":640,"url":"https://entire.io/gh/o/r/trails/640/slug","branch":"feat/x","title":"T"}`)

	// new cli + new api: URL is captured and available to display.
	var newClient TrailResource
	if err := json.Unmarshal(payload, &newClient); err != nil {
		t.Fatalf("new client failed to decode new payload: %v", err)
	}
	if newClient.URL != "https://entire.io/gh/o/r/trails/640/slug" {
		t.Fatalf("URL = %q, want server-provided url", newClient.URL)
	}

	// old cli + new api: a struct without a URL field must not choke on the
	// extra key, and still decodes the fields it knows about.
	var oldClient struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(payload, &oldClient); err != nil {
		t.Fatalf("old client rejected new payload with extra url field: %v", err)
	}
	if oldClient.Number != 640 || oldClient.Title != "T" {
		t.Fatalf("old client decoded wrong values: %+v", oldClient)
	}
}

func TestTrailResourceToMetadataUsesID(t *testing.T) {
	t.Parallel()

	metadata := (&TrailResource{ID: "trail-db-id", URL: "https://entire.io/gh/o/r/trails/9", Branch: "feature/x", Phase: "has_code"}).ToMetadata()
	if got := metadata.TrailID.String(); got != "trail-db-id" {
		t.Fatalf("metadata TrailID = %q, want stable API id", got)
	}
	if metadata.Phase != "has_code" {
		t.Fatalf("metadata Phase = %q, want has_code", metadata.Phase)
	}
	// The server-provided URL must propagate so callers relying on ToMetadata()
	// don't silently drop it.
	if metadata.URL != "https://entire.io/gh/o/r/trails/9" {
		t.Fatalf("metadata URL = %q, want propagated server url", metadata.URL)
	}
}
