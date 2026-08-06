# Agentic-Era Product Story and Workshop Session Notes

This document captures the main ideas, drafts, decisions, and open questions from a working session about how to explain Entire and how to turn that story into a hands-on workshop.

It is not a finished CFP submission. It is a record of the thinking so far.

## Writing and audience guidelines

- Write at an eighth-grade reading level or below.
- Lead with real developer problems, not product features.
- Speak to students, indie developers, open-source maintainers, and professional developers—not only enterprise teams.
- Keep the tone useful and honest instead of overly sales-focused.
- Do not claim that Entire captures a model's private chain of thought. Entire captures the visible session record, including prompts, tool calls, attempts, results, and code state.
- Do not claim that data about rising activity proves existing code hosts are failing. It shows that code activity and automation are growing.

## Main product story

Software development tools were built around developers making and reviewing a limited number of changes at a time.

Coding agents changed the pace. One developer might now produce ten pull requests in a day instead of spending two weeks on one. Agents can also keep working while developers sleep.

That can feel empowering, productive, and freeing. But generating more code creates problems throughout the rest of the software development lifecycle.

The promise of coding agents is not simply more code. It is giving every developer more power to create. Developers should not have to give up understanding, confidence, or control to get that power.

Entire provides development infrastructure for the work that happens around code generation.

## Core developer problems and Entire products

### Git Hosting

**Developer problem:**

> As a developer, I want my code host to handle the traffic agents create without slowing down or becoming less reliable.

Many agents can clone, branch, push, and run checks at the same time and throughout the day.

**Entire's role:**

Entire provides Git hosting built to handle many agents working in parallel. It mirrors repositories from GitHub into Entire, so developers can use Entire for agent-driven work without fully moving away from GitHub.

### Graph

**Developer problem:**

> As a developer, I want agents to find the right code without wasting tokens searching through the whole codebase.

Agents can spend tokens opening unrelated files, following the wrong path, and rereading the same code.

**Entire's role:**

Graph points agents to relevant code and shows how it connects. It can return ranked code regions, symbols, callers, dependencies, and possible impact. This gives the agent focused context instead of the whole codebase.

Graph does not act as a general memory of everything earlier agents learned. It maps the code and its relationships outside the model.

### Sessions

**Developer problem:**

> As a developer, I want to continue agent work in a new session without repeating the goal, past decisions, and progress—or using context that no longer matches the code.

When a session ends, the next session may not know the goal, what has already been tried, or why earlier decisions were made. Loose memory can also bring back old information that no longer matches the code.

**Entire's role:**

Sessions save the history of the agent's work and tie it to the version of the code where it happened. This helps developers continue with the right context instead of starting over.

### Trails

**Developer problem:**

> As a developer, I want to verify agent-written code faster and feel confident about what I merge.

A diff shows what changed. It may not show what the developer asked for, what the agent tried, what evidence supports the change, or what must pass before merge.

**Entire's role:**

Trails put intent, evidence, and merge rules in one place. They show what the developer asked for, what the agent changed, the test and review results, and what must pass before the code can merge.

The checks do not automatically prove that the evidence matches the intent. They make it easier for a developer to compare the two and make a decision.

### Entire Search

Entire Search and Graph should not be presented as the same feature.

The simplest distinction is:

> Search finds what happened before. Graph finds where to work now.

Entire Search primarily searches checkpoints, commits, and sessions using semantic and keyword matching. It can search across repositories and filter by author, date, branch, and repository. The CLI also provides a separate `--code` path for hosted code-content search.

Graph searches the current repository or working tree. It returns focused code regions and code relationships for the task at hand.

For a workshop:

- Use Search to find earlier work, attempts, or decisions.
- Use Graph to find and understand the current code.
- Avoid teaching hosted `entire search --code` and Graph with the same example because their code-search stories overlap.

## Supporting evidence

### Git Hosting

- Commits grew 25.1% and merged pull requests grew 29% in 2025. [GitHub Octoverse 2025](https://github.blog/news-insights/octoverse/octoverse-a-new-developer-joins-github-every-second-as-ai-leads-typescript-to-1/)
- GitHub Actions use grew 35% in 2025. [GitHub analysis of 2025 code activity](https://github.blog/news-insights/octoverse/what-986-million-code-pushes-say-about-the-developer-workflow-in-2025/)
- Major code hosts place limits on API or automated activity: [GitHub](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api), [GitLab](https://docs.gitlab.com/administration/dedicated/user_rate_limits/), and [Bitbucket](https://support.atlassian.com/bitbucket-cloud/docs/api-request-limits/).

These sources show growing activity and finite shared capacity. They do not prove that agent traffic is already causing widespread code-host outages.

### Graph and token use

- Agent coding tasks can use 1,000 times more tokens than code chat.
- Runs of the same task can differ by as much as 30 times in token use. [Stanford Digital Economy Lab](https://digitaleconomy.stanford.edu/publication/how-do-ai-agents-spend-your-money-analyzing-and-predicting-token-consumption-in-agentic-coding-tasks/)
- Focused repository search reduced main-agent token use by as much as 60%. [Microsoft FastContext study](https://arxiv.org/abs/2606.14066)

### Sessions and context

- 65% of developers say AI misses important context.
- Reported context problems fell from 54% to 16% when context was saved and reused across sessions. [Qodo 2025 State of AI Code Quality](https://www.qodo.ai/reports/state-of-ai-code-quality/)
- Researchers found stale code references in 23% of the repository instruction files they reviewed. [Context Rot study](https://arxiv.org/abs/2606.09090)

### Trails and review

- 96% of developers do not fully trust AI-generated code.
- 38% say reviewing AI-generated code takes more effort than reviewing human-written code. [Sonar 2026 State of Code](https://www.sonarsource.com/blog/state-of-code-developer-survey-report-the-current-reality-of-ai-coding/)

## Slide-deck direction

The working deck title is:

> **Rebuilding the SDLC for the Agentic Era**

The product order is:

1. Git Hosting
2. Graph
3. Sessions
4. Trails

The narrative is:

> Host the code → find the right context → preserve the work → merge with confidence.

Trails goes last because it provides the emotional payoff: the developer understands the change, sees the evidence, and controls the merge.

The working closing is:

> We've solved the problem of generating code quickly. Entire is built for our industry's next challenge: ensuring developers can understand, trust, and maintain agent-generated code.

## Workshop directions explored

### Broad developer theme

One direction was a workshop about the changing role of a software developer:

> **How to Be a Software Developer in the Agentic Era**

The idea was that agents can write code, but developers are still responsible for goals, context, decisions, evidence, and the merge.

Possible titles included:

- Agents Write Code. Developers Build Software.
- So the Agent Wrote the Code. Now What?
- The Prompt Is Just the Beginning.
- You're Still the Developer.
- How to Develop Software When Agents Write the Code.
- Your Agent Can Code. Can You Ship?
- Don't Just Prompt. Develop.
- From Vibes to Verified.

This theme is broad and inviting, but it does not automatically create a clear hands-on activity.

### Review-focused workshop

A separate proposal centered on this title:

> **Why Is This Code Here? A Hands-On Workshop on Reviewing Work You Didn't Write**

Its core exercise was:

1. Review prepared changes using only the diff.
2. Approve or reject each change.
3. Reveal the session history behind the change.
4. Review the change again with more context.
5. Discuss whether the decision changed.

The current proposed copy was:

> AI agents now write more code than any team can realistically review. Some say that's a volume problem, but the real issue underneath it is context. A pull request shows you what changed line by line, but it rarely shows you why the change exists, what the agent tried and threw away along the way, or how anyone actually knows it works. It's honestly silly that developers keep reviewing code the way we always have—reading lines, checking syntax—when the "author" made a bunch of decisions we never got to see.
>
> This workshop is about closing that gap with Entire. Entire captures agent sessions as checkpoints in your git history—prompts, reasoning, tool calls, working state—linked to the commit they produced. We'll look at how that captured reasoning can become the raw material for a different kind of code review.
>
> We'll start by having you review some real changes blind—just the diff, the way you'd review today. You'll make real approve/request-changes calls, note a quick reason for each, and commit to them. Then we'll turn the reasoning on, look at the Entire checkpoints behind those changes, and see how many calls flip. Code you approved might suddenly look wrong; changes you flagged might turn out to be exactly right. The code didn't change, but what you could see about why it exists did.
>
> From there, we'll work more the way Entire is meant for: reviewing intent, not just inspecting output. That might mean using checkpoint history as evidence, running a review across a branch with full reasoning context before a PR opens, or looking at how things like Trails and Gates shift the unit of review from the diff itself toward the path from intent to outcome. The goal is reviewing at AI scale—asking whether the code does what it was meant to, not just whether the lines look right.
>
> Bring a laptop. You'll leave reviewing a bit differently, and with Entire set up on a repo of your own.

Concerns about this direction:

- It is still the same basic blind-review-and-reveal workshop format used before, with Entire added to the reveal.
- It focuses on prepared changes instead of having participants create their own work.
- It treats Entire mainly as a review-context tool.
- Git Hosting, Search, and Graph are barely present.
- Participants do not clearly create their own Trail.
- "Reasoning" is used repeatedly even though "session history" or "work history" is more accurate.
- "It's honestly silly" may make experienced reviewers feel blamed.
- Phrases like "that might mean" and "things like Trails and Gates" make the practical part sound undecided.

This could still work as a focused 60-to-90-minute review workshop. It is not the same as an end-to-end Entire workshop.

## Desired workshop: end-to-end creation

The desired workshop is a live build challenge. Participants should not only inspect artifacts created by the presenter. They should create the artifacts themselves.

Every participant should leave with:

- A repository hosted or mirrored on Entire
- An agent Session
- Their own checkpoints
- A Search result
- A Graph result
- A Session that was stopped and continued
- Their own Trail
- Test and review evidence
- Gates or merge rules that pass
- A change that is ready to merge

The central promise is:

> Start with a repository. End with a passing Trail and a trusted merge.

The end-to-end flow is:

1. Host or mirror a repository on Entire.
2. Start a Session around a real task.
3. Use Search to find related work.
4. Use Graph to find the right current code.
5. Work with an agent to make the change.
6. Create checkpoints while the work is happening.
7. Stop and continue the work in a new Session.
8. Create a Trail.
9. Add test and review evidence.
10. Run the Gates or merge rules.
11. Fix anything that fails.
12. Merge when the required checks pass.

Review is part of this journey, but it is not the whole workshop.

## Competition ideas

### Green Trail challenge

The first competition idea was to see who could get their Trail to the highest passing state.

The question is not only who can generate code fastest. It is who can provide the strongest proof that the change is ready.

### Token Budget challenge

Everyone solves the same task. A change only qualifies if its Trail passes. Among passing Trails, the participant who uses the fewest tokens wins.

This makes Graph and token efficiency central.

### Context Relay

One developer starts the task and creates checkpoints. Another developer must continue in a new Session without receiving a verbal explanation.

This demonstrates whether the saved context is useful enough for someone else to continue.

### Agent Handoff challenge

Participants begin the task with one coding agent and finish it with another. They should not have to rewrite the original prompt or reconstruct the entire task.

This demonstrates that the work history does not belong to one agent provider.

### Repo Rescue

Participants receive a repository containing a partly finished feature, a stopped Session, failed tests, several checkpoints, and an earlier wrong approach.

They must discover what happened, continue from the right point, fix the change, and create a passing Trail.

### Evidence challenge

The winner is not the person with the most code. The winner has the strongest evidence that the change satisfies the intent.

Evidence could include useful tests, review results, clear checkpoints, and merge rules that catch an incorrect solution.

### Search scavenger hunt

Important clues are hidden across commits, Sessions, checkpoints, prompts, and earlier attempts. Participants must use Entire Search to find them before coding.

### Blast Radius challenge

Before editing, participants use Graph to predict which callers, files, tests, or related code could be affected. Their prediction is compared with the final change.

### Multi-Agent race

Participants use several agents on parts of the same task. They must prevent duplicate work, bring the changes together, resolve conflicts, and produce one passing Trail.

### Agentic Gauntlet

This combines the whole platform into one score. Participants could earn points for:

- Hosting the repository on Entire
- Finding related work with Search
- Finding the correct code with Graph
- Creating checkpoints
- Continuing in a new Session
- Passing tests and merge rules
- Creating and merging a Trail
- Staying within a token budget

A green Trail could be required to qualify rather than being the only scoring method.

Possible awards:

- First passing Trail
- Lowest token use among passing Trails
- Best Session handoff
- Strongest evidence
- Best recovery from failure
- Best use of Graph
- Best overall score

## Workshop title ideas

Titles connected to the end-to-end challenge included:

- Repo to Green
- From Repo to Green
- Can Your Agent Get to Green?
- Can You Get Your Agent to Green?
- Build It. Prove It. Merge It.
- Prompt. Checkpoint. Prove. Merge.
- The Race to a Trusted Merge
- The Race to a Green Trail
- The Agentic Development Challenge
- The Agentic Gauntlet
- The Repo-to-Merge Challenge
- From Clone to Trusted Merge
- The Trail to Green

The clearest short title so far is:

> **Repo to Green**

The most playful competition title is:

> **Can You Get Your Agent to Green?**

## Two-hour end-to-end outline

Even in two hours, the workshop should be one continuous journey rather than separate product demos.

| Time | Participant activity |
| --- | --- |
| 0–10 minutes | Learn the challenge and choose the task |
| 10–25 minutes | Mirror the repository into Entire |
| 25–35 minutes | Start a Session and state the goal |
| 35–45 minutes | Search for related work |
| 45–60 minutes | Use Graph to find the right code |
| 60–80 minutes | Work with the agent and create checkpoints |
| 80–90 minutes | Stop and continue in a new Session |
| 90–105 minutes | Finish the change and create a Trail |
| 105–115 minutes | Run checks, fix failures, and improve the Trail |
| 115–120 minutes | Merge, compare results, and announce winners |

## Logistics for 80 participants

- Use the same prepared starter repository and task for a fair competition.
- Let each participant mirror their own copy, create their own Session, and create their own Trail.
- Require installation and login before the workshop.
- Provide a preflight command participants can run before arriving.
- Prepare backup screenshots or saved results in case a live service fails.
- Have several helpers available for setup and account problems.
- Confirm that the platform can support the expected number of simultaneous mirrors, clones, agent Sessions, Search requests, Graph queries, checkpoints, Trails, and checks.
- If the competition uses token count or Trail state, confirm that the number is visible and measured consistently before putting it in the CFP.

## Current disagreement to resolve

There are two different workshop concepts:

### Review workshop

Participants review prepared changes, reveal their hidden work history, and reconsider their decisions.

### End-to-end Entire workshop

Participants host a repository, create agent work, create checkpoints, continue a Session, create their own Trail, satisfy its rules, and merge.

The desired direction expressed in this session is the second one.

The strongest way to describe the difference is:

> The review workshop changes how participants think about review. The end-to-end workshop changes how participants actually build with agents.

Or:

> Review should be one step in the journey, not the whole workshop.

## Open questions

1. Is the workshop 120 minutes or 180 minutes? Both lengths were discussed.
2. Will participants use their own repositories or personal copies of one starter repository?
3. Does the current Trail experience expose a numeric passing rate, or should the contest use passed Gates and other visible measures?
4. What exact task will every participant complete?
5. How will Search have useful history in a newly mirrored repository?
6. Will participants work alone, in pairs, or both?
7. How many workshop helpers will be available for 80 people?
8. Which coding agents will be supported during the event?
9. Will every participant be expected to merge, or is a merge-ready Trail the finish line?
10. Which competition matters most: speed, token efficiency, handoff quality, evidence, or an overall score?

## Recommended next step

Choose the competition mechanic before writing another CFP abstract.

The mechanic determines the title, the starter repository, the required product steps, the scoring, and the final participant outcome.
