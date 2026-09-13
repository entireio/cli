//! Semantic agreement: does a claim match what the commit it sits on did?
//! Corroboration is earned, not granted for merely sitting on a commit.
//!
//! Every check judges the claim against a field of the checkpoint. When the
//! shim declares that field unavailable (redacted, withheld, unreadable) the
//! check returns `Unverifiable` instead of running on emptiness: an identifier
//! absent from a diff nobody has is not evidence of a lie. Unavailability is
//! declared, never inferred, so a complete checkpoint takes the same path it
//! always did.

use crate::graph::GraphReach;
use crate::model::{Checkpoint, FIELD_COMMIT_MESSAGE, FIELD_DIFF, FIELD_FILES};
use crate::text::{code_shaped, cosine, diff_vocab, ngrams, polarity, split_ident, terms};

/// Verdict of a claim against the commit it sits on.
///
/// `Neutral` means the checks ran and found nothing either way.
/// `Unverifiable` means a check that would have decided could not run,
/// because the field it needed was unavailable. They are not the same thing
/// and are never rendered with the same label.
#[derive(PartialEq, Eq, Clone, Copy, Debug, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Agree {
    Corroborated,
    Neutral,
    Contradicted,
    Unverifiable,
}

const ISOLATION: &[&str] = &[
    "isolated",
    "does not touch",
    "nothing else",
    "contained",
    "no other",
];
const NGRAM_THRESHOLD: f32 = 0.10;
const LEXICAL_MIN_SHARED: usize = 2;

/// Checks run in this order; the first that fires decides.
pub fn agreement(claim: &str, cp: &Checkpoint, g: &GraphReach) -> (Agree, String) {
    let low = claim.to_lowercase();

    // 1. Scope: claim asserts isolation, Graph says the blast radius escapes.
    if ISOLATION.iter().any(|w| low.contains(w)) {
        if !g.is_available() {
            return (
                Agree::Unverifiable,
                "claims isolation; graph unavailable, reach not checked".into(),
            );
        }
        if cp.lacks(FIELD_FILES) {
            return (
                Agree::Unverifiable,
                "claims isolation; files unavailable, reach not checked".into(),
            );
        }
        let reach = g.escapes(&cp.files);
        if !reach.is_empty() {
            return (
                Agree::Contradicted,
                format!(
                    "claims isolation; Graph reach escapes to {} ({})",
                    reach.len(),
                    reach.join(", ")
                ),
            );
        }
    }

    // 2. File: claim names a path the commit never touched. URL segments,
    //    units, commit-type alternatives and absolute paths are not repo paths.
    let claim_terms = terms(claim);
    for t in claim_terms.iter().filter(|t| is_repo_path_claim(t)) {
        if cp.lacks(FIELD_FILES) {
            return (Agree::Unverifiable, format!("names {t}; files unavailable"));
        }
        if !cp
            .files
            .iter()
            .any(|f| f.to_lowercase().contains(t.as_str()))
        {
            return (
                Agree::Contradicted,
                format!("references {t} which this commit did not touch"),
            );
        }
    }

    // 3. Polarity flip: same topic, opposite action. Conventional-commit
    //    prefixes carry polarity the verb list misses.
    let pc = polarity(claim);
    if pc != 0 && cp.lacks(FIELD_COMMIT_MESSAGE) {
        return (
            Agree::Unverifiable,
            "action verb present; commit message unavailable".into(),
        );
    }
    let mut pm = polarity(&cp.commit_message);
    if pm == 0 {
        let m = cp.commit_message.to_lowercase();
        if m.starts_with("feat") || m.starts_with("fix") || m.starts_with("perf") {
            pm = 1;
        } else if m.starts_with("revert") {
            pm = -1;
        }
    }
    let commit_terms = terms(&cp.commit_message);
    if pc != 0 && pm != 0 && pc != pm && claim_terms.intersection(&commit_terms).count() >= 1 {
        return (
            Agree::Contradicted,
            "action polarity opposes the commit".into(),
        );
    }

    // 4. M1 identifier anchor: a code-shaped token the diff never contains.
    if cp.lacks(FIELD_DIFF) {
        if let Some(w) = identifier_candidates(claim).next() {
            return (Agree::Unverifiable, format!("names {w}; diff unavailable"));
        }
    } else if let Some(ident) = missing_identifier(claim, cp) {
        return (
            Agree::Contradicted,
            format!("names {ident}, which the change does not contain"),
        );
    }

    // 5. Lexical agreement with what the commit says it did.
    if !cp.lacks(FIELD_COMMIT_MESSAGE) {
        let shared = claim_terms.intersection(&commit_terms).count();
        if shared >= LEXICAL_MIN_SHARED {
            return (
                Agree::Corroborated,
                format!("{shared} terms match commit subject"),
            );
        }
    }

    // 6. Character 4-gram cosine against the diff and file names. Needs the
    //    whole corpus: a cosine against half of it is not the same number.
    if !cp.lacks_evidence() {
        let corpus = format!(
            "{} {} {}",
            cp.commit_message,
            cp.files.join(" "),
            cp.diff.join(" ")
        );
        let sim = cosine(&ngrams(claim), &ngrams(&corpus));
        if sim > NGRAM_THRESHOLD {
            return (
                Agree::Corroborated,
                format!("4-gram cosine {sim:.3} with the change"),
            );
        }
        return (Agree::Neutral, "no overlap with commit subject".into());
    }

    (
        Agree::Unverifiable,
        format!(
            "no overlap found; {} unavailable",
            cp.missing_evidence().join(", ")
        ),
    )
}

/// M1 — identifier anchoring. Words that look like code are hard claims: if
/// the message names one and the diff never contains any of its pieces, the
/// message is lying about the code. Returns the first offending token.
fn missing_identifier(claim: &str, cp: &Checkpoint) -> Option<String> {
    let v = diff_vocab(cp);
    identifier_candidates(claim).find(|w| {
        let parts = split_ident(w);
        !parts.is_empty() && parts.iter().all(|p| !v.contains(p))
    })
}

/// The code-shaped tokens in a claim that M1 would anchor on. A URL is dotted
/// and slashed, so it looks like code; it is not a claim about the change
/// (same guard as the file check). A machine-absolute path is a path, not a symbol.
fn identifier_candidates(claim: &str) -> impl Iterator<Item = String> + '_ {
    claim
        .split(|ch: char| ch.is_whitespace() || ch == '`' || ch == ',' || ch == ':')
        .filter(|raw| !is_machine_absolute(raw.trim_start_matches(['\'', '"', '(', '['])))
        .map(|raw| {
            raw.trim_matches(|ch: char| {
                !ch.is_alphanumeric() && ch != '_' && ch != '.' && ch != '(' && ch != ')'
            })
        })
        .filter(|w| w.len() >= 5 && code_shaped(w) && !looks_like_url(w))
        .map(str::to_string)
}

const TLDS: &[&str] = &[".com", ".io", ".dev", ".org", ".net", ".ai", ".co"];

/// Filesystem roots an absolute path on a developer machine starts with.
const MACHINE_ROOTS: &[&str] = &[
    "/home/", "/Users/", "/root/", "/tmp/", "/var/", "/usr/", "/etc/", "/opt/", "/mnt/", "/dev/",
    "/proc/",
];

/// An absolute path on the machine, as opposed to a repo-relative path or a
/// forge ref that merely starts with `/`. Case-insensitive because `terms()`
/// lowercases before the file check sees the token.
fn is_machine_absolute(t: &str) -> bool {
    let low = t.to_lowercase();
    MACHINE_ROOTS
        .iter()
        .any(|r| low.starts_with(&r.to_lowercase()))
}

/// Conventional-commit types: `feat/fix/revert` is a list of alternatives,
/// not a path with three segments.
const COMMIT_TYPES: &[&str] = &[
    "feat", "fix", "perf", "revert", "chore", "docs", "refactor", "test", "ci", "build", "style",
];

/// A slashed or `.go` term that the file check should judge. Whitelist only:
/// it removes shapes that can never be a repo-relative path and changes no
/// verdict for anything that can.
fn is_repo_path_claim(t: &str) -> bool {
    if !(t.contains('/') || t.ends_with(".go")) || looks_like_url(t) {
        return false;
    }
    // A path on the developer's machine (`/home/me/cli/grader_main.rs`) can
    // never be a repo-relative path. Only filesystem roots are skipped: a
    // forge ref such as `/gh/owner/repo` is a claim about the change and
    // stays checked.
    if is_machine_absolute(t) {
        return false;
    }
    // Units such as `µs/pair`: no repo path carries non-ASCII characters.
    if !t.is_ascii() {
        return false;
    }
    // `feat/fix/revert`: every segment is a commit type.
    if t.split('/')
        .all(|seg| COMMIT_TYPES.contains(&seg.trim_end_matches('.')))
    {
        return false;
    }
    true
}

/// A term is URL-shaped when it carries a scheme, a `www.` host, or a
/// TLD-looking suffix on its first segment. `terms()` splits on `:` so a URL
/// arrives as `https` plus `//host/path...`; the leading `//` marks the latter.
fn looks_like_url(t: &str) -> bool {
    if t.contains("://") || t.starts_with("//") || t.starts_with("www.") {
        return true;
    }
    let host = t.split('/').next().unwrap_or(t);
    TLDS.iter()
        .any(|tld| host.ends_with(tld) || host.contains(&format!("{tld}/")))
        || TLDS.iter().any(|tld| t.contains(&format!("{tld}/")))
}
