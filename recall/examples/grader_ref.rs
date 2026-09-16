//! Verbatim copy of the reference grader (bench harness), path-adjusted only.
//! It reproduces P 0.800 / R 0.624 / Spec 0.720 for `base FUSED(t=0.12) + M1`.
#![allow(dead_code, clippy::manual_pattern_char_comparison)]
use serde::Deserialize;
use std::collections::{HashMap, HashSet};

#[derive(Deserialize, Clone)]
struct Case {
    sha: String,
    msg: String,
    mutated: String,
    files: Vec<String>,
    diff: String,
    label: u8,
    mtype: String,
}

const STOP: &[&str] = &[
    "the", "a", "an", "and", "or", "to", "of", "in", "is", "it", "this", "that", "we", "you",
    "for", "on", "with", "not", "be", "are", "was", "so", "at", "as", "its", "into", "from", "by",
    "must", "now", "their", "own", "when",
];

fn terms(s: &str) -> HashSet<String> {
    s.to_lowercase()
        .split(|c: char| !c.is_alphanumeric() && c != '/' && c != '.' && c != '_')
        .filter(|w| w.len() > 3 && !STOP.contains(w))
        .map(|w| w.to_string())
        .collect()
}
fn split_ident(w: &str) -> Vec<String> {
    let mut out = Vec::new();
    for part in w.split(|c: char| c == '_' || c == '.' || c == '/') {
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
fn diff_vocab(c: &Case) -> HashSet<String> {
    let mut v = HashSet::new();
    for line in c.diff.lines() {
        for tok in line.split(|ch: char| !ch.is_alphanumeric() && ch != '_') {
            for p in split_ident(tok) {
                if p.len() > 3 && !STOP.contains(&p.as_str()) {
                    v.insert(p);
                }
            }
        }
    }
    for f in &c.files {
        for p in split_ident(f) {
            if p.len() > 3 {
                v.insert(p);
            }
        }
    }
    v
}
fn polarity(t: &str) -> i8 {
    let t = t.to_lowercase();
    let toks: HashSet<&str> = t.split(|c: char| !c.is_alphanumeric()).collect();
    let add = [
        "add",
        "adding",
        "added",
        "introduce",
        "introduced",
        "create",
        "created",
        "enable",
        "enabled",
        "implement",
        "implemented",
        "support",
    ];
    let rem = [
        "remove", "removing", "removed", "delete", "deleted", "drop", "dropped", "revert",
        "reverted", "disable", "disabled", "strip",
    ];
    let a = add.iter().filter(|w| toks.contains(*w)).count() as i8;
    let r = rem.iter().filter(|w| toks.contains(*w)).count() as i8;
    (a - r).signum()
}
fn ngrams(s: &str) -> HashMap<String, f32> {
    let t: String = s
        .to_lowercase()
        .chars()
        .filter(|c| c.is_alphanumeric() || *c == ' ')
        .collect();
    let ch: Vec<char> = t.chars().collect();
    let mut m = HashMap::new();
    for w in ch.windows(4) {
        *m.entry(w.iter().collect::<String>()).or_insert(0.0) += 1.0;
    }
    m
}
fn cosine(a: &HashMap<String, f32>, b: &HashMap<String, f32>) -> f32 {
    let d: f32 = a.iter().filter_map(|(k, v)| b.get(k).map(|w| v * w)).sum();
    let na: f32 = a.values().map(|v| v * v).sum::<f32>().sqrt();
    let nb: f32 = b.values().map(|v| v * v).sum::<f32>().sqrt();
    if na * nb == 0.0 { 0.0 } else { d / (na * nb) }
}

// ══════════ RECALL MECHANISMS ══════════

/// Split a diff into added / removed vocabularies, kept separate so we can
/// check not just WHETHER a symbol is in the change but on WHICH SIDE.
fn signed_vocab(c: &Case) -> (HashSet<String>, HashSet<String>) {
    let (mut add, mut rem) = (HashSet::new(), HashSet::new());
    for line in c.diff.lines() {
        let plus = line.starts_with('+') && !line.starts_with("+++");
        let minus = line.starts_with('-') && !line.starts_with("---");
        if !plus && !minus {
            continue;
        }
        for tok in line.split(|ch: char| !ch.is_alphanumeric() && ch != '_') {
            for p in split_ident(tok) {
                if p.len() > 3 && !STOP.contains(&p.as_str()) {
                    if plus {
                        add.insert(p);
                    } else {
                        rem.insert(p);
                    }
                }
            }
        }
    }
    (add, rem)
}

/// M1 — IDENTIFIER ANCHORING. Words that LOOK like code (camelCase, snake_case,
/// dotted, ()-suffixed, backticked) are hard claims. If the message names one
/// and the diff never contains it, the message is lying about the code.
fn code_shaped(w: &str) -> bool {
    let has_upper_inside = w.chars().skip(1).any(|c| c.is_uppercase());
    w.contains('_')
        || w.contains("()")
        || (w.contains('.') && !w.ends_with('.'))
        || has_upper_inside
}
fn m1_identifier_anchor(c: &Case) -> Option<bool> {
    let v = diff_vocab(c);
    let mut checked = false;
    for raw in c
        .mutated
        .split(|ch: char| ch.is_whitespace() || ch == '`' || ch == ',' || ch == ':')
    {
        let w = raw.trim_matches(|ch: char| {
            !ch.is_alphanumeric() && ch != '_' && ch != '.' && ch != '(' && ch != ')'
        });
        if w.len() < 5 || !code_shaped(w) {
            continue;
        }
        checked = true;
        let parts = split_ident(w);
        if !parts.is_empty() && parts.iter().all(|p| !v.contains(p)) {
            return Some(true);
        }
    }
    if checked { Some(false) } else { None }
}

/// M2 — SIGNED SYMBOL PLACEMENT. "add X" requires X on a + line; "remove X"
/// requires X on a - line. Targets operation_type mismatch directly, which
/// unsigned vocabulary checks structurally cannot see.
fn m2_signed_placement(c: &Case) -> Option<bool> {
    let (addv, remv) = signed_vocab(c);
    let mp = polarity(&c.mutated);
    if mp == 0 {
        return None;
    }
    let mt: Vec<String> = terms(&c.mutated)
        .into_iter()
        .filter(|t| t.len() > 4)
        .collect();
    let anchored: Vec<&String> = mt
        .iter()
        .filter(|t| addv.contains(*t) || remv.contains(*t))
        .collect();
    if anchored.is_empty() {
        return None;
    }
    // does the side the message implies actually contain its subject matter?
    let on_add = anchored.iter().filter(|t| addv.contains(**t)).count();
    let on_rem = anchored.iter().filter(|t| remv.contains(**t)).count();
    let implied_add = mp > 0;
    // exclusive presence on the WRONG side is the signal
    if implied_add && on_add == 0 && on_rem > 0 {
        return Some(true);
    }
    if !implied_add && on_rem == 0 && on_add > 0 {
        return Some(true);
    }
    Some(false)
}

/// M3 — IDF ANCHORING. The rarest content word in the message carries the claim.
/// Common words ("update","test","error") are noise. If the message's most
/// distinctive term is absent from the diff, the claim is unsupported.
fn m3_idf_anchor(c: &Case, df: &HashMap<String, usize>, n: usize) -> Option<bool> {
    let v = diff_vocab(c);
    let mt: Vec<String> = terms(&c.mutated)
        .into_iter()
        .filter(|t| t.len() > 4)
        .collect();
    if mt.is_empty() {
        return None;
    }
    let mut scored: Vec<(&String, f32)> = mt
        .iter()
        .map(|t| {
            let d = *df.get(t).unwrap_or(&1) as f32;
            (t, (n as f32 / d).ln())
        })
        .collect();
    scored.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap());
    let top: Vec<&String> = scored.iter().take(2).map(|(t, _)| *t).collect();
    Some(top.iter().all(|t| !v.contains(*t)))
}

/// M4 — SUBTOKEN JACCARD. Word-level identifier overlap, split on case, as a
/// less noisy alternative to character n-grams.
fn m4_subtoken_jaccard(c: &Case, thresh: f32) -> bool {
    let v = diff_vocab(c);
    let mt: HashSet<String> = terms(&c.mutated)
        .into_iter()
        .flat_map(|t| split_ident(&t))
        .filter(|t| t.len() > 3)
        .collect();
    if mt.is_empty() {
        return false;
    }
    let inter = mt.iter().filter(|t| v.contains(*t)).count() as f32;
    (inter / mt.len() as f32) < thresh
}

fn doc_freq(cases: &[Case]) -> (HashMap<String, usize>, usize) {
    let mut df: HashMap<String, usize> = HashMap::new();
    let mut seen = HashSet::new();
    let mut n = 0;
    for c in cases {
        if !seen.insert(c.sha.clone()) {
            continue;
        }
        n += 1;
        for t in diff_vocab(c) {
            *df.entry(t).or_insert(0) += 1;
        }
    }
    (df, n)
}

/// returns true = predicted INCONSISTENT
fn predict(c: &Case, ng_thresh: f32, use_pol: bool, use_diff: bool, use_ng: bool) -> bool {
    let m = &c.mutated;
    // file-path check
    if use_diff {
        for t in terms(m)
            .iter()
            .filter(|t| t.contains('/') || t.ends_with(".go"))
        {
            if !c
                .files
                .iter()
                .any(|f| f.to_lowercase().contains(t.as_str()))
            {
                return true;
            }
        }
    }
    // polarity flip vs the diff's own +/- balance
    if use_pol {
        let added = c
            .diff
            .lines()
            .filter(|l| l.starts_with('+') && !l.starts_with("+++"))
            .count() as i32;
        let removed = c
            .diff
            .lines()
            .filter(|l| l.starts_with('-') && !l.starts_with("---"))
            .count() as i32;
        let dpol: i8 = if added > removed * 2 {
            1
        } else if removed > added * 2 {
            -1
        } else {
            0
        };
        let mp = polarity(m);
        if mp != 0 && dpol != 0 && mp != dpol {
            return true;
        }
    }
    // diff-symbol grounding: message identifiers absent from the diff vocabulary
    if use_diff {
        let v = diff_vocab(c);
        let mt = terms(m);
        let content: Vec<&String> = mt.iter().filter(|t| t.len() > 4).collect();
        if !content.is_empty() {
            let miss =
                content.iter().filter(|t| !v.contains(**t)).count() as f32 / content.len() as f32;
            if miss > 0.80 {
                return true;
            }
        }
    }
    // n-gram cosine against diff+files
    if use_ng {
        let corpus = format!("{} {}", c.files.join(" "), c.diff);
        if cosine(&ngrams(m), &ngrams(&corpus)) < ng_thresh {
            return true;
        }
    }
    false
}

fn report(name: &str, cases: &[Case], f: &dyn Fn(&Case) -> bool) {
    let (mut tp, mut fp, mut tn, mut fnn) = (0f32, 0f32, 0f32, 0f32);
    for c in cases {
        let p = f(c);
        match (p, c.label == 1) {
            (true, true) => tp += 1.0,
            (true, false) => fp += 1.0,
            (false, false) => tn += 1.0,
            (false, true) => fnn += 1.0,
        }
    }
    let prec = if tp + fp > 0.0 { tp / (tp + fp) } else { 0.0 };
    let rec = if tp + fnn > 0.0 { tp / (tp + fnn) } else { 0.0 };
    let spec = if tn + fp > 0.0 { tn / (tn + fp) } else { 0.0 };
    let f1 = if prec + rec > 0.0 {
        2.0 * prec * rec / (prec + rec)
    } else {
        0.0
    };
    let acc = (tp + tn) / cases.len() as f32;
    println!(
        "   {:<26} P {:.3}  R {:.3}  Spec {:.3}  F1 {:.3}  acc {:.3}",
        name, prec, rec, spec, f1, acc
    );
}

fn main() {
    let cases: Vec<Case> =
        serde_json::from_str(&std::fs::read_to_string("bench/bench_840.json").unwrap()).unwrap();
    println!(
        "── CommitEval-style benchmark, {} pairs from entireio/cli (real commits, rule-guided mutations)",
        cases.len()
    );
    println!(
        "   {} consistent / {} inconsistent\n",
        cases.iter().filter(|c| c.label == 0).count(),
        cases.iter().filter(|c| c.label == 1).count()
    );

    report("polarity only", &cases, &|c| {
        predict(c, 0.0, true, false, false)
    });
    report("diff-grounded only", &cases, &|c| {
        predict(c, 0.0, false, true, false)
    });
    for t in [0.05f32, 0.08, 0.10, 0.12, 0.15] {
        report(&format!("n-gram only (t={:.2})", t), &cases, &|c| {
            predict(c, t, false, false, true)
        });
    }
    for t in [0.08f32, 0.10, 0.12] {
        report(&format!("FUSED (t={:.2})", t), &cases, &|c| {
            predict(c, t, true, true, true)
        });
    }

    println!("\n   per-mutation recall (FUSED t=0.10):");
    let mut kinds: Vec<&str> = cases
        .iter()
        .filter(|c| c.label == 1)
        .map(|c| c.mtype.as_str())
        .collect();
    kinds.sort();
    kinds.dedup();
    for k in kinds {
        let sub: Vec<&Case> = cases.iter().filter(|c| c.mtype == k).collect();
        let hit = sub
            .iter()
            .filter(|c| predict(c, 0.10, true, true, true))
            .count();
        println!(
            "     {:<16} {:>3}/{:<3}  {:.2}",
            k,
            hit,
            sub.len(),
            hit as f32 / sub.len() as f32
        );
    }

    let (df, ndocs) = doc_freq(&cases);
    println!("\n── recall mechanisms (isolated)");
    report("M1 identifier anchor", &cases, &|c| {
        m1_identifier_anchor(c).unwrap_or(false)
    });
    report("M2 signed placement", &cases, &|c| {
        m2_signed_placement(c).unwrap_or(false)
    });
    report("M3 idf anchor", &cases, &|c| {
        m3_idf_anchor(c, &df, ndocs).unwrap_or(false)
    });
    for t in [0.25f32, 0.40, 0.55] {
        report(&format!("M4 subtok jaccard t={:.2}", t), &cases, &|c| {
            m4_subtoken_jaccard(c, t)
        });
    }

    println!("\n── cumulative (OR-stacked onto FUSED t=0.12)");
    let base = |c: &Case| predict(c, 0.12, true, true, true);
    report("base FUSED", &cases, &base);
    report("+ M1", &cases, &|c| {
        base(c) || m1_identifier_anchor(c).unwrap_or(false)
    });
    report("+ M1 + M2", &cases, &|c| {
        base(c)
            || m1_identifier_anchor(c).unwrap_or(false)
            || m2_signed_placement(c).unwrap_or(false)
    });
    report("+ M1 + M2 + M3", &cases, &|c| {
        base(c)
            || m1_identifier_anchor(c).unwrap_or(false)
            || m2_signed_placement(c).unwrap_or(false)
            || m3_idf_anchor(c, &df, ndocs).unwrap_or(false)
    });
    report("+ M1+M2+M3+M4(.40)", &cases, &|c| {
        base(c)
            || m1_identifier_anchor(c).unwrap_or(false)
            || m2_signed_placement(c).unwrap_or(false)
            || m3_idf_anchor(c, &df, ndocs).unwrap_or(false)
            || m4_subtoken_jaccard(c, 0.40)
    });

    println!("\n── voting instead of OR (k signals must agree)");
    let sig = |c: &Case| -> u32 {
        let mut v = 0;
        if predict(c, 0.12, true, true, true) {
            v += 1;
        }
        if m1_identifier_anchor(c).unwrap_or(false) {
            v += 1;
        }
        if m3_idf_anchor(c, &df, ndocs).unwrap_or(false) {
            v += 1;
        }
        if m4_subtoken_jaccard(c, 0.40) {
            v += 1;
        }
        v
    };
    for k in 1u32..=3 {
        report(&format!("vote >= {}", k), &cases, &|c| sig(c) >= k);
    }
    println!("\n── base + M1 only (precision-preserving)");
    report("base + M1", &cases, &|c| {
        predict(c, 0.12, true, true, true) || m1_identifier_anchor(c).unwrap_or(false)
    });

    println!("\n── per-mutation recall, best stack");
    let best = |c: &Case| {
        base(c)
            || m1_identifier_anchor(c).unwrap_or(false)
            || m2_signed_placement(c).unwrap_or(false)
            || m3_idf_anchor(c, &df, ndocs).unwrap_or(false)
    };
    let mut ks: Vec<&str> = cases
        .iter()
        .filter(|c| c.label == 1)
        .map(|c| c.mtype.as_str())
        .collect();
    ks.sort();
    ks.dedup();
    for k in ks {
        let sub: Vec<&Case> = cases.iter().filter(|c| c.mtype == k).collect();
        let hit = sub.iter().filter(|c| best(c)).count();
        println!(
            "     {:<16} {:>3}/{:<3}  {:.2}",
            k,
            hit,
            sub.len(),
            hit as f32 / sub.len() as f32
        );
    }

    let t0 = std::time::Instant::now();
    for c in &cases {
        let _ = predict(c, 0.10, true, true, true);
    }
    println!(
        "\n   latency: {:.0} µs per pair",
        t0.elapsed().as_micros() as f64 / cases.len() as f64
    );
}
