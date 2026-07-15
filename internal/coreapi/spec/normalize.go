//go:build ignore

// Command normalize rewrites the fetched Core API spec into a form the Go
// client generator (ogen) turns into an ergonomic client. It reads the
// pristine upstream spec (core.openapi.json) and writes a generated,
// committed artifact (core.gen.json) that ogen consumes; the upstream
// file is never mutated, so a refresh is a clean `curl` overwrite.
//
// This command applies two transforms, documented below. The running
// checklist of upstream fixes lives in internal/coreapi/UPSTREAM.md.
//
// Transform 1 (codegen-ergonomics fold, not a bug workaround): fold every
// operation's explicit error responses (4xx/5xx) into a single "default"
// error, leaving the real success response (201 for creates, 200 for
// reads, 204 for deletes) untouched. The spec declares accurate success
// codes but enumerates each error status separately with no "default"; ogen
// turns that into a per-operation sum type that forces a type switch at
// every call site. All error responses reference the same ErrorModel, so
// folding them into one "default" is lossless and flips ogen into
// "convenient errors": `(*T, error)` with any non-2xx as a typed
// `*ErrorModelStatusCode`. Keeping the literal success code (rather than a
// "2XX" range) means ogen returns the success type directly — no
// `*…StatusCode` wrapper to unwrap. This stays until/unless the spec grows
// a shared "default" response.
//
// Transform 2 (forward-compat, display-only read enums): drop the "enum"
// constraint from selected read-model string fields (see
// readModelEnumFields). ogen turns an enum field into a named type with a
// strict Validate() that the response decoder calls unconditionally, so a
// single unknown value the server adds later (a new repo state, say) fails
// the whole list/get request. These fields are display-only on the client —
// nothing branches on them — so we model them as open strings: unknown
// values decode and print verbatim instead of aborting the request. Only
// response read models are loosened; request-body enums stay strict so we
// still reject a bad value we are about to send.
//
// Run via `go generate ./internal/coreapi/...` (the first generate step in
// gen.go), or by hand after refreshing the spec:
//
//	curl -fsSL https://us.console.entire.io/api/v1/openapi.json \
//	    | jq . > internal/coreapi/spec/core.openapi.json
//	go run spec/normalize.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

const (
	srcPath = "spec/core.openapi.json"
	outPath = "spec/core.gen.json"
)

// errorModelRef is the component schema every problem+json error response
// in this spec already points at; the injected default reuses it.
const errorModelRef = "#/components/schemas/ErrorModel"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "normalize: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read spec: %w", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse spec: %w", err)
	}

	ops := foldErrorResponses(doc)
	loosened := loosenReadModelEnums(doc)
	reenroll := addCreateCIWebhookReenrollStatus(doc)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode spec: %w", err)
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil { //nolint:gosec // spec is not a secret
		return fmt.Errorf("write spec: %w", err)
	}

	fmt.Printf("normalize: folded error responses on %d operation(s), loosened %d read-model enum field(s), added %d re-enroll status → %s\n", ops, loosened, reenroll, outPath)
	return nil
}

// addCreateCIWebhookReenrollStatus teaches the generated client that
// POST /repos/{repoId}/ci-webhooks can answer 200 as well as 201.
//
// The handler is idempotent: it returns 201 on a fresh enroll and 200 on an
// idempotent re-enroll of an existing subscription (both carry the same
// CIWebhookView body). huma only documents its Operation.DefaultStatus (201),
// so the pristine spec omits the 200 — and ogen's decoder then treats a 200
// re-enroll as an unexpected status, surfacing the idempotent success as a
// content-type error rather than returning the subscription.
//
// The fix replaces the single literal 201 with a "2XX" range (same schema),
// so ogen decodes any 2xx to one concrete *CIWebhookViewStatusCode — no
// per-status sum type (declaring 200 and 201 separately would force one),
// and the wrapper's StatusCode still tells fresh (201) from re-enroll (200)
// apart. Returns 1 if applied, else 0.
//
// See UPSTREAM.md item 3: the real fix is huma advertising the 200 upstream,
// after which this transform can be deleted.
func addCreateCIWebhookReenrollStatus(doc map[string]any) int {
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return 0
	}
	item, ok := paths["/repos/{repoId}/ci-webhooks"].(map[string]any)
	if !ok {
		return 0
	}
	post, ok := item["post"].(map[string]any)
	if !ok {
		return 0
	}
	responses, ok := post["responses"].(map[string]any)
	if !ok {
		return 0
	}
	created, ok := responses["201"]
	if !ok {
		return 0
	}
	delete(responses, "201")
	responses["2XX"] = created
	return 1
}

// readModelEnumFields lists the response read-model schema fields whose
// "enum" constraint is dropped by loosenReadModelEnums, keyed by component
// schema name. These are display-only on the client: the CLI prints them
// but never branches on their value, so a value the server adds later must
// pass through rather than fail the whole request in ogen's Validate().
//
// Only response read models belong here. Request-body schemas (e.g.
// SetRepoVisibilityInputBody) keep their enums so we still reject a bad
// value before sending it.
var readModelEnumFields = map[string][]string{
	"Repo": {"objectFormat", "state", "visibility"},
}

// loosenReadModelEnums deletes the "enum" key from each field named in
// readModelEnumFields, leaving `type: string`. Without an enum, ogen emits a
// plain string with no Validate() method, so an unknown server value decodes
// and displays instead of aborting the request. Returns the number of fields
// loosened; a missing schema or field is skipped (a spec refresh that renames
// or removes one simply loosens nothing there).
func loosenReadModelEnums(doc map[string]any) int {
	components, ok := doc["components"].(map[string]any)
	if !ok {
		return 0
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		return 0
	}
	count := 0
	for schemaName, fields := range readModelEnumFields {
		schema, ok := schemas[schemaName].(map[string]any)
		if !ok {
			continue
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range fields {
			prop, ok := props[field].(map[string]any)
			if !ok {
				continue
			}
			if _, had := prop["enum"]; had {
				delete(prop, "enum")
				count++
			}
		}
	}
	return count
}

// httpMethods is the set of OpenAPI path-item keys that are operations.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// foldErrorResponses rewrites each operation's responses to its real 2xx
// success entries plus a single "default" error, dropping the explicit
// 4xx/5xx codes.
//
// The spec declares accurate success codes (201 for creates, 200 for
// reads, 204 for deletes), so those are kept verbatim — keeping the literal
// code (not a "2XX" range) means ogen returns the success type directly, with no
// `*…StatusCode` wrapper. Every explicit error status references the same
// ErrorModel, so folding them all into one "default" is lossless and flips
// ogen into "convenient errors": `(*T, error)` with any non-2xx as a typed
// `*ErrorModelStatusCode`. Returns the number of operations rewritten.
func foldErrorResponses(doc map[string]any) int {
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return 0
	}
	count := 0
	for _, item := range paths {
		pathItem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for method, op := range pathItem {
			if !httpMethods[method] {
				continue
			}
			operation, ok := op.(map[string]any)
			if !ok {
				continue
			}
			responses, ok := operation["responses"].(map[string]any)
			if !ok {
				continue
			}
			folded := map[string]any{"default": defaultErrorResponse()}
			for status, resp := range responses {
				if len(status) > 0 && status[0] == '2' {
					folded[status] = resp
				}
			}
			operation["responses"] = folded
			count++
		}
	}
	return count
}

func defaultErrorResponse() map[string]any {
	return map[string]any{
		"description": "Error",
		"content": map[string]any{
			"application/problem+json": map[string]any{
				"schema": map[string]any{"$ref": errorModelRef},
			},
		},
	}
}
