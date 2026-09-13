//! CommitEval-style benchmark: run the ported `agreement()` over the 840
//! labelled (message, commit) pairs from real entireio/cli commits and report
//! precision / recall / specificity on the "inconsistent" class.
//!
//!   cargo run --release --example bench

use recall::agreement::{Agree, agreement};
use recall::graph::GraphReach;
use recall::model::Checkpoint;
use serde::Deserialize;

#[derive(Deserialize)]
struct Case {
    sha: String,
    msg: String,
    mutated: String,
    files: Vec<String>,
    diff: String,
    label: u8,
    mtype: String,
}

fn main() {
    let raw = std::fs::read_to_string("bench/bench_840.json").expect("bench/bench_840.json");
    let cases: Vec<Case> = serde_json::from_str(&raw).expect("parse bench");
    let g = GraphReach::default();
    let cps: Vec<Checkpoint> = cases
        .iter()
        .map(|c| Checkpoint {
            checkpoint_id: c.sha.clone(),
            commit_sha: c.sha.clone(),
            commit_message: c.msg.clone(),
            agent: "bench".into(),
            files: c.files.clone(),
            session: Vec::new(),
            diff: c.diff.lines().map(str::to_string).collect(),
            unavailable: Vec::new(),
        })
        .collect();

    let t0 = std::time::Instant::now();
    let preds: Vec<bool> = cases
        .iter()
        .zip(&cps)
        .map(|(c, cp)| agreement(&c.mutated, cp, &g).0 == Agree::Contradicted)
        .collect();
    let per_pair = t0.elapsed().as_micros() as f64 / cases.len() as f64;

    let (mut tp, mut fp, mut tn, mut fnn) = (0f32, 0f32, 0f32, 0f32);
    for (p, c) in preds.iter().zip(&cases) {
        match (*p, c.label == 1) {
            (true, true) => tp += 1.0,
            (true, false) => fp += 1.0,
            (false, false) => tn += 1.0,
            (false, true) => fnn += 1.0,
        }
    }
    let prec = if tp + fp > 0.0 { tp / (tp + fp) } else { 0.0 };
    let rec = if tp + fnn > 0.0 { tp / (tp + fnn) } else { 0.0 };
    let spec = if tn + fp > 0.0 { tn / (tn + fp) } else { 0.0 };
    println!(
        "agreement() over {} pairs: P {prec:.3}  R {rec:.3}  Spec {spec:.3}  {per_pair:.0} µs/pair",
        cases.len()
    );

    let mut kinds: Vec<&str> = cases
        .iter()
        .filter(|c| c.label == 1)
        .map(|c| c.mtype.as_str())
        .collect();
    kinds.sort();
    kinds.dedup();
    for k in kinds {
        let idx: Vec<usize> = cases
            .iter()
            .enumerate()
            .filter(|(_, c)| c.mtype == k)
            .map(|(i, _)| i)
            .collect();
        let hit = idx.iter().filter(|i| preds[**i]).count();
        println!(
            "  {k:<16} {hit:>3}/{:<3} {:.2}",
            idx.len(),
            hit as f32 / idx.len() as f32
        );
    }
}
