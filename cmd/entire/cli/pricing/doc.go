// Package pricing provides a small, embedded table of per-model token prices
// and a helper to estimate the USD cost of a checkpoint's token usage.
//
// The rate table is maintained as JSON files under models/ and compiled into
// the binary via go:embed. To change or add a price, edit the appropriate JSON
// file (grouped by provider) and open a pull request. Operators can layer
// per-repository overrides through settings, which LoadTable applies on top of
// the embedded defaults (replacing an entry whose id matches, or appending a
// new one).
//
// An opt-in remote layer (remote.go, gated by the pricing.remote setting) can
// additionally refresh a cached rate table from a remote source roughly once a
// day (RefreshRemoteCache), layered on top of the embedded defaults the same
// way settings overrides are. There is no inline fetch on the hook/condensation
// path: the refresh is spawned as a detached background worker
// (maybeSpawnPricingRefresh) and never blocks a foreground command — LoadTable
// only ever reads the on-disk cache the worker last wrote.
//
// Lookup performs exact-id and alias matching only, with no implicit
// model-family fallback: an unknown model resolves to no rate, and callers must
// treat that as "no estimate available" rather than guessing a price. Aliases
// cover the id spellings seen across providers and tools: bare ids, Anthropic
// dated ids (claude-haiku-4-5-20251001), slash-prefixed ids
// (anthropic/claude-sonnet-5), Bedrock ids and regional inference profiles
// (anthropic.claude-opus-4-8-v1:0, us.anthropic.claude-opus-4-8-v1:0), and
// Vertex ids (claude-opus-4-5@20251101).
//
// A few billing nuances are deliberately approximated:
//
//   - Long context: a 1M-context request on a current-generation model bills at
//     the model's standard rate — there is no long-context premium — so the
//     "[1m]" ids Claude Code emits (e.g. claude-fable-5[1m]) share the base rate.
//     Because path.Match would read a literal "[1m]" alias as a character class,
//     Lookup strips a trailing "[...]" suffix from the query rather than relying
//     on such an alias.
//
//   - Cache writes: the derived cache-write (1.25x input) and cache-read (0.1x
//     input) multipliers are Anthropic-only economics and are applied only when a
//     rate's provider is "anthropic" and it omits an explicit cache rate. For any
//     other provider a missing cache rate falls back to the full input rate (1.0x),
//     billing cached tokens as normal input so an estimate never silently
//     undercharges; the non-Anthropic embedded tables set explicit cache rates,
//     so that fallback is only a safety net. Cache writes are priced per TTL: the
//     1.25x multiplier applies to the 5-minute-TTL default, while 1-hour-TTL writes
//     bill at 2x input (AnthropicCacheWrite1hMultiplier). Claude Code transcripts
//     split the two via cache_creation.ephemeral_1h_input_tokens /
//     ephemeral_5m_input_tokens; the 1h portion is parsed into
//     TokenUsage.CacheCreation1hTokens and priced at 2x. The two TTLs are
//     resolved independently: an Anthropic entry's explicit 5-minute rate
//     (CacheWritePerMTok) does NOT also cover the 1-hour portion, because the two
//     are independent multiples of the input rate rather than one derived from
//     the other — a rate source that mirrors upstream list prices without
//     distinguishing the two TTLs (e.g. a remote catalog) must not silently
//     disable the 1-hour premium. A source that does know a distinct 1-hour rate
//     sets CacheWrite1hPerMTok, which wins outright.
//
//   - Fast mode: turns billed at the fast-mode premium (usage.speed == "fast")
//     are priced here at standard rates — another known under-estimate — because
//     that premium is not published in-table.
package pricing
