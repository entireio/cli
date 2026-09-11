//go:build ignore

// Command normalize rewrites the fetched Core API spec into a form the Go
// client generator (ogen) turns into an ergonomic client. It reads the
// pristine upstream spec (core.openapi.json) and writes a generated,
// committed artifact (core.gen.json) that ogen consumes; the upstream
// file is never mutated, so a refresh is a clean `curl` overwrite.
//
// This command applies four transforms, documented below. The running
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
// the whole list/get request. The client treats these as open sets rather
// than closed ones — it prints them, or tests for the values it knows and
// treats everything else as unknown — so we model them as open strings:
// unknown values decode and pass through instead of aborting the request.
// Only response read models are loosened; request-body enums stay strict so
// we still reject a bad value we are about to send.
//
// Transform 2b (forward- and backward-compat, non-load-bearing read-model
// fields): drop selected fields from a read model's "required" list (see
// readModelOptionalFields). The client does not depend on their presence —
// it either ignores them or reads them through an Opt accessor with a safe
// default — so a server that does not send them yet must not fail the whole
// request.
//
// Transform 3 (unsupported security schemes): drop the interactive login
// schemes (oauth2, oidc) the spec lists on every operation. The CLI never
// drives them through the generated client, and ogen has no generator for
// openIdConnect, so leaving them in stops generation outright. Emptying an
// operation's security list is refused rather than performed — see
// dropInteractiveSecurity.
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
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
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
	optional := loosenReadModelRequired(doc)
	schemes, err := dropInteractiveSecurity(doc)
	if err != nil {
		return err
	}

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

	fmt.Printf("normalize: folded error responses on %d operation(s), loosened %d read-model enum field(s), made %d read-model field(s) optional, dropped %d interactive security scheme(s) → %s\n", ops, loosened, optional, schemes, outPath)
	return nil
}

// interactiveSecuritySchemes are the browser and device login schemes the
// spec advertises on every operation. The CLI drives none of them through
// the generated client: it mints a bearer itself and hands it over as
// bearerAuth. ogen also has no generator for openIdConnect and stops on it.
var interactiveSecuritySchemes = map[string]bool{"oauth2": true, "oidc": true}

// dropInteractiveSecurity removes interactiveSecuritySchemes from
// components.securitySchemes, from the document-level security list, and
// from every operation's security list, so the generated SecuritySource
// keeps exactly the bearerAuth and sessionAuth methods the client
// implements. It returns the number of schemes removed from components.
//
// It fails rather than empty a security list that had entries: in OpenAPI an
// empty operation-level "security" means the operation needs NO
// authentication, so filtering an oauth2-only operation down to nothing
// would quietly generate a client that stops sending the bearer to it. No
// operation in today's spec has that shape — every one also offers
// bearerAuth — but a device-code or authorize endpoint added upstream would,
// and it is the one failure here that is otherwise silent. (Its sibling
// announces itself: a multi-key requirement mixing oauth2 with bearerAuth
// survives whole, leaving a reference to a scheme this function deleted, and
// ogen aborts at generate time.)
func dropInteractiveSecurity(doc map[string]any) (int, error) {
	count := 0
	if components, ok := doc["components"].(map[string]any); ok {
		if schemes, ok := components["securitySchemes"].(map[string]any); ok {
			for name := range interactiveSecuritySchemes {
				if _, present := schemes[name]; present {
					delete(schemes, name)
					count++
				}
			}
		}
	}
	if sec, ok := doc["security"].([]any); ok {
		kept, err := filterSecurityRequirements(sec)
		if err != nil {
			return count, fmt.Errorf("document-level security: %w", err)
		}
		doc["security"] = kept
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return count, nil
	}
	for path, item := range paths {
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
			sec, ok := operation["security"].([]any)
			if !ok {
				continue
			}
			kept, err := filterSecurityRequirements(sec)
			if err != nil {
				return count, fmt.Errorf("%s %s: %w", strings.ToUpper(method), path, err)
			}
			operation["security"] = kept
		}
	}
	return count, nil
}

// filterSecurityRequirements drops the alternatives that name only
// interactive schemes. A requirement in this spec is a one-key object, so
// dropping the object drops the alternative. It errors when that would empty
// a non-empty list — see dropInteractiveSecurity for why an empty list is
// not the same as no list.
func filterSecurityRequirements(reqs []any) ([]any, error) {
	kept := make([]any, 0, len(reqs))
	for _, r := range reqs {
		req, ok := r.(map[string]any)
		if !ok {
			kept = append(kept, r)
			continue
		}
		interactive := len(req) > 0
		for name := range req {
			if !interactiveSecuritySchemes[name] {
				interactive = false
			}
		}
		if !interactive {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 && len(reqs) > 0 {
		return nil, errors.New("every security alternative names only interactive schemes; " +
			"an empty \"security\" list would declare the operation unauthenticated. " +
			"Give it a bearerAuth alternative upstream, or teach normalize how this operation " +
			"should authenticate")
	}
	return kept, nil
}

// readModelEnumFields lists the response read-model schema fields whose
// "enum" constraint is dropped by loosenReadModelEnums, keyed by component
// schema name. The client treats these as open sets: it prints them, or
// tests for the specific values it knows and treats everything else as
// unknown, so a value the server adds later must pass through rather than
// fail the whole request in ogen's Validate(). Repo.provider is the
// branching case — `repo protection list` tests for "github" and "entire"
// separately and has a third rendering for anything else, because each of
// its two known values licenses a different claim and neither is the else
// of the other. A field whose unknown values would silently take a known
// value's branch does not belong here.
//
// Only response read models belong here. Request-body schemas (e.g.
// SetRepoVisibilityInputBody) keep their enums so we still reject a bad
// value before sending it.
var readModelEnumFields = map[string][]string{
	"Repo":             {"objectFormat", "provider", "state", "visibility"},
	"RepoIDResolution": {"provider"},
	"RepoIndexEntry":   {"permission", "provider"},
	"RepoReference":    {"provider"},
	"RepoResolution":   {"provider"},
}

// readModelOptionalFields lists response read-model fields the spec marks
// required but the client does not depend on, keyed by component schema name.
// loosenReadModelRequired drops them from "required" so a server that
// predates the field, or a test fake that omits it, still decodes.
//
// "Does not depend on" is the bar, not "never reads": a field may be read
// through its Opt accessor with a safe default, where absence degrades one
// branch rather than failing the response — Repo.provider is read exactly
// that way by `repo protection list`, which falls back to the generic empty
// message when the server omits it. A field must leave this list when the
// CLI needs it to be *present* to be correct.
var readModelOptionalFields = map[string][]string{
	"ListReposOutputBody": {"candidatesIncomplete"},
	"Org":                 {"capabilities"},
	"Project":             {"capabilities"},
	"Repo":                {"capabilities", "provider"},
	"RepoIndexEntry":      {"org", "provider"},
}

// loosenReadModelRequired removes each field named in readModelOptionalFields
// from its schema's "required" list. Returns the number of fields removed.
func loosenReadModelRequired(doc map[string]any) int {
	components, ok := doc["components"].(map[string]any)
	if !ok {
		return 0
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		return 0
	}
	count := 0
	for schemaName, fields := range readModelOptionalFields {
		schema, ok := schemas[schemaName].(map[string]any)
		if !ok {
			continue
		}
		required, ok := schema["required"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(required))
		for _, r := range required {
			name, _ := r.(string)
			if slices.Contains(fields, name) {
				count++
				continue
			}
			kept = append(kept, r)
		}
		schema["required"] = kept
	}
	return count
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
