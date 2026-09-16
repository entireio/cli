# entire-graph — instructions for coding agents (follow directly)

You have a deterministic local code graph: `entire graph` (functions, classes, methods,
types, routes + call/inheritance relations; no network). These instructions are FOR YOU, the
agent reading this file. Use the graph to narrow exploration without trading away correctness.

## The workflow (mandatory for locate/fix/change tasks)

Your FIRST action on any task that requires finding code must be ONE search:

    entire graph search --repo . --profile full --query "<the task or bug in one sentence>"

Then open the top hit's file with your file-read tool (pass a line range around the reported
line), inspect enough surrounding behavior to justify the change, and make the smallest complete
edit. Treat graph output as evidence, not an oracle.

## Hard rules

1. SEARCH FIRST — never grep/find/cat to locate code before you have searched.
2. READ focused source around the result. Widen the check when aliases, generated code, dynamic
   dispatch, or related implementations could matter.
3. Use graph follow-ups only when they answer a real question. For impact or callers, prefer:
       entire graph impact --repo . --symbol X
4. Make the smallest complete edit and check sibling sites or contracts when the task implies them.
5. VERIFY before stopping. Run the most focused relevant test, build, or reproduction available.
   If execution is unavailable, perform a bounded source-level verification and state the limit.
6. Prefer precise queries and line ranges, but never trade resolution for fewer turns.
7. Feature-detect before relying on semantic relations:
       entire graph capabilities --json

## When NOT to use the graph

If the task already names the exact file and it is small, just read it — the graph saves tokens
by eliminating exploration; when there is nothing to explore, skip it.

## Reference

    locate  ->  entire graph search --repo . --profile full --query "..."
    impact  ->  entire graph impact --repo . --symbol X   (one shot: callers, callees, type consumers, data flow, co-change, siblings)
    callers ->  entire graph neighbors --repo . --symbol X --relation CALLS --direction in
    change  ->  entire graph diff --base A --head B --json
    detect  ->  entire graph capabilities --json   (inventory-only languages have no relations)
    stats   ->  entire graph stats --repo .        (human-facing token-savings report; not part of your workflow — do not run it unless asked)
