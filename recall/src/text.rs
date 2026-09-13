//! Tokenisation helpers. Everything here is pure and shared by the agreement
//! checks; the thresholds live with the checks that use them.

use std::collections::{HashMap, HashSet};

use crate::model::Checkpoint;

pub const STOP: &[&str] = &[
    "the", "a", "an", "and", "or", "to", "of", "in", "is", "it", "this", "that", "we", "you",
    "for", "on", "with", "not", "be", "are", "was", "so", "at", "as", "its", "into", "from", "by",
    "must", "now", "their", "own",
];

/// Content terms: lowercase, > 3 chars, stop words removed. Keeps `/`, `.`
/// and `_` inside a token so paths and identifiers survive as one term.
pub fn terms(s: &str) -> HashSet<String> {
    s.to_lowercase()
        .split(|c: char| !c.is_alphanumeric() && c != '/' && c != '.' && c != '_')
        .filter(|w| w.len() > 3 && !STOP.contains(w))
        .map(|w| w.to_string())
        .collect()
}

/// Action polarity — a claim that ADDS while the commit REMOVES is refuted
/// regardless of how many words they share.
///
/// Word-boundary tokens only: substring matching made "wrapper" read as the
/// verb "wrap", cancelling polarity to zero and silently disabling this check.
pub fn polarity(text: &str) -> i8 {
    let t = text.to_lowercase();
    let toks: HashSet<&str> = t.split(|c: char| !c.is_alphanumeric()).collect();
    const ADD: &[&str] = &[
        "add",
        "adding",
        "added",
        "introduce",
        "introduced",
        "create",
        "created",
        "enable",
        "implement",
        "implemented",
    ];
    const REM: &[&str] = &[
        "remove", "removing", "removed", "delete", "deleted", "drop", "dropped", "revert",
        "reverted", "disable", "strip", "stopped",
    ];
    let a = ADD.iter().filter(|w| toks.contains(*w)).count() as i8;
    let r = REM.iter().filter(|w| toks.contains(*w)).count() as i8;
    (a - r).signum()
}

/// Split camelCase / snake_case / dotted / slashed identifiers into lowercase
/// pieces, so `maxAttempts` yields {max, attempts}.
pub fn split_ident(w: &str) -> Vec<String> {
    let mut out = Vec::new();
    for part in w.split(['_', '.', '/']) {
        let mut cur = String::new();
        for ch in part.chars() {
            if ch.is_uppercase() && !cur.is_empty() {
                out.push(cur.to_lowercase());
                cur.clear();
            }
            if ch.is_alphanumeric() {
                cur.push(ch);
            }
        }
        if !cur.is_empty() {
            out.push(cur.to_lowercase());
        }
    }
    out.into_iter().filter(|w| w.len() > 2).collect()
}

/// Vocabulary the change actually introduces: identifier pieces from every
/// diff line plus the touched file paths.
pub fn diff_vocab(cp: &Checkpoint) -> HashSet<String> {
    let mut v = HashSet::new();
    for line in &cp.diff {
        for tok in line.split(|c: char| !c.is_alphanumeric() && c != '_') {
            for piece in split_ident(tok) {
                if piece.len() > 3 && !STOP.contains(&piece.as_str()) {
                    v.insert(piece);
                }
            }
        }
    }
    for f in &cp.files {
        for piece in split_ident(f) {
            if piece.len() > 3 {
                v.insert(piece);
            }
        }
    }
    v
}

/// Words that LOOK like code: camelCase, snake_case, dotted, `()`-suffixed.
pub fn code_shaped(w: &str) -> bool {
    let has_upper_inside = w.chars().skip(1).any(|c| c.is_uppercase());
    w.contains('_')
        || w.contains("()")
        || (w.contains('.') && !w.ends_with('.'))
        || has_upper_inside
}

/// Character 4-gram bag — model-free vector similarity, no embedder.
pub fn ngrams(s: &str) -> HashMap<String, f32> {
    let t: String = s
        .to_lowercase()
        .chars()
        .filter(|c| c.is_alphanumeric() || *c == ' ')
        .collect();
    let ch: Vec<char> = t.chars().collect();
    let mut m: HashMap<String, f32> = HashMap::new();
    for w in ch.windows(4) {
        *m.entry(w.iter().collect()).or_insert(0.0) += 1.0;
    }
    m
}

pub fn cosine(a: &HashMap<String, f32>, b: &HashMap<String, f32>) -> f32 {
    let dot: f32 = a.iter().filter_map(|(k, v)| b.get(k).map(|w| v * w)).sum();
    let na: f32 = a.values().map(|v| v * v).sum::<f32>().sqrt();
    let nb: f32 = b.values().map(|v| v * v).sum::<f32>().sqrt();
    if na * nb == 0.0 { 0.0 } else { dot / (na * nb) }
}
