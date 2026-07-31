package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// TestEncodeTrailShowJSON pins the trail show --json payload: the resolved
// TrailResource with the detail-endpoint description and the browser URL the
// human output already surfaces.
func TestEncodeTrailShowJSON(t *testing.T) {
	t.Parallel()

	found := api.TrailResource{
		ID:     "tr_1",
		Number: 42,
		Title:  "Improve trail resume",
		Branch: "trail-resume",
		Base:   "main",
		Status: "open",
	}

	var out bytes.Buffer
	err := encodeTrailShowJSON(&out, found, "https://app.entire.io/t/42", "Long form description with <code> & links")
	if err != nil {
		t.Fatalf("encodeTrailShowJSON() error = %v", err)
	}

	// User-authored description text must not be HTML-escaped (jsonutil
	// convention): agents should read `<code> &` literally, not <.
	if !strings.Contains(out.String(), "<code> & links") {
		t.Errorf("output HTML-escapes the description: %s", out.String())
	}

	var payload map[string]any
	if unmarshalErr := json.Unmarshal(out.Bytes(), &payload); unmarshalErr != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", unmarshalErr, out.String())
	}
	for key, want := range map[string]any{
		"number": float64(42),
		"branch": "trail-resume",
		"title":  "Improve trail resume",
		"body":   "Long form description with <code> & links",
		"url":    "https://app.entire.io/t/42",
		"status": "open",
	} {
		if payload[key] != want {
			t.Errorf("payload[%q] = %v, want %v", key, payload[key], want)
		}
	}
}

// TestTrailShowCmdRegistersJSONFlag closes the one read-command gap in the
// trail group: show must accept --json like list, watch, and finding do.
func TestTrailShowCmdRegistersJSONFlag(t *testing.T) {
	t.Parallel()

	cmd := newTrailShowCmd()
	flag := cmd.Flags().Lookup("json")
	if flag == nil {
		t.Fatal("trail show has no --json flag")
	}
	if !strings.Contains(strings.ToLower(flag.Usage), "json") {
		t.Errorf("--json usage = %q, want a JSON description", flag.Usage)
	}
}
