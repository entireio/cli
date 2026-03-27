# AKF Integration — Trust Metadata for Checkpoints

## Overview

[AKF](https://github.com/HMAKT99/AKF) (Agent Knowledge Format) is the AI native file format — EXIF for AI. It embeds trust scores, source provenance, and compliance metadata into every file AI agents generate.

**Entire Checkpoints captures WHY code was written (context, reasoning, prompts).**
**AKF captures HOW TRUSTWORTHY that code is (trust scores, sources, compliance).**

Together they provide complete provenance: context + trust.

## How It Works

When Entire captures a checkpoint:

1. Entire records the session context (prompts, transcripts, tool calls)
2. AKF stamps the committed files with trust metadata:
   - Trust score (0-1) based on source tier
   - Source provenance chain
   - Compliance status (EU AI Act, SOX, HIPAA)
   - Integrity hash for tamper detection

## Integration Approach

### Option 1: Post-commit hook (alongside Entire hooks)

Add AKF stamping as a post-commit hook that runs after Entire captures context:

```json
{
  "hooks": {
    "PostCommit": [{
      "type": "command",
      "command": "git diff --name-only HEAD~1 | xargs -I{} python3 -m akf stamp {} --agent entire-checkpoint --evidence 'committed with Entire context'"
    }]
  }
}
```

### Option 2: Enrich checkpoint metadata

AKF metadata can be stored alongside Entire's checkpoint data on the `entire/checkpoints/v1` branch, adding trust scores to each checkpoint's file manifest.

### Option 3: Pre-push stamp

Before pushing, stamp all modified files so trust metadata travels with the code:

```bash
entire enable --agent claude-code
# ... agent generates code ...
# Entire captures context, AKF stamps trust metadata
git diff --name-only HEAD~1 | xargs -I{} akf stamp {} --agent claude-code --evidence "checkpointed by Entire"
git push  # Both context (Entire) and trust (AKF) travel with the code
```

## Why This Matters

- **47% of developers** distrust AI-generated code (Cognition/Devin, 2026)
- **EU AI Act Article 50** takes effect August 2, 2026 — AI content must carry transparency metadata
- Entire captures the reasoning; AKF captures the trust signal
- Together: reviewers see both WHY the code was written AND whether they can trust it

## Install

```bash
pip install akf
```

## Links

- [AKF — The AI Native File Format](https://akf.dev)
- [GitHub](https://github.com/HMAKT99/AKF)
- [Demo](https://huggingface.co/spaces/HANAKT19/akf)
