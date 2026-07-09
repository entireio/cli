// Package pricing provides a small, embedded table of per-model token prices
// and a helper to estimate the USD cost of a checkpoint's token usage.
//
// The rate table is maintained as JSON files under models/ and compiled into
// the binary via go:embed. To change or add a price, edit the appropriate JSON
// file (grouped by provider) and open a pull request; there is no runtime
// fetch. Operators can layer per-repository overrides through settings, which
// LoadTable applies on top of the embedded defaults (replacing an entry whose
// id matches, or appending a new one).
//
// Lookup performs exact-id and alias matching only, with no implicit
// model-family fallback: an unknown model resolves to no rate, and callers must
// treat that as "no estimate available" rather than guessing a price.
package pricing
