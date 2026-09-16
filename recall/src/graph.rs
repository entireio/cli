//! File-level reach from `entire graph impact`.
//!
//! For each touched file we ask the graph what depends on the symbol at the
//! file's first changed line (callers, type consumers, data flows) and keep
//! the files those live in. That is the "blast radius" an isolation claim is
//! checked against. The graph is optional: when it is unavailable the reach
//! is empty and the scope check simply cannot fire.

use std::collections::{HashMap, HashSet};
use std::process::Command;

use serde_json::Value;

use crate::model::Checkpoint;

#[derive(Debug, Clone)]
pub struct GraphReach {
    edges: HashMap<String, Vec<String>>,
    /// False when the graph was never consulted (skipped or failed). An
    /// available graph with no edges is a finding; an unavailable one is not.
    available: bool,
}

impl Default for GraphReach {
    fn default() -> Self {
        Self {
            edges: HashMap::new(),
            available: true,
        }
    }
}

impl GraphReach {
    pub fn from_map(edges: HashMap<String, Vec<String>>) -> Self {
        Self {
            edges,
            available: true,
        }
    }

    /// The graph was not consulted: the scope check cannot run and says so.
    pub fn unavailable() -> Self {
        Self {
            edges: HashMap::new(),
            available: false,
        }
    }

    pub fn is_available(&self) -> bool {
        self.available
    }

    pub fn with_availability(mut self, available: bool) -> Self {
        self.available = available;
        self
    }

    pub fn reach(&self, file: &str) -> &[String] {
        self.edges.get(file).map(Vec::as_slice).unwrap_or(&[])
    }

    pub fn edge_count(&self) -> usize {
        self.edges.values().map(Vec::len).sum()
    }

    pub fn is_empty(&self) -> bool {
        self.edges.is_empty()
    }

    /// Files reached from `touched` that are not themselves touched.
    pub fn escapes(&self, touched: &[String]) -> Vec<String> {
        let set: HashSet<&str> = touched.iter().map(String::as_str).collect();
        let mut out: Vec<String> = touched
            .iter()
            .flat_map(|f| self.reach(f).iter())
            .filter(|d| !set.contains(d.as_str()))
            .cloned()
            .collect();
        out.sort();
        out.dedup();
        out
    }

    /// Resolve reach for every touched file across all checkpoints.
    ///
    /// The first `+` line of a new file is a package or import line, which
    /// resolves to no symbol (verified: `impact --symbol file:1` returns
    /// `focus: None`). So instead we ask the graph which symbols each touched
    /// file *defines* (one bulk `symbols` stream) and run `impact` on the first
    /// few of them, taking the union of the files their dependants live in.
    /// Failures are logged to stderr and skipped; nothing is invented.
    pub fn from_entire_graph(repo_root: &str, cps: &[Checkpoint]) -> Self {
        let touched: Vec<String> = {
            let mut v: Vec<String> = cps.iter().flat_map(|c| c.files.iter().cloned()).collect();
            v.sort();
            v.dedup();
            v
        };
        let ndjson = match symbols_stream(repo_root) {
            Ok(s) => s,
            Err(e) => {
                eprintln!("recall: graph symbols: {e}");
                return Self::unavailable();
            }
        };
        let targets = symbol_lines_from_ndjson(&touched, &ndjson, SYMBOLS_PER_FILE);
        let mut edges: HashMap<String, Vec<String>> = HashMap::new();
        for (f, line) in targets.iter().take(MAX_IMPACT_CALLS) {
            match impact(repo_root, f, *line) {
                Ok(json) => {
                    let reached = parse_impact_json(f, &json);
                    if !reached.is_empty() {
                        let e = edges.entry(f.clone()).or_default();
                        e.extend(reached);
                        e.sort();
                        e.dedup();
                    }
                }
                Err(e) => eprintln!("recall: graph impact {f}:{line}: {e}"),
            }
        }
        if targets.len() > MAX_IMPACT_CALLS {
            eprintln!(
                "recall: graph reach capped at {MAX_IMPACT_CALLS} impact queries ({} candidate symbols)",
                targets.len()
            );
        }
        Self {
            edges,
            available: true,
        }
    }
}

/// Code symbols queried per touched file, in definition order.
pub const SYMBOLS_PER_FILE: usize = 2;
/// Upper bound on `impact` subprocesses per ingest (~1.5 s each on a warm cache).
pub const MAX_IMPACT_CALLS: usize = 40;

const CODE_KINDS: &[&str] = &[
    "function",
    "method",
    "type",
    "struct",
    "class",
    "interface",
    "enum",
    "trait",
];

fn impact(repo_root: &str, file: &str, line: usize) -> Result<String, String> {
    let out = Command::new("entire")
        .args([
            "graph",
            "impact",
            "--repo",
            repo_root,
            "--head",
            "--format",
            "json",
            "--depth",
            "1",
            "--exclude-tests",
            "--limit",
            "30",
            "--symbol",
        ])
        .arg(format!("{file}:{line}"))
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err(String::from_utf8_lossy(&out.stderr).trim().to_string());
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

fn symbols_stream(repo_root: &str) -> Result<String, String> {
    let out = Command::new("entire")
        .args([
            "graph", "symbols", "--repo", repo_root, "--format", "ndjson",
        ])
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err(String::from_utf8_lossy(&out.stderr).trim().to_string());
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

/// From the bulk symbols stream, pick up to `per_file` code symbols (by start
/// line) for each touched file. Headings, config keys and untouched files are
/// skipped. Output is ordered by touched-file order, then line.
pub fn symbol_lines_from_ndjson(
    touched: &[String],
    ndjson: &str,
    per_file: usize,
) -> Vec<(String, usize)> {
    let want: HashSet<&str> = touched.iter().map(String::as_str).collect();
    let mut by_file: HashMap<String, Vec<usize>> = HashMap::new();
    for line in ndjson.lines() {
        let Ok(v) = serde_json::from_str::<Value>(line) else {
            continue;
        };
        if v.get("record_type").and_then(Value::as_str) != Some("symbol") {
            continue;
        }
        let Some(file) = v.get("file_path").and_then(Value::as_str) else {
            continue;
        };
        if !want.contains(file) {
            continue;
        }
        let kind = v.get("kind").and_then(Value::as_str).unwrap_or("");
        if !CODE_KINDS.contains(&kind) {
            continue;
        }
        if let Some(start) = v.get("start_line").and_then(Value::as_u64) {
            by_file
                .entry(file.to_string())
                .or_default()
                .push(start as usize);
        }
    }
    let mut out = Vec::new();
    for f in touched {
        if let Some(lines) = by_file.get_mut(f) {
            lines.sort_unstable();
            lines.dedup();
            out.extend(lines.iter().take(per_file).map(|l| (f.clone(), *l)));
        }
    }
    out
}

/// Extract the set of internal files an impact result reaches, excluding the
/// focus file itself and external (stdlib / dependency) endpoints.
pub fn parse_impact_json(file: &str, json: &str) -> Vec<String> {
    let Ok(v) = serde_json::from_str::<Value>(json) else {
        return Vec::new();
    };
    let mut files: HashSet<String> = HashSet::new();
    for section in ["callers", "type_consumers", "data_flows"] {
        let Some(entries) = v
            .get(section)
            .and_then(|s| s.get("entries"))
            .and_then(Value::as_array)
        else {
            continue;
        };
        for e in entries {
            let Some(ep) = e.get("endpoint") else {
                continue;
            };
            if ep.get("external").and_then(Value::as_bool).unwrap_or(false) {
                continue;
            }
            if let Some(p) = ep
                .get("file_path")
                .and_then(Value::as_str)
                .filter(|p| *p != file)
            {
                files.insert(p.to_string());
            }
        }
    }
    files.into_iter().collect()
}
