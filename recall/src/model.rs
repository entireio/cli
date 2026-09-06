//! Wire types shared with the Go shim.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct Turn {
    pub role: String,
    pub text: String,
}

/// Fields a checkpoint may declare unavailable. The shim declares them; the
/// ranking side never infers unavailability from an empty field, because an
/// empty diff on an unredacted checkpoint is evidence and must stay so.
pub const FIELD_SESSION: &str = "session";
pub const FIELD_DIFF: &str = "diff";
pub const FIELD_FILES: &str = "files";
pub const FIELD_COMMIT_MESSAGE: &str = "commit_message";

/// The fields `agreement()` reads as evidence. `session` is where claims come
/// from, not what they are judged against, so it is deliberately absent.
const EVIDENCE_FIELDS: &[&str] = &[FIELD_COMMIT_MESSAGE, FIELD_FILES, FIELD_DIFF];

/// Accept JSON `null` for a sequence: the Go shim's nil slices arrive that
/// way on a ledger-only checkpoint.
fn null_as_empty<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

#[derive(Debug, Deserialize, Serialize, Clone, Default)]
pub struct Checkpoint {
    pub checkpoint_id: String,
    pub commit_sha: String,
    pub commit_message: String,
    pub agent: String,
    #[serde(default, deserialize_with = "null_as_empty")]
    pub files: Vec<String>,
    #[serde(default, deserialize_with = "null_as_empty")]
    pub session: Vec<Turn>,
    /// Unified diff lines (`+`/`-` prefixed; hunk headers tolerated).
    #[serde(default, deserialize_with = "null_as_empty")]
    pub diff: Vec<String>,
    /// Fields the shim could not provide (redacted, withheld, or unreadable).
    /// Empty on a complete checkpoint, which is the only path the benchmark
    /// exercises.
    #[serde(default, deserialize_with = "null_as_empty")]
    pub unavailable: Vec<String>,
}

impl Checkpoint {
    pub fn lacks(&self, field: &str) -> bool {
        self.unavailable.iter().any(|f| f == field)
    }

    /// Any field at all is missing.
    pub fn is_partial(&self) -> bool {
        !self.unavailable.is_empty()
    }

    /// A field the agreement checks judge against is missing.
    pub fn lacks_evidence(&self) -> bool {
        EVIDENCE_FIELDS.iter().any(|f| self.lacks(f))
    }

    /// The missing evidence fields, for the reason string.
    pub fn missing_evidence(&self) -> Vec<&str> {
        EVIDENCE_FIELDS
            .iter()
            .copied()
            .filter(|f| self.lacks(f))
            .collect()
    }
}

/// A trailered commit whose checkpoint could not be turned into anything.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct Skipped {
    pub checkpoint_id: String,
    pub reason: String,
}

/// Everything `recall ingest` needs on stdin.
#[derive(Debug, Deserialize, Serialize, Clone, Default)]
pub struct IngestInput {
    /// Repository root, used to run `entire graph impact`. Empty disables the graph.
    #[serde(default)]
    pub repo_root: String,
    pub checkpoints: Vec<Checkpoint>,
    /// Checkpoints the shim saw but could not emit even as a ledger record.
    #[serde(default)]
    pub skipped: Vec<Skipped>,
    /// The shim's commit walk hit its budget: older checkpoints were never examined.
    #[serde(default)]
    pub truncated: bool,
}

/// How much of the checkpoint history the brain actually holds. Written at
/// ingest, returned with every activate, so no answer is presented without
/// saying what it was drawn from.
#[derive(Debug, Deserialize, Serialize, Clone, Default)]
pub struct Coverage {
    /// Checkpoints seen: indexed plus skipped.
    pub total: usize,
    pub complete: usize,
    pub partial: usize,
    pub skipped: usize,
    pub truncated: bool,
    pub graph_available: bool,
    /// Field name → number of checkpoints lacking it.
    pub unavailable: BTreeMap<String, usize>,
}

impl Coverage {
    pub fn from_input(input: &IngestInput, graph_available: bool) -> Self {
        let mut unavailable: BTreeMap<String, usize> = BTreeMap::new();
        let mut partial = 0;
        for cp in &input.checkpoints {
            if cp.is_partial() {
                partial += 1;
            }
            for f in &cp.unavailable {
                *unavailable.entry(f.clone()).or_insert(0) += 1;
            }
        }
        Self {
            total: input.checkpoints.len() + input.skipped.len(),
            complete: input.checkpoints.len() - partial,
            partial,
            skipped: input.skipped.len(),
            truncated: input.truncated,
            graph_available,
            unavailable,
        }
    }

    /// Every checkpoint seen was indexed whole, the walk was not cut short,
    /// and the graph was consulted. Anything less is partial context.
    pub fn is_complete(&self) -> bool {
        self.partial == 0 && self.skipped == 0 && !self.truncated && self.graph_available
    }
}

#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct IngestReport {
    pub checkpoints: usize,
    pub engrams: usize,
    pub graph_edges: usize,
    pub brain: String,
    pub coverage: Coverage,
}
