//! The seven reference tests, ported verbatim against the library API, plus
//! the checks the buildathon brief adds (M1 identifier anchor, graph-impact
//! parsing, check ordering).

use fluctlightdb::brain::FluctlightBrain;
use fluctlightdb::confidence::{Evidence, SourceKind, recall_confidence};
use recall::agreement::{Agree, agreement};
use recall::graph::{GraphReach, parse_impact_json};
use recall::ingest::ingest;
use recall::model::Checkpoint;
use recall::text::polarity;
use std::collections::HashMap;

fn cps() -> Vec<Checkpoint> {
    serde_json::from_str(&std::fs::read_to_string("fixtures/checkpoints.json").unwrap()).unwrap()
}
fn cp(sha: &str) -> Checkpoint {
    cps().into_iter().find(|c| c.commit_sha == sha).unwrap()
}

/// The fixture's reach map, identical to the reference stub: retry.go and
/// client.go escape the dispatch package.
fn fixture_graph() -> GraphReach {
    GraphReach::from_map(HashMap::from([
        (
            "internal/dispatch/retry.go".to_string(),
            vec![
                "internal/hooks/runner.go".to_string(),
                "internal/telemetry/emit.go".to_string(),
            ],
        ),
        (
            "internal/dispatch/client.go".to_string(),
            vec![
                "internal/dispatch/generate.go".to_string(),
                "internal/hooks/runner.go".to_string(),
            ],
        ),
    ]))
}

#[test]
fn wrapper_does_not_read_as_the_verb_wrap() {
    // regression: substring matching cancelled polarity to zero
    assert_eq!(polarity("Removing the retry wrapper entirely"), -1);
    assert_eq!(polarity("Add a retry wrapper"), 1);
}

#[test]
fn isolation_claim_refuted_by_graph_reach() {
    let (v, _) = agreement(
        "This is contained within internal/dispatch and does not touch callers.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted);
}

#[test]
fn polarity_flip_against_conventional_commit() {
    let (v, _) = agreement(
        "Dropped the empty-repo guard from dispatch generation.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted);
}

#[test]
fn untouched_file_is_contradiction() {
    let (v, _) = agreement(
        "Edited internal/auth/token.go to fix the guard.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted);
}

#[test]
fn agreeing_claim_is_corroborated() {
    let (v, _) = agreement(
        "Guard dispatch generation against empty repositories.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Corroborated);
}

#[test]
fn contradiction_collapses_confidence_below_corroborated() {
    let ok = recall_confidence(&[
        Evidence::new(SourceKind::UserStated, 1.0),
        Evidence::new(SourceKind::Verified, 1.0),
    ]);
    let bad = recall_confidence(&[
        Evidence::new(SourceKind::UserStated, 1.0),
        Evidence::new(SourceKind::Unknown, 0.15),
    ]) * 0.45;
    assert!(
        bad < ok,
        "contradicted {bad} must rank under corroborated {ok}"
    );
}

#[test]
fn brain_survives_reopen() {
    let dir = std::env::temp_dir().join("recall-test-brain");
    let _ = std::fs::remove_dir_all(&dir);
    let mut b = FluctlightBrain::open(&dir).unwrap();
    ingest(&mut b, &cps(), &fixture_graph());
    b.checkpoint().unwrap();
    drop(b);
    let r = FluctlightBrain::open_readonly(&dir).unwrap();
    assert!(!r.activate("retry wrapper").recalls.is_empty());
}

// ── additions from the brief ─────────────────────────────────────────────

#[test]
fn identifier_absent_from_diff_is_contradiction() {
    // M1: a code-shaped token the diff never introduces refutes the claim.
    let (v, why) = agreement(
        "Added the guard in generate.go via checkEmptyRepo() and setMaxRetries.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted, "{why}");
}

#[test]
fn identifier_present_in_diff_is_not_refuted_by_m1() {
    // withRetry and maxAttempts both appear on + lines of 9f2c1ab.
    let (v, why) = agreement(
        "Implemented withRetry with maxAttempts of three.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_ne!(v, Agree::Contradicted, "{why}");
}

#[test]
fn paraphrase_is_corroborated_by_ngram_overlap() {
    // No 2-term lexical overlap with the subject, but the diff vocabulary
    // (backoff, attempt, retries) carries it over the 4-gram threshold.
    let (v, why) = agreement(
        "Introduced automatic re-attempts when the send fails, backing off each time.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Corroborated, "{why}");
}

#[test]
fn unrelated_claim_stays_neutral() {
    let (v, why) = agreement(
        "Ran the formatter across the package.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Neutral, "{why}");
}

#[test]
fn impact_json_yields_file_edges_excluding_self_and_externals() {
    let json = r#"{"focus":{"file_path":"internal/dispatch/retry.go"},
      "callers":{"entries":[
        {"endpoint":{"file_path":"internal/hooks/runner.go","external":false}},
        {"endpoint":{"file_path":"internal/dispatch/retry.go"}},
        {"endpoint":{"qualified_name":"fmt.Errorf","external":true}}]},
      "type_consumers":{"entries":[{"endpoint":{"file_path":"internal/telemetry/emit.go"}}]}}"#;
    let mut reached = parse_impact_json("internal/dispatch/retry.go", json);
    reached.sort();
    assert_eq!(
        reached,
        vec!["internal/hooks/runner.go", "internal/telemetry/emit.go"]
    );
}

#[test]
fn url_in_claim_is_not_a_file_path() {
    // Regression from the real transcript: the pasted brief carried a docs URL
    // and the file check read its path segments as untouched repo paths.
    let mut c = cp("5c81f33");
    c.files = vec!["cmd/entire/cli/recall_cmd.go".into()];
    let claim = "Participant guide: https://docs.google.com/document/d/REDACTED/edit and rules at \
                 https://build.bengalurutechweek.com/ — see www.example.org and entire.io for context.";
    let (v, why) = agreement(claim, &c, &fixture_graph());
    assert_ne!(v, Agree::Contradicted, "{why}");
}

#[test]
fn real_path_next_to_a_url_is_still_checked() {
    let (v, why) = agreement(
        "See https://example.com/docs — edited internal/auth/token.go to fix the guard.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted, "{why}");
    assert!(why.contains("internal/auth/token.go"), "{why}");
}

#[test]
fn symbol_lines_come_from_symbols_stream_for_touched_files_only() {
    use recall::graph::symbol_lines_from_ndjson;
    let ndjson = concat!(
        r#"{"record_type":"symbol","kind":"function","name":"a","file_path":"x/a.go","start_line":10,"language":"Go"}"#,
        "\n",
        r#"{"record_type":"symbol","kind":"method","name":"b","file_path":"x/a.go","start_line":30,"language":"Go"}"#,
        "\n",
        r#"{"record_type":"symbol","kind":"function","name":"c","file_path":"x/a.go","start_line":50,"language":"Go"}"#,
        "\n",
        r#"{"record_type":"symbol","kind":"heading","name":"Title","file_path":"docs/a.md","start_line":1,"language":"Markdown"}"#,
        "\n",
        r#"{"record_type":"symbol","kind":"function","name":"z","file_path":"y/other.go","start_line":5,"language":"Go"}"#,
        "\n",
        r#"{"record_type":"summary"}"#,
        "\n",
    );
    let touched = vec!["x/a.go".to_string(), "docs/a.md".to_string()];
    let got = symbol_lines_from_ndjson(&touched, ndjson, 2);
    assert_eq!(
        got,
        vec![
            ("x/a.go".to_string(), 10usize),
            ("x/a.go".to_string(), 30usize)
        ],
        "two code symbols per touched file, in line order; headings and untouched files are skipped"
    );
}

// ── privacy boundary: redacted or unavailable checkpoint fields ──────────

use recall::model::{Coverage, IngestInput, Skipped};
use recall::rank::{PARTIAL_CONFIDENCE_CAP, rerank};

/// A checkpoint whose diff was withheld: the shim declares it, the field is empty.
fn cp_without_diff(sha: &str) -> Checkpoint {
    let mut c = cp(sha);
    c.diff.clear();
    c.unavailable = vec!["diff".into()];
    c
}

fn recall_of(
    content: &str,
    kind: &str,
    sha: &str,
    activation: f32,
) -> fluctlightdb::types::RecallResult {
    serde_json::from_value(serde_json::json!({
        "engram_id": "00000000-0000-0000-0000-000000000001",
        "activation": activation,
        "completion_strength": 1.0,
        "episode": {
            "content": content,
            "context": format!("checkpoint:x commit:{sha} role:assistant"),
            "outcome": null,
            "salience_hint": 0.5,
            "semantic_vector": null,
            "agent_id": "t",
            "tenant_id": null,
            "rag": null,
            "provenance": {"kind": kind, "source_uri": null, "confidence": 0.5, "verified": false}
        }
    }))
    .expect("recall result literal")
}

#[test]
fn missing_diff_makes_identifier_check_unverifiable_not_contradicted() {
    // Same claim as identifier_absent_from_diff_is_contradiction, but the diff
    // is unavailable: absence of evidence is not evidence of a lie.
    let (v, why) = agreement(
        "Added the guard in generate.go via checkEmptyRepo() and setMaxRetries.",
        &cp_without_diff("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Unverifiable, "{why}");
    assert!(
        why.contains("diff"),
        "the reason names the missing field: {why}"
    );
}

#[test]
fn isolation_claim_with_graph_unavailable_is_unverifiable() {
    let (v, why) = agreement(
        "This is contained within internal/dispatch and does not touch callers.",
        &cp("9f2c1ab"),
        &GraphReach::unavailable(),
    );
    assert_eq!(v, Agree::Unverifiable, "{why}");
    assert!(why.contains("graph"), "{why}");
}

#[test]
fn empty_graph_is_not_unavailable_graph() {
    // Bench parity: GraphReach::default() is "no edges", not "no graph". The
    // isolation claim falls through to the later checks exactly as before.
    let (v, why) = agreement(
        "This is contained within internal/dispatch and does not touch callers.",
        &cp("9f2c1ab"),
        &GraphReach::default(),
    );
    assert_ne!(v, Agree::Unverifiable, "{why}");
}

#[test]
fn unavailable_files_make_a_path_claim_unverifiable() {
    let mut c = cp("5c81f33");
    c.files.clear();
    c.unavailable = vec!["files".into()];
    let (v, why) = agreement(
        "Edited internal/auth/token.go to fix the guard.",
        &c,
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Unverifiable, "{why}");
    assert!(why.contains("files"), "{why}");
}

#[test]
fn nothing_fires_on_a_partial_checkpoint_is_unverifiable_not_neutral() {
    // Neutral means "checked, found nothing". With the diff missing the
    // n-gram check never ran, so the honest answer is Unverifiable.
    let (v, why) = agreement(
        "Ran the formatter across the package.",
        &cp_without_diff("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Unverifiable, "{why}");
}

#[test]
fn unverifiable_serialises_as_its_own_word() {
    assert_eq!(
        serde_json::to_string(&Agree::Unverifiable).unwrap(),
        "\"unverifiable\""
    );
}

#[test]
fn partial_checkpoint_caps_confidence_and_names_the_missing_field() {
    let mut partial = cp_without_diff("5c81f33");
    partial.commit_sha = "part001".into();
    let cps = vec![cp("5c81f33"), partial];
    let claim = "Guard dispatch generation against empty repositories.";
    let ranked = rerank(
        &[
            recall_of(claim, "chat_assertion", "part001", 1.0),
            recall_of(&format!("{claim} Done."), "chat_assertion", "5c81f33", 1.0),
        ],
        &cps,
        &fixture_graph(),
    );
    let complete = ranked
        .iter()
        .find(|r| r.commit.as_deref() == Some("5c81f33"))
        .unwrap();
    let capped = ranked
        .iter()
        .find(|r| r.commit.as_deref() == Some("part001"))
        .unwrap();
    assert_eq!(complete.verdict, Agree::Corroborated);
    assert!(complete.partial.is_empty());
    assert!(complete.confidence > PARTIAL_CONFIDENCE_CAP);
    assert_eq!(capped.verdict, Agree::Corroborated, "{}", capped.why);
    assert_eq!(capped.partial, vec!["diff".to_string()]);
    assert!(
        capped.confidence <= PARTIAL_CONFIDENCE_CAP,
        "{}",
        capped.confidence
    );
    assert_eq!(
        ranked[0].commit.as_deref(),
        Some("5c81f33"),
        "the complete hit ranks first"
    );
}

#[test]
fn ledger_hit_on_partial_checkpoint_keeps_confidence_but_carries_marker() {
    let mut partial = cp("9f2c1ab");
    partial.session.clear();
    partial.unavailable = vec!["session".into()];
    let ranked = rerank(
        &[recall_of(
            "COMMIT 9f2c1ab: feat: add retry wrapper",
            "ledger_verified",
            "9f2c1ab",
            1.0,
        )],
        &[partial],
        &fixture_graph(),
    );
    assert_eq!(ranked[0].tier, "LEDGER");
    assert!(
        ranked[0].confidence > PARTIAL_CONFIDENCE_CAP,
        "the commit record is itself complete"
    );
    assert_eq!(ranked[0].partial, vec!["session".to_string()]);
}

#[test]
fn redacted_marker_in_claim_marks_partial_without_changing_verdict() {
    let claim = "Guard dispatch generation against empty repositories; the key was REDACTED.";
    let (v, _) = agreement(claim, &cp("5c81f33"), &fixture_graph());
    let ranked = rerank(
        &[recall_of(claim, "chat_assertion", "5c81f33", 1.0)],
        &cps(),
        &fixture_graph(),
    );
    assert_eq!(ranked[0].verdict, v, "the marker never routes the verdict");
    assert_eq!(ranked[0].partial, vec!["claim redacted".to_string()]);
    assert!(ranked[0].confidence <= PARTIAL_CONFIDENCE_CAP);
}

#[test]
fn units_commit_types_and_absolute_paths_are_not_repo_paths() {
    // The three false contradictions from the morning transcript.
    let (v, why) = agreement(
        "Preserve 139 µs/pair. Infer polarity from feat/fix/revert. Reference: /home/me/Desktop/cli/grader_main.rs",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_ne!(v, Agree::Contradicted, "{why}");
}

#[test]
fn coverage_counts_complete_partial_skipped_and_truncation() {
    let mut partial = cp("9f2c1ab");
    partial.unavailable = vec!["session".into()];
    let input = IngestInput {
        repo_root: String::new(),
        checkpoints: vec![cp("5c81f33"), cp("3ee7d90"), partial],
        skipped: vec![
            Skipped {
                checkpoint_id: "01A".into(),
                reason: "fetch failed".into(),
            },
            Skipped {
                checkpoint_id: "01B".into(),
                reason: "fetch failed".into(),
            },
        ],
        truncated: true,
    };
    let c = Coverage::from_input(&input, false);
    assert_eq!((c.complete, c.partial, c.skipped, c.total), (2, 1, 2, 5));
    assert!(!c.graph_available);
    assert!(c.truncated);
    assert!(!c.is_complete());
    let full = Coverage::from_input(
        &IngestInput {
            checkpoints: cps(),
            ..Default::default()
        },
        true,
    );
    assert!(full.is_complete());
}

#[test]
fn null_session_files_and_diff_deserialise_as_empty() {
    // The Go shim's nil slices arrive as JSON null; a ledger-only checkpoint
    // must still parse.
    let cp: Checkpoint = serde_json::from_str(
        r#"{"checkpoint_id":"x","commit_sha":"abc","commit_message":"feat: x","agent":"",
            "files":null,"session":null,"diff":null,"unavailable":["session"]}"#,
    )
    .expect("null sequences parse as empty");
    assert!(cp.files.is_empty() && cp.session.is_empty() && cp.diff.is_empty());
    assert!(cp.lacks("session"));
}
