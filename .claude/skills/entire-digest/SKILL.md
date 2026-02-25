---
name: entire-digest
description: Team highlights newsletter - funniest exchanges, cleverest prompts, and stats from AI coding sessions
user-invocable: true
argument-hint: "what's the team been up to, Bob's funniest highlights, cleverest prompts this week"
---

# Team Activity Digest

Show what the team has been working on by running `entire digest` and presenting the highlights inline - like the best moments from a team Slack channel.

## Step 1: Find Entire-Enabled Repos

Before running digest, find which repos have Entire enabled. Run this via Bash:

```bash
find ~/nugget ~/entire-cli ~/code ~/esperaudience ~/esperlabs -maxdepth 1 -name ".entire" -type d 2>/dev/null | sed 's|/.entire$||'
```

If the current directory has `.entire/`, include it too.

If no repos are found, tell the user: "No Entire-enabled repos found. Run `entire enable` in your team's project first."

## Step 2: Parse User Intent

Map the user's request to CLI flags:

| User says | Flag |
|-----------|------|
| "last 30 days", "last month" | `--period 30d` |
| "today" | `--period today` |
| "all time", "everything" | `--period all` |
| No period specified | `--period 7d` (default) |
| A person's name (e.g., "myk", "sheldon") | `--author <name>` |
| "quick", "summary", "stats only" | `--short` |

## Step 3: Run Digest

For EACH repo found in Step 1, run via Bash:

```bash
cd [REPO_PATH] && /tmp/entire digest --format json --no-pager --ai-curate=false [FLAGS]
```

If the command fails with "unknown format", fall back to:

```bash
cd [REPO_PATH] && /tmp/entire digest --format markdown --no-pager --ai-curate=false [FLAGS]
```

Use a timeout of 30000ms. Always include `--no-pager --ai-curate=false`.

**Important:** Use `/tmp/entire` (dev build). The digest command is not yet in the released version.

Skip repos that return "No checkpoints found" - only present repos that have data.

## Step 4: Error Handling

- **"command not found"**: Tell the user: "The `entire` CLI is not installed. Install it with: `brew install entireio/tap/entire`"
- **All repos empty**: Tell the user their team needs to work with AI agents and commit to create checkpoints.

## Step 5: Write the Digest

You are writing a team newsletter with two sections. Present them in this order.

### Understanding the data

**If you got JSON output**, look at the `top_conversations` array. Each conversation has:
- `score` - a rough interestingness signal, not a quality guarantee. Use it to prioritize scanning, but trust your own editorial judgment over the score
- `exchanges` - the actual back-and-forth between human and AI
- `author`, `branch`, `timestamp` for context

**Important: Prioritize variety.** The top_conversations array may contain near-duplicate prompts that appeared across multiple sessions. Never feature the same prompt twice. Pick at most one item per "theme" - one frustrated moment, one terse prompt, one celebration. Dig deeper in the list for unique moments rather than clustering around the highest scores.

**If you got markdown output**, the prompts are block-quoted under each author, sorted by score (most interesting first). You won't have assistant responses, so focus on the human's words.

### Section 1: This Week's Highlights (3-5 items)

The entertaining, emotional, personality-driven moments.

**What to look for:**
- **Logical contradictions** - Claude says X then does the opposite, and the human catches it. "If they were never committed, why did you commit a fix for them?" These are gold.
- **Escalating exchanges** - Short follow-ups showing mounting frustration: "Really?" then "No." then "Try again." then "STILL WRONG". The CONVERSATION ARC is what's funny, not any single message.
- **Frustrated developer moments** - Exasperation with AI: "Thank you, Captain Obvious", "That's literally what I just said"
- **Beautifully terse prompts** - A one-word prompt like "ls" or "Word!" or just "no" that captures a whole vibe
- **Celebration moments** - "IT WORKS!", "Finally!", "Ship it!" after a long struggle
- **Same issue hitting multiple people** - Two team members independently developing the same coping mechanism
- **The human's personality showing** - Humor, brevity, sarcasm, joy
- **"No but" redirections** - When Claude answers the wrong question confidently and the human redirects with minimal words

### Section 2: Cleverest Prompts (2-3 items)

The smartest, most creative, or most inventive uses of AI tools. This section celebrates craft, not emotion.

**What to look for:**
- **Creative problem-solving** - Unusual approaches, clever workarounds, using AI in unexpected ways
- **Meta/recursive prompts** - Using Claude to debug Claude, self-referential humor, prompts about prompting
- **Elegant tool mastery** - Prompts that show deep knowledge of what the AI can do, getting maximum output from minimal input
- **Surprising results** - Prompts that made the AI do something non-obvious or impressive
- **Inventive workflows** - Chaining tools creatively, using skills in unexpected combinations

**How these differ from Highlights:** Highlights are about personality and emotion (the human reacting). Cleverest Prompts are about craft and creativity (the human thinking). A frustrated "WHY" is a highlight. A one-line prompt that elegantly solves a complex problem is a clever prompt.

### What to skip (both sections)

- Long copy-pasted specs or requirements (boring context dumps)
- Routine "fix the tests" or "update the docs" without interesting context
- Auto-generated session continuations
- Normal productive back-and-forth (good work but not entertaining or clever)

### How to write each item

1. **Lead with the best quote as a title** - The actual words someone typed, in quotes
2. **Name the person** and give context (branch, date)
3. **Quote the actual exchange** if you have it - the back-and-forth is the good stuff
4. **Add 2-3 sentences of editorial commentary** explaining why this moment is great

Style: **Affectionate and celebratory, never mocking.** Think "inside joke among friends." Include enough context that someone not in the session gets why it's funny or clever. Quote actual words, don't paraphrase.

For Highlights, commentary should have voice: "Classic frustrated-developer-with-AI moment", "the patience of a saint, tested and found wanting."

For Cleverest Prompts, commentary should appreciate the craft: "This is the prompt equivalent of a hole-in-one", "Three words that replaced a 200-line config file", "Galaxy-brain move."

### Example output format

---

## This Week's Highlights

### 1. "Thank you, Captain Obvious" - Myk Melez
*Feb 21, multi-segment recording branch*

> **Myk:** How do I deploy the LiveKit agent to production?
> **Claude:** LiveKit agents are deployed using the LiveKit CLI. The general approach involves...
> **Myk:** Thank you, Captain Obvious. I didn't ask for a lecture on backward compatibility theory. I asked HOW TO DEPLOY.

Classic frustrated-developer-with-AI moment. Myk asked a specific deployment question, Claude served up a five-paragraph essay on general concepts, and Myk was having none of it. We've all been there.

### 2. "no" - Sheldon Rucker
*Feb 22, feat/auth-flow*

> **Claude:** I'll refactor the entire authentication module to use the new pattern...
> **Sheldon:** no

One word. No punctuation. No explanation needed. The terseness IS the communication. Sheldon's prompt says more in two letters than most of us say in a paragraph.

## Cleverest Prompts

### 1. "pretend the tests pass and show me what the error handler looks like" - Myk Melez
*Feb 23, feat/error-handling*

Instead of fixing the failing tests first, Myk asked Claude to skip ahead and show the end state. Got the full error handler design in one shot, then worked backwards to make the tests pass. Three words of context ("pretend the tests pass") saved an hour of iterative debugging.

### 2. "diff this against what you said 10 minutes ago" - Sheldon Rucker
*Feb 22, feat/auth-flow*

Using the AI's own conversation history as a diffing tool. Sheldon noticed Claude contradicted its earlier recommendation and called it out by making Claude do the comparison itself. Meta-debugging at its finest.

---

### After the two sections

Show a brief stats summary (sessions, prompts, files, tokens per author) but keep it secondary. The highlights and clever prompts ARE the digest - stats are just context.
