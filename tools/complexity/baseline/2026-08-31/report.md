# Complexity baseline

Ranking tables exclude features marked norank in the mapping (test infrastructure, generated code).

Excluded (norank), still measured in the CSVs: agent:vogon (1121 prod LOC), test-infra (10210 prod LOC).

## By area

| name | area | prod LOC | test LOC | ratio | funcs | Σcognit | cog/100loc | max cog | >20 cog | cov% | uncov cog | commits 90d |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| command |  | 84641 | 103839 | 1.23 | 2920 | 14538 | 17.2 | 105 | 121 | 72.7 | 2545 | 3741 |
| capture |  | 47572 | 73965 | 1.55 | 1402 | 7378 | 15.5 | 83 | 74 | 80.6 | 374 | 2066 |
| agents |  | 22208 | 29957 | 1.35 | 882 | 3461 | 15.6 | 54 | 31 | 78.7 | 267 | 607 |
| infra |  | 9484 | 12051 | 1.27 | 395 | 1151 | 12.1 | 28 | 2 | 83.3 | 70 | 426 |
| platform |  | 4431 | 6429 | 1.45 | 126 | 741 | 16.7 | 49 | 9 | 76.1 | 50 | 134 |

## By feature

| name | area | prod LOC | test LOC | ratio | funcs | Σcognit | cog/100loc | max cog | >20 cog | cov% | uncov cog | commits 90d |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| capture:strategy | capture | 19322 | 34012 | 1.76 | 502 | 3022 | 15.6 | 62 | 34 | 77.3 | 227 | 813 |
| capture:checkpoint-store | capture | 12494 | 18028 | 1.44 | 420 | 1854 | 14.8 | 48 | 18 | 80.2 | 81 | 608 |
| cmd:trail | command | 8221 | 7270 | 0.88 | 343 | 1697 | 20.6 | 52 | 11 | 64.8 | 468 | 339 |
| cmd:review | command | 9452 | 11897 | 1.26 | 343 | 1663 | 17.6 | 47 | 14 | 69.7 | 340 | 428 |
| cmd:control-plane | command | 5796 | 5331 | 0.92 | 205 | 1033 | 17.8 | 56 | 9 | 65.4 | 411 | 407 |
| cmd:explain | command | 5694 | 10555 | 1.85 | 154 | 1028 | 18.1 | 45 | 12 | 80.5 | 75 | 202 |
| cmd:session | command | 5506 | 10352 | 1.88 | 176 | 1018 | 18.5 | 44 | 8 | 76.5 | 98 | 324 |
| cmd:enable-disable-configure | command | 4328 | 7016 | 1.62 | 134 | 886 | 20.5 | 61 | 13 | 72.8 | 82 | 158 |
| cmd:auth-login | command | 7169 | 10033 | 1.40 | 288 | 840 | 11.7 | 27 | 3 | 85.2 | 27 | 512 |
| cmd:search | command | 5099 | 5308 | 1.04 | 148 | 839 | 16.5 | 105 | 8 | 73.5 | 117 | 261 |
| cmd:investigate | command | 5764 | 5721 | 0.99 | 170 | 803 | 13.9 | 47 | 6 | 75.4 | 92 | 23 |
| capture:transcript | capture | 3393 | 3226 | 0.95 | 108 | 797 | 23.5 | 55 | 10 | 88.0 | 6 | 28 |
| capture:hooks-lifecycle | capture | 5447 | 9284 | 1.70 | 145 | 778 | 14.3 | 83 | 5 | 79.8 | 46 | 294 |
| cmd:plugin | command | 4399 | 3957 | 0.90 | 130 | 725 | 16.5 | 34 | 7 | 65.9 | 192 | 148 |
| agent:claudecode | agents | 3346 | 5164 | 1.54 | 109 | 630 | 18.8 | 49 | 7 | 85.5 | 1 | 104 |
| agent:codex | agents | 3354 | 3667 | 1.09 | 136 | 627 | 18.7 | 46 | 7 | 82.8 | 16 | 160 |
| platform:git-remote-entire | platform | 3258 | 5220 | 1.60 | 87 | 600 | 18.4 | 49 | 7 | 75.0 | 47 | 90 |
| capture:redaction | capture | 2945 | 4765 | 1.62 | 108 | 507 | 17.2 | 33 | 4 | 89.6 | 1 | 92 |
| cmd:doctor | command | 2307 | 2950 | 1.28 | 66 | 478 | 20.7 | 52 | 5 | 70.0 | 129 | 122 |
| cmd:dispatch | command | 2771 | 4627 | 1.67 | 99 | 474 | 17.1 | 48 | 4 | 75.0 | 53 | 54 |
| agent:framework | agents | 4675 | 5184 | 1.11 | 201 | 422 | 9.0 | 24 | 1 | 74.4 | 62 | 144 |
| infra:settings | infra | 2955 | 3184 | 1.08 | 124 | 411 | 13.9 | 28 | 1 | 83.2 | 21 | 122 |
| agent:factoryaidroid | agents | 1888 | 2932 | 1.55 | 74 | 372 | 19.7 | 49 | 4 | 86.0 | 3 | 31 |
| cmd:import | command | 2219 | 3502 | 1.58 | 76 | 335 | 15.1 | 42 | 2 | 80.2 | 20 | 178 |
| cmd:recap | command | 1886 | 1343 | 0.71 | 80 | 292 | 15.5 | 22 | 1 | 76.2 | 45 | 15 |
| infra:git-layer | infra | 2395 | 3503 | 1.46 | 109 | 290 | 12.1 | 23 | 1 | 86.0 | 16 | 88 |
| cmd:blame-why | command | 1451 | 1190 | 0.82 | 60 | 288 | 19.8 | 53 | 2 | 79.5 | 24 | 42 |
| cmd:checkpoint | command | 1760 | 1142 | 0.65 | 67 | 284 | 16.1 | 32 | 1 | 79.5 | 30 | 79 |
| cmd:activity | command | 1513 | 1087 | 0.72 | 47 | 281 | 18.6 | 54 | 3 | 65.1 | 55 | 20 |
| agent:geminicli | agents | 1710 | 3037 | 1.78 | 61 | 273 | 16.0 | 33 | 4 | 86.5 | 5 | 19 |
| cmd:experts | command | 1353 | 1035 | 0.76 | 61 | 268 | 19.8 | 54 | 3 | 81.5 | 2 | 38 |
| agent:pi | agents | 1581 | 1846 | 1.17 | 65 | 262 | 16.6 | 54 | 2 | 79.4 | 8 | 51 |
| infra:observability | infra | 2337 | 3398 | 1.45 | 91 | 257 | 11.0 | 16 | 0 | 83.0 | 19 | 139 |
| cmd:status | command | 1276 | 2850 | 2.23 | 41 | 250 | 19.6 | 53 | 2 | 84.2 | 5 | 56 |
| capture:cell-routing | capture | 2288 | 2897 | 1.27 | 65 | 236 | 10.3 | 25 | 1 | 85.4 | 13 | 171 |
| agent:copilotcli | agents | 1475 | 2892 | 1.96 | 61 | 235 | 15.9 | 32 | 2 | 87.9 | 0 | 24 |
| agent:cursor | agents | 1602 | 2863 | 1.79 | 59 | 235 | 14.7 | 27 | 2 | 84.5 | 0 | 29 |
| cmd:runner | command | 1186 | 508 | 0.43 | 39 | 223 | 18.8 | 42 | 3 | 35.4 | 144 | 15 |
| agent:opencode | agents | 1456 | 2236 | 1.54 | 61 | 220 | 15.1 | 19 | 0 | 76.2 | 14 | 33 |
| capture:session-state | capture | 1683 | 1753 | 1.04 | 54 | 184 | 10.9 | 35 | 2 | 90.4 | 0 | 60 |
| cmd:checkpoint-policy | command | 1052 | 1211 | 1.15 | 51 | 166 | 15.8 | 17 | 0 | 83.5 | 0 | 109 |
| cmd:labs-version-shell | command | 1346 | 1631 | 1.21 | 45 | 151 | 11.2 | 24 | 2 | 66.1 | 43 | 104 |
| cmd:agent-help-mcp | command | 1105 | 1624 | 1.47 | 32 | 146 | 13.2 | 30 | 1 | 89.9 | 1 | 50 |
| platform:coreapi-handwritten | platform | 1173 | 1209 | 1.03 | 39 | 141 | 12.0 | 23 | 2 | 81.2 | 3 | 44 |
| infra:process-layer | infra | 944 | 783 | 0.83 | 37 | 121 | 12.8 | 14 | 0 | 72.5 | 4 | 37 |
| cmd:clean | command | 533 | 867 | 1.63 | 14 | 112 | 21.0 | 24 | 1 | 57.0 | 48 | 9 |
| cmd:api-passthrough | command | 471 | 251 | 0.53 | 16 | 105 | 22.3 | 16 | 0 | 54.3 | 44 | 17 |
| cmd:agent | command | 580 | 257 | 0.44 | 20 | 86 | 14.8 | 17 | 0 | 85.9 | 0 | 8 |
| infra:ui-utils | infra | 853 | 1183 | 1.39 | 34 | 72 | 8.4 | 9 | 0 | 85.7 | 10 | 40 |
| cmd:tokens | command | 404 | 324 | 0.80 | 15 | 67 | 16.6 | 13 | 0 | 85.5 | 0 | 23 |

## By package (top 40 by cognitive complexity)

| name | area | prod LOC | test LOC | ratio | funcs | Σcognit | cog/100loc | max cog | >20 cog | cov% | uncov cog | commits 90d | in | out |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| cmd/entire/cli |  | 63943 | 81907 | 1.28 | 2122 | 11394 | 17.8 | 105 | 102 | 71.7 | 2140 | 3079 | 1 | 66 |
| cmd/entire/cli/strategy |  | 19322 | 34012 | 1.76 | 502 | 3022 | 15.6 | 62 | 34 | 77.3 | 227 | 813 | 4 | 35 |
| cmd/entire/cli/checkpoint |  | 9167 | 14590 | 1.59 | 304 | 1463 | 16.0 | 48 | 15 | 78.5 | 79 | 423 | 9 | 19 |
| cmd/entire/cli/review |  | 7872 | 10511 | 1.34 | 286 | 1409 | 17.9 | 47 | 13 | 68.6 | 309 | 355 | 5 | 18 |
| cmd/entire/cli/investigate |  | 5039 | 5177 | 1.03 | 145 | 654 | 13.0 | 47 | 4 | 71.5 | 92 | 18 | 1 | 17 |
| cmd/entire/cli/agent/claudecode |  | 3346 | 5164 | 1.54 | 109 | 630 | 18.8 | 49 | 7 | 85.5 | 1 | 104 | 5 | 12 |
| cmd/entire/cli/agent/codex |  | 3354 | 3667 | 1.09 | 136 | 627 | 18.7 | 46 | 7 | 82.8 | 16 | 160 | 5 | 11 |
| cmd/entire/cli/transcript/compact |  | 2463 | 1642 | 0.67 | 74 | 600 | 24.4 | 55 | 7 | 87.1 | 5 | 5 | 3 | 4 |
| redact |  | 2945 | 4765 | 1.62 | 108 | 507 | 17.2 | 33 | 4 | 89.6 | 1 | 92 | 10 | 0 |
| e2e/agents |  | 3026 | 648 | 0.21 | 173 | 463 | 15.3 | 22 | 2 | 13.4 | 353 | 38 | 2 | 1 |
| cmd/entire/cli/agent/factoryaidroid |  | 1888 | 2932 | 1.55 | 74 | 372 | 19.7 | 49 | 4 | 86.0 | 3 | 31 | 4 | 7 |
| cmd/entire/cli/integration_test |  | 3993 | 26314 | 6.59 | 191 | 358 | 9.0 | 18 | 0 | 84.0 | 1 | 285 | 0 | 11 |
| cmd/entire/cli/settings |  | 2388 | 2832 | 1.19 | 97 | 330 | 13.8 | 20 | 0 | 83.6 | 20 | 95 | 10 | 7 |
| cmd/entire/cli/dispatch |  | 1724 | 2871 | 1.67 | 52 | 310 | 18.0 | 48 | 2 | 84.6 | 3 | 44 | 1 | 13 |
| cmd/entire/cli/checkpoint/remote |  | 1822 | 2643 | 1.45 | 67 | 300 | 16.5 | 48 | 3 | 88.9 | 0 | 107 | 3 | 5 |
| e2e/testutil |  | 1666 | 0 | 0.00 | 80 | 274 | 16.4 | 32 | 2 | 0.0 | 274 | 14 | 0 | 15 |
| cmd/entire/cli/agent/geminicli |  | 1710 | 3037 | 1.78 | 61 | 273 | 16.0 | 33 | 4 | 86.5 | 5 | 19 | 5 | 8 |
| cmd/entire/cli/agentimport |  | 1635 | 2590 | 1.58 | 62 | 258 | 15.8 | 42 | 2 | 85.6 | 10 | 124 | 1 | 17 |
| internal/remotehelper/githelper |  | 1107 | 1792 | 1.62 | 14 | 256 | 23.1 | 49 | 5 | 69.0 | 24 | 13 | 1 | 2 |
| cmd/entire/cli/auth |  | 2196 | 2921 | 1.33 | 89 | 249 | 11.3 | 18 | 0 | 84.7 | 0 | 165 | 4 | 7 |
| cmd/entire/cli/agent |  | 2861 | 3398 | 1.19 | 94 | 237 | 8.3 | 19 | 0 | 87.5 | 16 | 110 | 22 | 5 |
| cmd/entire/cli/agent/copilotcli |  | 1475 | 2892 | 1.96 | 61 | 235 | 15.9 | 32 | 2 | 87.9 | 0 | 24 | 3 | 5 |
| cmd/entire/cli/agent/cursor |  | 1602 | 2863 | 1.79 | 59 | 235 | 14.7 | 27 | 2 | 84.5 | 0 | 29 | 3 | 6 |
| cmd/entire/cli/agent/pi |  | 1362 | 1667 | 1.22 | 59 | 225 | 16.5 | 54 | 2 | 76.9 | 8 | 51 | 2 | 8 |
| cmd/entire/cli/agent/opencode |  | 1456 | 2236 | 1.54 | 61 | 220 | 15.1 | 19 | 0 | 76.2 | 14 | 33 | 4 | 6 |
| cmd/entire/cli/session |  | 1683 | 1753 | 1.04 | 54 | 184 | 10.9 | 35 | 2 | 90.4 | 0 | 60 | 7 | 9 |
| cmd/entire/cli/recap |  | 1112 | 599 | 0.54 | 49 | 176 | 15.8 | 22 | 1 | 87.0 | 5 | 5 | 1 | 3 |
| cmd/entire/cli/summarize |  | 827 | 1678 | 2.03 | 22 | 168 | 20.3 | 36 | 1 | 89.7 | 0 | 7 | 2 | 10 |
| cmd/entire/cli/gitrepo |  | 1170 | 1515 | 1.29 | 58 | 152 | 13.0 | 23 | 1 | 83.1 | 6 | 39 | 10 | 2 |
| cmd/entire/cli/investigate/flowchart |  | 677 | 409 | 0.60 | 23 | 148 | 21.9 | 40 | 2 | 97.3 | 0 | 4 | 1 | 0 |
| e2e/vogon |  | 715 | 62 | 0.09 | 27 | 139 | 19.4 | 44 | 2 | 1.1 | 139 | 5 | 0 | 1 |
| internal/remotehelper/gitproto |  | 495 | 606 | 1.22 | 17 | 137 | 27.7 | 30 | 1 | 87.0 | 0 | 0 | 1 | 0 |
| cmd/entire/cli/agent/external |  | 1242 | 1711 | 1.38 | 86 | 136 | 11.0 | 24 | 1 | 57.7 | 37 | 21 | 4 | 6 |
| cmd/entire/cli/search |  | 871 | 1170 | 1.34 | 29 | 136 | 15.6 | 15 | 0 | 92.9 | 0 | 64 | 2 | 2 |
| cmd/entire/cli/checkpointpolicy |  | 745 | 883 | 1.19 | 36 | 127 | 17.0 | 17 | 0 | 83.7 | 0 | 67 | 2 | 5 |
| internal/remotehelper/transport |  | 757 | 1833 | 2.42 | 26 | 118 | 15.6 | 39 | 1 | 93.3 | 0 | 16 | 1 | 5 |
| internal/coreapi |  | 962 | 1209 | 1.26 | 34 | 98 | 10.2 | 23 | 1 | 81.2 | 3 | 124 | 1 | 4 |
| e2e/cmd/testreport |  | 332 | 0 | 0.00 | 9 | 94 | 28.3 | 48 | 1 | 0.0 | 94 | 0 | 0 | 1 |
| cmd/entire/cli/api |  | 1270 | 1454 | 1.14 | 38 | 92 | 7.2 | 9 | 0 | 81.6 | 7 | 101 | 8 | 2 |
| cmd/entire/cli/versioncheck |  | 693 | 1241 | 1.79 | 26 | 83 | 12.0 | 16 | 0 | 80.9 | 7 | 50 | 2 | 5 |

## Top functions by cognitive complexity

| cognit | cyclo | loc | cov% | feature | function |
|---:|---:|---:|---:|---|---|
| 105 | 51 | 318 | 61.0 | cmd:search | `cmd/entire/cli/search_cmd.go:27` newSearchCmd |
| 83 | 56 | 374 | 81.3 | capture:hooks-lifecycle | `cmd/entire/cli/lifecycle.go:686` handleLifecycleTurnEnd |
| 62 | 31 | 176 | 55.8 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_pending.go:316` ManualCommitStrategy.RestoreLogsOnly |
| 61 | 40 | 139 | 84.1 | cmd:enable-disable-configure | `cmd/entire/cli/setup.go:558` applyAgentChanges |
| 56 | 17 | 110 | 85.7 | cmd:control-plane | `cmd/entire/cli/repo.go:183` newRepoListCmd |
| 55 | 22 | 70 | 86.1 | capture:transcript | `cmd/entire/cli/transcript/compact/parse.go:46` BuildCondensedEntries |
| 54 | 23 | 116 | 95.7 | cmd:activity | `cmd/entire/cli/activity_render.go:418` renderCommitListN |
| 54 | 24 | 83 | 81.2 | agent:pi | `cmd/entire/cli/agent/pi/reviewer.go:44` parsePiReviewOutput |
| 54 | 40 | 153 | 76.8 | cmd:experts | `cmd/entire/cli/experts_cmd.go:214` runExperts |
| 53 | 26 | 86 | 75.0 | cmd:blame-why | `cmd/entire/cli/attribution.go:1104` renderAttributionLineWhy |
| 53 | 32 | 167 | 91.5 | cmd:status | `cmd/entire/cli/status.go:469` writeActiveSessions |
| 53 | 27 | 122 | 85.4 | capture:transcript | `cmd/entire/cli/transcript/compact/codex.go:73` compactCodex |
| 52 | 27 | 161 | 65.8 | cmd:doctor | `cmd/entire/cli/doctor.go:116` runSessionsFix |
| 52 | 13 | 68 | 14.6 | cmd:control-plane | `cmd/entire/cli/project.go:94` newProjectListCmd |
| 52 | 26 | 146 | 35.8 | cmd:control-plane | `cmd/entire/cli/repo_mirror_use.go:402` newRepoMirrorUseCmd |
| 52 | 24 | 117 | 81.0 | cmd:status | `cmd/entire/cli/status.go:852` runStatusJSON |
| 52 | 28 | 143 | 78.9 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_opf_refs.go:35` RewriteQueuedCheckpointRefsWithOPF |
| 52 | 25 | 76 | 78.0 | cmd:trail | `cmd/entire/cli/trail_cmd.go:564` listTrailResources |
| 49 | 23 | 127 | 90.5 | agent:claudecode | `cmd/entire/cli/agent/claudecode/hooks.go:324` ClaudeCodeAgent.UninstallHooks |
| 49 | 23 | 125 | 90.5 | agent:factoryaidroid | `cmd/entire/cli/agent/factoryaidroid/hooks.go:273` FactoryAIDroidAgent.UninstallHooks |
| 49 | 28 | 161 | 88.5 | capture:strategy | `cmd/entire/cli/strategy/common.go:640` EnsurePrimaryRef |
| 49 | 27 | 117 | 81.7 | platform:git-remote-entire | `internal/remotehelper/githelper/list.go:21` handleList |
| 48 | 26 | 134 | 85.3 | capture:checkpoint-store | `cmd/entire/cli/checkpoint/persistent.go:334` treeWriter.applyTranscriptBackfill |
| 48 | 27 | 115 | 85.0 | capture:checkpoint-store | `cmd/entire/cli/checkpoint/remote/util.go:63` fetchURLAuthoritative |
| 48 | 32 | 148 | 76.0 | cmd:dispatch | `cmd/entire/cli/dispatch/mode_local.go:181` enumerateRepoCandidates |
| 48 | 21 | 185 | 70.2 | cmd:enable-disable-configure | `cmd/entire/cli/setup.go:788` newEnableCmd |
| 47 | 32 | 168 | 53.2 | cmd:investigate | `cmd/entire/cli/investigate/cmd.go:594` runFresh |
| 47 | 30 | 108 | 86.2 | cmd:review | `cmd/entire/cli/review/cmd.go:662` buildConfiguredProfile |
| 47 | 28 | 181 | 38.6 | cmd:review | `cmd/entire/cli/review/cmd.go:792` runReview |
| 47 | 28 | 152 | 84.3 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_opf_rewrite.go:260` RewriteUnpushedV1WithOPF |
| 46 | 30 | 149 | 80.5 | cmd:activity | `cmd/entire/cli/activity_render.go:181` renderDotChart |
| 46 | 26 | 151 | 96.5 | agent:codex | `cmd/entire/cli/agent/codex/reviewer.go:128` parseCodexOutputBuf |
| 45 | 26 | 97 | 95.1 | cmd:experts | `cmd/entire/cli/experts_tui.go:371` expertsTUIModel.renderDetail |
| 45 | 28 | 142 | 70.8 | cmd:explain | `cmd/entire/cli/explain.go:1640` explainTemporaryCheckpoint |
| 44 | 17 | 81 | 97.8 | agent:claudecode | `cmd/entire/cli/agent/claudecode/reviewer.go:92` parseClaudeOutputBuf |
| 44 | 19 | 59 | 96.4 | cmd:session | `cmd/entire/cli/attach_transcript.go:24` extractTranscriptMetadata |
| 44 | 29 | 178 | 79.3 | cmd:explain | `cmd/entire/cli/explain.go:693` runExplainCheckpointWithLookup |
| 44 | 20 | 98 | 81.5 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_opf_rewrite.go:691` rebuildTreeWithCachedRedaction |
| 43 | 33 | 202 | 91.0 | agent:factoryaidroid | `cmd/entire/cli/agent/factoryaidroid/hooks.go:46` FactoryAIDroidAgent.InstallHooks |
| 43 | 28 | 197 | 88.5 | cmd:review | `cmd/entire/cli/review/cmd.go:94` NewCommand |

## Complex and under-covered (cognit ≥ 20, coverage < 50%)

| cognit | cyclo | loc | cov% | feature | function |
|---:|---:|---:|---:|---|---|
| 52 | 13 | 68 | 14.6 | cmd:control-plane | `cmd/entire/cli/project.go:94` newProjectListCmd |
| 52 | 26 | 146 | 35.8 | cmd:control-plane | `cmd/entire/cli/repo_mirror_use.go:402` newRepoMirrorUseCmd |
| 47 | 28 | 181 | 38.6 | cmd:review | `cmd/entire/cli/review/cmd.go:792` runReview |
| 42 | 18 | 80 | 0.0 | cmd:runner | `cmd/entire/cli/runner_gather.go:278` gatherTrails |
| 38 | 25 | 175 | 0.0 | cmd:review | `cmd/entire/cli/review/picker.go:753` RunReviewProfileConfigPicker |
| 37 | 23 | 143 | 49.0 | capture:strategy | `cmd/entire/cli/strategy/common.go:437` EnsureRedactionConfigured |
| 34 | 15 | 117 | 40.4 | cmd:session | `cmd/entire/cli/resume.go:746` checkRemoteMetadata |
| 33 | 24 | 202 | 33.0 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_hooks.go:358` ManualCommitStrategy.PrepareCommitMsg |
| 32 | 17 | 72 | 0.0 | cmd:trail | `cmd/entire/cli/trail_resume_cmd.go:203` runTrailResume |
| 31 | 17 | 76 | 33.3 | capture:checkpoint-store | `cmd/entire/cli/checkpoint/persistent.go:2739` readTranscriptFromTree |
| 31 | 17 | 110 | 21.2 | cmd:doctor | `cmd/entire/cli/doctor_migrate.go:16` newDoctorMigrateCheckpointsCmd |
| 31 | 18 | 66 | 0.0 | cmd:review | `cmd/entire/cli/review/picker.go:337` pickSlotList |
| 29 | 22 | 108 | 8.2 | cmd:search | `cmd/entire/cli/search_v4.go:97` semanticSearchV4Session.search |
| 28 | 13 | 53 | 0.0 | cmd:trail | `cmd/entire/cli/trail_cmd.go:1875` runTrailDelete |
| 26 | 14 | 61 | 10.5 | cmd:plugin | `cmd/entire/cli/plugin_group.go:640` newPluginInfoCmd |
| 25 | 11 | 36 | 47.6 | cmd:doctor | `cmd/entire/cli/doctor_logs.go:114` followFile |
| 25 | 18 | 100 | 16.7 | cmd:search | `cmd/entire/cli/search_cmd.go:511` searchAllCells |
| 24 | 20 | 93 | 0.0 | cmd:plugin | `cmd/entire/cli/plugin_group.go:203` runRemoteInstall |
| 23 | 11 | 58 | 12.9 | cmd:plugin | `cmd/entire/cli/plugin_group.go:531` newPluginUpgradeCmd |
| 23 | 13 | 90 | 14.3 | cmd:control-plane | `cmd/entire/cli/repo_mirror.go:470` newRepoMirrorCreateCmd |
| 23 | 20 | 94 | 0.0 | cmd:runner | `cmd/entire/cli/runner_setup.go:176` applyTuneWithAgent |
| 23 | 21 | 73 | 0.0 | cmd:trail | `cmd/entire/cli/trail_watch_cmd.go:95` runTrailWatchResolved |
| 22 | 11 | 103 | 44.7 | cmd:control-plane | `cmd/entire/cli/repo_clone.go:77` newRepoCloneCmd |
| 22 | 10 | 45 | 15.4 | cmd:trail | `cmd/entire/cli/trail_comment_cmd.go:354` newTrailCommentDeleteCmd |
| 21 | 16 | 61 | 39.5 | cmd:activity | `cmd/entire/cli/activity_tui.go:111` activityModel.Update |
| 21 | 11 | 75 | 44.1 | cmd:explain | `cmd/entire/cli/explain_export.go:247` matchCheckpointPrefixWithRemoteFallback |
| 21 | 14 | 47 | 0.0 | cmd:runner | `cmd/entire/cli/runner_gather.go:106` gatherRepoStatics |
| 20 | 11 | 72 | 20.0 | cmd:plugin | `cmd/entire/cli/plugin_group.go:702` newPluginBrowseCmd |
| 20 | 15 | 47 | 0.0 | cmd:runner | `cmd/entire/cli/runner_gather.go:230` gatherCheckpoints |
| 20 | 16 | 93 | 0.0 | capture:strategy | `cmd/entire/cli/strategy/cleanup.go:309` DeleteOrphanedCheckpoints |

## File hotspots: commits(90d) × cognitive complexity (top 40)

| commits | sum cognit | loc | cov% | feature | file |
|---:|---:|---:|---:|---|---|
| 93 | 624 | 2924 | 74.5 | cmd:enable-disable-configure | `cmd/entire/cli/setup.go` |
| 85 | 642 | 3370 | 80.3 | cmd:explain | `cmd/entire/cli/explain.go` |
| 82 | 533 | 2459 | 73.1 | cmd:trail | `cmd/entire/cli/trail_cmd.go` |
| 84 | 516 | 3353 | 70.0 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_hooks.go` |
| 63 | 572 | 2958 | 78.2 | capture:checkpoint-store | `cmd/entire/cli/checkpoint/persistent.go` |
| 75 | 361 | 2227 | 78.0 | capture:hooks-lifecycle | `cmd/entire/cli/lifecycle.go` |
| 63 | 350 | 1752 | 65.3 | cmd:review | `cmd/entire/cli/review/cmd.go` |
| 66 | 294 | 1964 | 78.9 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_condensation.go` |
| 61 | 302 | 1781 | 69.8 | capture:strategy | `cmd/entire/cli/strategy/common.go` |
| 78 | 230 | 1347 | 82.1 | cmd:control-plane | `cmd/entire/cli/repo_mirror.go` |
| 52 | 265 | 1176 | 71.0 | cmd:search | `cmd/entire/cli/search_cmd.go` |
| 51 | 265 | 1798 | 80.9 | infra:settings | `cmd/entire/cli/settings/settings.go` |
| 26 | 382 | 1758 | 61.8 | cmd:trail | `cmd/entire/cli/trail_review_cmd.go` |
| 34 | 266 | 1865 | 74.8 | cmd:search | `cmd/entire/cli/search_tui.go` |
| 42 | 212 | 1122 | 67.3 | cmd:session | `cmd/entire/cli/resume.go` |
| 32 | 270 | 1347 | 31.9 | cmd:review | `cmd/entire/cli/review/picker.go` |
| 46 | 180 | 982 | 78.2 | cmd:session | `cmd/entire/cli/attach.go` |
| 47 | 166 | 893 | 69.2 | cmd:doctor | `cmd/entire/cli/doctor.go` |
| 26 | 288 | 1451 | 79.5 | cmd:blame-why | `cmd/entire/cli/attribution.go` |
| 40 | 144 | 1002 | 85.4 | cmd:auth-login | `cmd/entire/cli/login.go` |
| 24 | 212 | 983 | 86.2 | cmd:status | `cmd/entire/cli/status.go` |
| 20 | 238 | 1182 | 56.5 | cmd:trail | `cmd/entire/cli/trail_resume_cmd.go` |
| 25 | 186 | 760 | 82.8 | agent:codex | `cmd/entire/cli/agent/codex/hooks.go` |
| 44 | 103 | 1185 | 90.5 | capture:session-state | `cmd/entire/cli/session/state.go` |
| 53 | 85 | 571 | 73.6 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_push.go` |
| 31 | 132 | 838 | 92.6 | cmd:search | `cmd/entire/cli/search/search.go` |
| 34 | 118 | 827 | 82.5 | capture:hooks-lifecycle | `cmd/entire/cli/git_operations.go` |
| 32 | 114 | 607 | 81.4 | cmd:session | `cmd/entire/cli/session_adopt.go` |
| 11 | 316 | 1417 | 73.8 | capture:checkpoint-store | `cmd/entire/cli/checkpoint/ephemeral.go` |
| 32 | 104 | 668 | 84.2 | cmd:auth-login | `cmd/entire/cli/auth.go` |
| 17 | 189 | 798 | 86.1 | cmd:review | `cmd/entire/cli/review/manifest.go` |
| 20 | 160 | 736 | 83.4 | cmd:experts | `cmd/entire/cli/experts_cmd.go` |
| 23 | 131 | 753 | 55.3 | cmd:search | `cmd/entire/cli/search_v4.go` |
| 27 | 110 | 443 | 78.1 | cmd:control-plane | `cmd/entire/cli/repo.go` |
| 20 | 145 | 673 | 87.8 | cmd:checkpoint | `cmd/entire/cli/checkpoint_tokens.go` |
| 20 | 143 | 791 | 31.8 | cmd:control-plane | `cmd/entire/cli/repo_mirror_create_wizard.go` |
| 21 | 136 | 585 | 69.5 | cmd:review | `cmd/entire/cli/review/profile.go` |
| 18 | 155 | 657 | 85.0 | cmd:dispatch | `cmd/entire/cli/dispatch/mode_local.go` |
| 24 | 115 | 675 | 85.8 | capture:strategy | `cmd/entire/cli/strategy/manual_commit_session.go` |
| 15 | 179 | 824 | 19.9 | cmd:plugin | `cmd/entire/cli/plugin_group.go` |
