package api

import "testing"

func TestDecodeNormalizedTrailJSONPreservesFreeFormMapKeys(t *testing.T) {
	t.Parallel()
	var got struct {
		ID       string         `json:"id"`
		Context  map[string]any `json:"context"`
		Labels   map[string]any `json:"labels"`
		ReviewID string         `json:"reviewId"`
	}
	data := []byte(`{
		"id":"t1",
		"context":{"my_custom_key":1,"nested":{"keep_snake_case":true}},
		"labels":{"team_a":true},
		"review_session_id":"rvw_1"
	}`)
	if err := decodeNormalizedTrailJSON(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Context["my_custom_key"]; !ok {
		t.Fatalf("context keys were re-cased: %#v", got.Context)
	}
	nested, ok := got.Context["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested context = %#v", got.Context["nested"])
	}
	if _, ok := nested["keep_snake_case"]; !ok {
		t.Fatalf("nested context keys were re-cased: %#v", nested)
	}
	if _, ok := got.Labels["team_a"]; !ok {
		t.Fatalf("label keys were re-cased: %#v", got.Labels)
	}
	if got.ReviewID != "rvw_1" {
		t.Fatalf("reviewId = %q, want rvw_1", got.ReviewID)
	}
}

func TestLegacyTrailRequestBodyPreservesFreeFormMapKeys(t *testing.T) {
	t.Parallel()
	body := struct {
		BranchName string         `json:"branchName"`
		Metadata   map[string]any `json:"metadata"`
	}{
		BranchName: "feature/test",
		Metadata: map[string]any{
			"userDefinedKey": 1,
			"already_snake":  2,
		},
	}
	normalized, err := legacyTrailRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := normalized.(map[string]any)
	if !ok {
		t.Fatalf("normalized body = %#v", normalized)
	}
	if got["branch_name"] != "feature/test" {
		t.Fatalf("typed key was not converted: %#v", got)
	}
	metadata, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v", got["metadata"])
	}
	if _, ok := metadata["userDefinedKey"]; !ok {
		t.Fatalf("metadata keys were re-cased: %#v", metadata)
	}
	if _, ok := metadata["already_snake"]; !ok {
		t.Fatalf("metadata keys were re-cased: %#v", metadata)
	}
}

func TestNormalizeTrailJSONKeySharesReviewAlias(t *testing.T) {
	t.Parallel()
	if got := NormalizeTrailJSONKey("review_session_id"); got != "reviewId" {
		t.Fatalf("key = %q, want reviewId", got)
	}
}
