//go:build ignore

// Command normalize rewrites the fetched Core API spec into a form the Go
// client generator (ogen) turns into an ergonomic client. It reads the
// pristine upstream spec (core.openapi.json) and writes a generated,
// committed artifact (core.gen.json) that ogen consumes; the upstream
// file is never mutated, so a refresh is a clean `curl` overwrite.
//
// This command applies one transform, documented below. It is a deliberate
// codegen-ergonomics fold, not a bug workaround — the upstream spec is
// otherwise accurate (it declares real success codes and non-nullable
// arrays). The running checklist of upstream fixes lives in
// internal/coreapi/UPSTREAM.md.
//
// Fold every operation's explicit error responses (4xx/5xx) into a single
// "default" error, leaving the real success response (201 for creates, 200
// for reads, 204 for deletes) untouched. The spec declares accurate success
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
// Run via `go generate ./internal/coreapi/...` (the first generate step in
// gen.go), or by hand after refreshing the spec:
//
//	curl -fsSL https://us.console.entire.io/api/v1/openapi.json \
//	    -o internal/coreapi/spec/core.openapi.json
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

	fmt.Printf("normalize: folded error responses on %d operation(s) → %s\n", ops, outPath)
	return nil
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
