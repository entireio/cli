// Package recap provides the data model, loaders, aggregators, and
// renderers behind the `entire recap` command. It is a read-only
// projection over existing session state and committed checkpoint
// metadata, with optional api enrichment for work-mode labels and
// tool-time. Recap types are never persisted.
//
// See docs/superpowers/plans/2026-04-21-entire-recap.md for the design doc.
package recap
