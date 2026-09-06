//! Checkpoints → engrams.

use fluctlightdb::brain::FluctlightBrain;
use fluctlightdb::types::{Episode, Provenance, ProvenanceKind};

use crate::graph::GraphReach;
use crate::model::Checkpoint;

pub const SALIENCE_COMMIT: f32 = 0.90;
pub const SALIENCE_USER: f32 = 0.85;
pub const SALIENCE_ASSISTANT: f32 = 0.45;

fn provenance(
    kind: ProvenanceKind,
    checkpoint_id: &str,
    confidence: f32,
    verified: bool,
) -> Provenance {
    Provenance {
        kind,
        source_uri: Some(format!("entire://checkpoint/{checkpoint_id}")),
        confidence,
        verified,
    }
}

fn episode(
    content: String,
    context: String,
    salience: f32,
    agent: &str,
    prov: Provenance,
) -> Episode {
    Episode {
        content,
        context,
        outcome: None,
        salience_hint: salience,
        semantic_vector: None,
        agent_id: Some(agent.to_string()),
        tenant_id: None,
        rag: None,
        provenance: Some(prov),
    }
}

/// Write one commit engram, one engram per session turn, and one file-node
/// engram per touched file (with the graph reach folded into its context so
/// activation can spread file → dependants). Returns the engram count.
pub fn ingest(brain: &mut FluctlightBrain, cps: &[Checkpoint], g: &GraphReach) -> usize {
    let mut n = 0;
    for cp in cps {
        let mut commit_ep = episode(
            format!("COMMIT {}: {}", cp.commit_sha, cp.commit_message),
            format!(
                "checkpoint:{} commit:{} files:{}",
                cp.checkpoint_id,
                cp.commit_sha,
                cp.files.join(",")
            ),
            SALIENCE_COMMIT,
            &cp.agent,
            provenance(
                ProvenanceKind::LedgerVerified,
                &cp.checkpoint_id,
                0.95,
                true,
            ),
        );
        commit_ep.outcome = Some(format!("touched {} files", cp.files.len()));
        brain.experience(commit_ep).expect("commit engram");
        n += 1;

        for t in &cp.session {
            let is_user = t.role == "user";
            let ep = episode(
                t.text.clone(),
                format!(
                    "checkpoint:{} commit:{} role:{}",
                    cp.checkpoint_id, cp.commit_sha, t.role
                ),
                if is_user {
                    SALIENCE_USER
                } else {
                    SALIENCE_ASSISTANT
                },
                &cp.agent,
                provenance(
                    if is_user {
                        ProvenanceKind::UserExplicit
                    } else {
                        ProvenanceKind::ChatAssertion
                    },
                    &cp.checkpoint_id,
                    if is_user { 0.8 } else { 0.4 },
                    false,
                ),
            );
            brain.experience(ep).expect("session engram");
            n += 1;
        }

        for f in &cp.files {
            let reach = g.reach(f);
            let ep = episode(
                format!("FILE {f} changed in {}", cp.commit_sha),
                format!(
                    "checkpoint:{} commit:{} file:{f} reaches:{}",
                    cp.checkpoint_id,
                    cp.commit_sha,
                    reach.join(",")
                ),
                0.5,
                &cp.agent,
                provenance(
                    ProvenanceKind::FileObservation,
                    &cp.checkpoint_id,
                    0.9,
                    true,
                ),
            );
            brain.experience(ep).expect("file engram");
            n += 1;
        }
    }
    n
}
