//! `recall` binary — two modes driven by the Go shim.
//!
//!   recall ingest   --brain <dir>            (IngestInput JSON on stdin)
//!   recall activate --brain <dir> --k <n> <question>   (ranked hits JSON on stdout)

use std::io::Read;
use std::path::PathBuf;
use std::process::ExitCode;

use fluctlightdb::brain::FluctlightBrain;
use recall::graph::GraphReach;
use recall::ingest::ingest;
use recall::model::{Checkpoint, Coverage, IngestInput, IngestReport};
use recall::rank::{Ranked, rerank};
use serde::Serialize;

const CHECKPOINTS_FILE: &str = "checkpoints.json";
/// Written beside the brain at ingest and returned with every answer, so an
/// answer drawn from partial context says so.
const COVERAGE_FILE: &str = "coverage.json";

/// The activate envelope: what the hits were drawn from, then the hits.
#[derive(Serialize)]
struct ActivateOutput {
    coverage: Coverage,
    hits: Vec<Ranked>,
}

fn usage() -> ExitCode {
    eprintln!(
        "usage:\n  recall ingest --brain <dir> [--no-graph]   < ingest.json\n  recall activate --brain <dir> [--k N] <question>"
    );
    ExitCode::from(2)
}

fn flag(args: &[String], name: &str) -> Option<String> {
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1).cloned())
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let Some(mode) = args.first() else {
        return usage();
    };
    let Some(brain_dir) = flag(&args, "--brain").map(PathBuf::from) else {
        return usage();
    };
    match mode.as_str() {
        "ingest" => run_ingest(&brain_dir, args.iter().any(|a| a == "--no-graph")),
        "activate" => {
            let k: usize = flag(&args, "--k").and_then(|s| s.parse().ok()).unwrap_or(8);
            let question = args
                .iter()
                .skip(1)
                .enumerate()
                .filter(|(i, a)| {
                    let prev = args.get(*i); // args[i] is the element before args[i+1]
                    !a.starts_with("--") && prev.is_none_or(|p| p != "--brain" && p != "--k")
                })
                .map(|(_, a)| a.as_str())
                .collect::<Vec<_>>()
                .join(" ");
            if question.trim().is_empty() {
                return usage();
            }
            run_activate(&brain_dir, &question, k)
        }
        _ => usage(),
    }
}

fn run_ingest(brain_dir: &PathBuf, no_graph: bool) -> ExitCode {
    let mut raw = String::new();
    if let Err(e) = std::io::stdin().read_to_string(&mut raw) {
        eprintln!("recall: read stdin: {e}");
        return ExitCode::FAILURE;
    }
    let input: IngestInput = match serde_json::from_str(&raw) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("recall: parse ingest input: {e}");
            return ExitCode::FAILURE;
        }
    };
    let graph = if no_graph || input.repo_root.is_empty() {
        GraphReach::unavailable()
    } else {
        GraphReach::from_entire_graph(&input.repo_root, &input.checkpoints)
    };
    // The brain is derived state: rebuild from scratch so removed checkpoints
    // (rebase, amend) do not linger as stale engrams.
    let _ = std::fs::remove_dir_all(brain_dir);
    if let Err(e) = std::fs::create_dir_all(brain_dir) {
        eprintln!("recall: create {}: {e}", brain_dir.display());
        return ExitCode::FAILURE;
    }
    let mut brain = match FluctlightBrain::open(brain_dir) {
        Ok(b) => b,
        Err(e) => {
            eprintln!("recall: open brain: {e}");
            return ExitCode::FAILURE;
        }
    };
    let engrams = ingest(&mut brain, &input.checkpoints, &graph);
    if let Err(e) = brain.checkpoint() {
        eprintln!("recall: persist brain: {e}");
        return ExitCode::FAILURE;
    }
    // Keep the checkpoints beside the brain so activate can judge agreement
    // without the Go side re-reading git.
    if let Err(e) = std::fs::write(
        brain_dir.join(CHECKPOINTS_FILE),
        serde_json::to_vec(&input).unwrap_or_default(),
    ) {
        eprintln!("recall: write {CHECKPOINTS_FILE}: {e}");
        return ExitCode::FAILURE;
    }
    let coverage = Coverage::from_input(&input, graph.is_available());
    if let Err(e) = std::fs::write(
        brain_dir.join(COVERAGE_FILE),
        serde_json::to_vec(&coverage).unwrap_or_default(),
    ) {
        eprintln!("recall: write {COVERAGE_FILE}: {e}");
        return ExitCode::FAILURE;
    }
    let report = IngestReport {
        checkpoints: input.checkpoints.len(),
        engrams,
        graph_edges: graph.edge_count(),
        brain: brain_dir.display().to_string(),
        coverage,
    };
    println!("{}", serde_json::to_string(&report).unwrap_or_default());
    ExitCode::SUCCESS
}

fn run_activate(brain_dir: &PathBuf, question: &str, k: usize) -> ExitCode {
    let input: IngestInput = match std::fs::read_to_string(brain_dir.join(CHECKPOINTS_FILE))
        .map_err(|e| e.to_string())
        .and_then(|s| serde_json::from_str(&s).map_err(|e| e.to_string()))
    {
        Ok(v) => v,
        Err(e) => {
            eprintln!(
                "recall: no ingested brain at {} ({e}); run `entire recall ingest` first",
                brain_dir.display()
            );
            return ExitCode::FAILURE;
        }
    };
    // No coverage record means a brain from before coverage was tracked; an
    // answer without one would be presented as complete by default.
    let coverage: Coverage = match std::fs::read_to_string(brain_dir.join(COVERAGE_FILE))
        .map_err(|e| e.to_string())
        .and_then(|s| serde_json::from_str(&s).map_err(|e| e.to_string()))
    {
        Ok(c) => c,
        Err(e) => {
            eprintln!(
                "recall: no coverage record at {} ({e}); run `entire recall ingest` again",
                brain_dir.display()
            );
            return ExitCode::FAILURE;
        }
    };
    let brain = match FluctlightBrain::open_readonly(brain_dir) {
        Ok(b) => b,
        Err(e) => {
            eprintln!("recall: open brain: {e}");
            return ExitCode::FAILURE;
        }
    };
    let cps: &[Checkpoint] = &input.checkpoints;
    // Reach edges were folded into file engrams at ingest; rebuild the map
    // from them so the scope check sees the same graph activate did not query.
    let graph = graph_from_brain(&brain, cps).with_availability(coverage.graph_available);
    let res = brain.activate_scoped(question, None, None, k.max(1) * 3);
    let hits: Vec<Ranked> = rerank(&res.recalls, cps, &graph)
        .into_iter()
        .take(k)
        .collect();
    let out = ActivateOutput { coverage, hits };
    println!("{}", serde_json::to_string(&out).unwrap_or_default());
    ExitCode::SUCCESS
}

fn graph_from_brain(brain: &FluctlightBrain, cps: &[Checkpoint]) -> GraphReach {
    let mut edges = std::collections::HashMap::new();
    for cp in cps {
        for f in &cp.files {
            let res = brain.activate_scoped(
                &format!("FILE {f} changed in {}", cp.commit_sha),
                None,
                None,
                4,
            );
            for r in res.recalls {
                let ctx = &r.episode.context;
                let Some(file) = ctx.split_whitespace().find_map(|t| t.strip_prefix("file:"))
                else {
                    continue;
                };
                let Some(reach) = ctx
                    .split_whitespace()
                    .find_map(|t| t.strip_prefix("reaches:"))
                else {
                    continue;
                };
                if file == f && !reach.is_empty() {
                    edges.entry(f.clone()).or_insert_with(|| {
                        reach
                            .split(',')
                            .filter(|s| !s.is_empty())
                            .map(str::to_string)
                            .collect()
                    });
                }
            }
        }
    }
    GraphReach::from_map(edges)
}
