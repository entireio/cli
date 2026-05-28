package cliapi

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/entireio/cli/internal/entiredb/core/api"
)

// LookupRef is one entry sent to or received from POST /api/v1/lookup.
// The JSON shape mirrors api/corev1.LookupRefResult; the local type
// stays as a separate definition so callers that pre-date corev1
// don't need to import it.
//
// REQUEST-vs-RESPONSE: the server's input schema is strict (huma's
// additionalProperties:false on api/corev1.LookupRef, which declares
// only `type` and `id`). The Name/OwnerID/OwnerType/ProjectID/URL
// fields here are RESPONSE-ONLY — populating them on a ref passed to
// Resolve will be rejected by huma's body validator with a 422.
// Callers that need to round-trip a previously-resolved row must
// strip the response fields first.
type LookupRef struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	OwnerID   string `json:"ownerId,omitempty"`
	OwnerType string `json:"ownerType,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	URL       string `json:"url,omitempty"`
}

// lookupBatchSize must stay ≤ the maxItems cap declared on
// api/corev1.BatchLookupInput.Body.Refs (currently `maxItems:"100"`).
// If the server lowers its cap, lower this in the same change —
// otherwise multi-batch Resolve calls fail with 422 instead of
// paging cleanly. Huma's maxItems is inclusive, so equality is fine.
const lookupBatchSize = 100

// Resolve calls POST /api/lookup with the given refs and returns the
// enriched rows. Refs above the server's per-request cap are sent in
// sequential chunks; results are concatenated in input order. IDs
// the caller can't read (or that don't exist) are silently dropped
// by the server and so will be absent from the result.
func Resolve(c *api.Client, refs []LookupRef) ([]LookupRef, error) {
	// Always return a non-nil slice. Callers JSON-marshal the result
	// straight to stdout, and `nil` would serialize as `null` — which
	// breaks `jq '.refs[]'` for users who legitimately have zero
	// accessible resources.
	out := []LookupRef{}
	if len(refs) == 0 {
		return out, nil
	}
	for start := 0; start < len(refs); start += lookupBatchSize {
		end := min(start+lookupBatchSize, len(refs))
		body := map[string]any{"refs": refs[start:end]}
		data, err := c.PostJSON("/api/v1/lookup", body)
		if err != nil {
			return nil, fmt.Errorf("lookup: %w", err)
		}
		var resp struct {
			Refs []LookupRef `json:"refs"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("decode lookup response: %w", err)
		}
		out = append(out, resp.Refs...)
	}
	return out, nil
}

// ListAccessibleAndResolve does the two-call dance behind
// `entire-project list` and `entire-repo list`: fetch the SpiceDB
// accessible ID set for resourceType (with the "read"-flavored
// permission), then call /api/lookup to enrich those IDs with names
// and identifying refs. Prints the enriched response as JSON to
// stdout. Used wherever a CLI verb needs to show "everything I can
// see" without inventing a per-type aggregate endpoint.
func ListAccessibleAndResolve(c *api.Client, resourceType, permission string) error {
	data, err := c.GetJSON("/api/v1/access/" + resourceType + "?permission=" + permission)
	if err != nil {
		return err
	}
	var access struct {
		ResourceIDs []string `json:"resourceIds"`
	}
	if err := json.Unmarshal(data, &access); err != nil {
		return fmt.Errorf("decode access list: %w", err)
	}
	refs := make([]LookupRef, 0, len(access.ResourceIDs))
	for _, id := range access.ResourceIDs {
		refs = append(refs, LookupRef{Type: resourceType, ID: id})
	}
	resolved, err := Resolve(c, refs)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(map[string]any{"refs": resolved}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
	return nil
}
