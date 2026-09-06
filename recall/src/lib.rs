//! `recall` — associative, provenance-weighted memory over Entire checkpoints.
//!
//! The Go side (`entire recall`) reads checkpoint storage and pipes JSON in;
//! this crate owns every ranking decision. The algorithm is a port of the
//! validated reference (see `docs/recall/architecture.md`).

pub mod agreement;
pub mod graph;
pub mod ingest;
pub mod model;
pub mod rank;
pub mod text;
