//go:build hookperf

package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// perSessionProfile holds detailed sub-step timings for a single session.
type perSessionProfile struct {
	sessionID  string
	phase      session.Phase
	condensed  bool
	skippedWhy string // why condensation was skipped (e.g., "no_new_content", "no_overlap", "no_files")

	// Pre-transition timings (already measured in outer loop)
	shadowResolve time.Duration
	hasNewContent time.Duration

	// TransitionAndLog decomposition
	stateMachine time.Duration // Transition() pure function
	overlapCheck time.Duration // shouldCondenseWithOverlapCheck
	prepCondense time.Duration // PrepareCondensation total
	stateUpdate  time.Duration // updateBaseCommitIfChanged (non-condensed path)

	// PrepareCondensation sub-steps (only for condensed sessions)
	pcShadowRef   time.Duration // shadow branch ref resolution inside PrepareCondensation
	pcExtractData time.Duration // extractSessionData / extractSessionDataFromLiveTranscript
	pcFilterFiles time.Duration // file filtering (intersection with committed files)
	pcAttribution time.Duration // calculateSessionAttributions
	pcSummary     time.Duration // summary generation
	pcBuildResult time.Duration // building the result struct
}

// TestPostCommitProfile breaks down PostCommit into timed sub-steps to identify
// where time is spent. Manually decomposes TransitionAndLog into overlap check
// and PrepareCondensation sub-steps.
//
// Run: go test -v -run TestPostCommitProfile -tags hookperf -timeout 15m ./cmd/entire/cli/strategy/
func TestPostCommitProfile(t *testing.T) {
	cacheDir := cloneSourceRepo(t)

	for _, sc := range []struct {
		name   string
		ended  int
		idle   int
		active int
	}{
		{"200sessions", 176, 22, 2},
	} {
		t.Run(sc.name, func(t *testing.T) {
			totalSessions := sc.ended + sc.idle + sc.active

			dir := localClone(t, cacheDir)
			t.Chdir(dir)
			paths.ClearWorktreeRootCache()
			session.ClearGitCommonDirCache()

			seedBranches(t, dir, 200)
			gitRun(t, dir, "pack-refs", "--all")

			// Control commit
			timeControlCommit(t, dir)
			gitRun(t, dir, "reset", "HEAD~1")
			gitRun(t, dir, "add", "perf_control.txt")

			// Setup Entire
			createHookPerfSettings(t, dir)
			baseCommits := collectBaseCommits(t, dir, totalSessions)
			seedHookPerfSessions(t, dir, baseCommits, sc.ended, sc.idle, sc.active)

			t.Setenv("ENTIRE_TEST_TTY", "1")
			paths.ClearWorktreeRootCache()
			session.ClearGitCommonDirCache()

			// PrepareCommitMsg
			commitMsgFile := filepath.Join(dir, ".git", "COMMIT_EDITMSG")
			if err := os.WriteFile(commitMsgFile, []byte("implement feature\n"), 0o644); err != nil {
				t.Fatalf("write commit msg: %v", err)
			}

			s := &ManualCommitStrategy{}
			if err := s.PrepareCommitMsg(context.Background(), commitMsgFile, "message"); err != nil {
				t.Fatalf("PrepareCommitMsg: %v", err)
			}

			msgBytes, err := os.ReadFile(commitMsgFile) //nolint:gosec // test file
			if err != nil {
				t.Fatalf("read commit msg: %v", err)
			}
			commitMsg := string(msgBytes)
			if _, found := trailers.ParseCheckpoint(commitMsg); !found {
				cpID, genErr := id.Generate()
				if genErr != nil {
					t.Fatalf("generate checkpoint ID: %v", genErr)
				}
				commitMsg = fmt.Sprintf("%s\n%s: %s\n",
					strings.TrimRight(commitMsg, "\n"),
					trailers.CheckpointTrailerKey, cpID)
			}

			gitRun(t, dir, "commit", "-m", commitMsg)

			// Now profile PostCommit step by step
			paths.ClearWorktreeRootCache()
			session.ClearGitCommonDirCache()
			ctx := context.Background()

			// ── Step 1: Open repo + resolve HEAD ──
			t0 := time.Now()
			repo, err := OpenRepository(ctx)
			if err != nil {
				t.Fatalf("open repo: %v", err)
			}
			head, err := repo.Head()
			if err != nil {
				t.Fatalf("head: %v", err)
			}
			commitObj, err := repo.CommitObject(head.Hash())
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			checkpointID, found := trailers.ParseCheckpoint(commitObj.Message)
			if !found {
				t.Fatal("no checkpoint trailer")
			}
			tOpenRepo := time.Since(t0)

			// ── Step 2: Find sessions ──
			t1 := time.Now()
			worktreePath, err := paths.WorktreeRoot(ctx)
			if err != nil {
				t.Fatalf("worktree root: %v", err)
			}
			sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
			if err != nil || len(sessions) == 0 {
				t.Fatalf("find sessions: %v (count=%d)", err, len(sessions))
			}
			tFindSessions := time.Since(t1)

			// ── Step 3: Resolve HEAD tree + parent tree + committed files ──
			t2 := time.Now()
			var headTree *object.Tree
			if tr, tErr := commitObj.Tree(); tErr == nil {
				headTree = tr
			}
			var parentTree *object.Tree
			if commitObj.NumParents() > 0 {
				if parent, pErr := commitObj.Parent(0); pErr == nil {
					if tr, tErr := parent.Tree(); tErr == nil {
						parentTree = tr
					}
				}
			}
			committedFileSet := filesChangedInCommit(commitObj, headTree, parentTree)
			tResolveTreesFiles := time.Since(t2)

			// ── Step 4: Per-session profiling with decomposed TransitionAndLog ──
			newHead := head.Hash().String()
			shadowBranchesToDelete := make(map[string]struct{})

			// Precompute headTree hash index (same as real PostCommit does)
			headTreeHashes, _ := BuildTreeHashIndex(ctx, headTree)

			profiles := make([]perSessionProfile, 0, len(sessions))

			type processedWithProfile struct {
				ps      processedSession
				profile perSessionProfile
			}
			allProcessed := make([]processedWithProfile, 0, len(sessions))

			for _, state := range sessions {
				prof := perSessionProfile{
					sessionID: state.SessionID,
					phase:     state.Phase,
				}
				shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

				// Time: shadow ref + tree resolution
				ts0 := time.Now()
				var shadowRef *plumbing.Reference
				var shadowTree *object.Tree
				if ref, refErr := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true); refErr == nil {
					shadowRef = ref
					if sc, scErr := repo.CommitObject(ref.Hash()); scErr == nil {
						if st, stErr := sc.Tree(); stErr == nil {
							shadowTree = st
						}
					}
				}
				prof.shadowResolve = time.Since(ts0)

				// Time: sessionHasNewContent
				ts1 := time.Now()
				var hasNew bool
				if state.Phase.IsActive() {
					hasNew = true
				} else {
					var contentErr error
					hasNew, contentErr = s.sessionHasNewContent(ctx, repo, state, shadowTree)
					if contentErr != nil {
						hasNew = true
					}
				}
				prof.hasNewContent = time.Since(ts1)

				filesTouchedBefore := make([]string, len(state.FilesTouched))
				copy(filesTouchedBefore, state.FilesTouched)
				if len(filesTouchedBefore) == 0 && state.Phase.IsActive() && state.TranscriptPath != "" {
					filesTouchedBefore = s.extractFilesFromLiveTranscript(ctx, state)
				}

				// ── Decompose TransitionAndLog ──

				// 4a: State machine transition (pure function, should be ~0)
				tsm := time.Now()
				transitionCtx := session.TransitionContext{HasFilesTouched: len(state.FilesTouched) > 0}
				result := session.Transition(state.Phase, session.EventGitCommit, transitionCtx)
				prof.stateMachine = time.Since(tsm)

				// Determine if this session will condense based on the actions
				willCondense := false
				for _, action := range result.Actions {
					if action == session.ActionCondense {
						willCondense = true
						break
					}
					if action == session.ActionCondenseIfFilesTouched {
						willCondense = len(state.FilesTouched) > 0
						break
					}
				}

				var handler *postCommitActionHandler
				if willCondense {
					// 4b: Overlap check
					toc := time.Now()
					shouldCondense := overlapCheckForProfile(ctx, repo, shadowBranchName, commitObj, headTree, parentTree, shadowTree, hasNew, filesTouchedBefore, committedFileSet, state.Phase.IsActive())
					prof.overlapCheck = time.Since(toc)

					if shouldCondense {
						// 4c: PrepareCondensation — decomposed
						prof.condensed = true
						profilePrepareCondensation(ctx, s, repo, checkpointID, state, committedFileSet, shadowRef, headTree, headTreeHashes, &prof)
					} else {
						prof.skippedWhy = "no_overlap"
						tsu := time.Now()
						s.updateBaseCommitIfChanged(ctx, state, newHead)
						prof.stateUpdate = time.Since(tsu)
					}
				} else if !hasNew {
					prof.skippedWhy = "no_new_content"
					tsu := time.Now()
					s.updateBaseCommitIfChanged(ctx, state, newHead)
					prof.stateUpdate = time.Since(tsu)
				} else {
					prof.skippedWhy = "no_files"
					tsu := time.Now()
					s.updateBaseCommitIfChanged(ctx, state, newHead)
					prof.stateUpdate = time.Since(tsu)
				}

				// Build the handler for batch write compatibility
				handler = &postCommitActionHandler{
					s:                      s,
					ctx:                    ctx,
					repo:                   repo,
					checkpointID:           checkpointID,
					head:                   head,
					commit:                 commitObj,
					newHead:                newHead,
					shadowBranchName:       shadowBranchName,
					shadowBranchesToDelete: shadowBranchesToDelete,
					committedFileSet:       committedFileSet,
					hasNew:                 hasNew,
					filesTouchedBefore:     filesTouchedBefore,
					headTree:               headTree,
					parentTree:             parentTree,
					shadowRef:              shadowRef,
					shadowTree:             shadowTree,
					headTreeHashes:         headTreeHashes,
					condensed:              prof.condensed,
				}

				// If we condensed, we need to actually run it through the real path
				// for the batch write to work. We already timed the sub-steps above,
				// so now run the real PrepareCondensation to get the pending result.
				if prof.condensed {
					handler.prepareCondensation(state)
				}

				// Mark shadow branch for deletion if condensed
				if prof.condensed {
					shadowBranchesToDelete[shadowBranchName] = struct{}{}
				}

				profiles = append(profiles, prof)
				allProcessed = append(allProcessed, processedWithProfile{
					ps: processedSession{
						state:              state,
						handler:            handler,
						shadowBranchName:   shadowBranchName,
						filesTouchedBefore: filesTouchedBefore,
						shadowTree:         shadowTree,
					},
					profile: prof,
				})
			}

			// ── Step 5: Batch write ──
			processed := make([]processedSession, len(allProcessed))
			for i, p := range allProcessed {
				processed[i] = p.ps
			}

			t5 := time.Now()
			batchWriteSucceeded := s.postCommitBatchWrite(ctx, processed)
			tBatchWrite := time.Since(t5)

			// ── Step 6: Finalize all ──
			uncondensedActiveOnBranch := make(map[string]bool)
			t6 := time.Now()
			s.postCommitFinalizeAll(ctx, repo, processed, batchWriteSucceeded,
				checkpointID, newHead, commitObj, headTree, committedFileSet,
				shadowBranchesToDelete, uncondensedActiveOnBranch)
			tFinalize := time.Since(t6)

			// ── Step 7: Shadow branch cleanup ──
			t7 := time.Now()
			cleanedUp := 0
			for shadowBranchName := range shadowBranchesToDelete {
				if uncondensedActiveOnBranch[shadowBranchName] {
					continue
				}
				if err := deleteShadowBranch(ctx, repo, shadowBranchName); err == nil {
					cleanedUp++
				}
			}
			tCleanup := time.Since(t7)

			// ── Aggregate timings ──
			var (
				totalShadowResolve time.Duration
				totalHasNewContent time.Duration
				totalStateMachine  time.Duration
				totalOverlapCheck  time.Duration
				totalPrepCondense  time.Duration
				totalStateUpdate   time.Duration

				// PrepareCondensation sub-step totals
				totalPCShadowRef   time.Duration
				totalPCExtractData time.Duration
				totalPCFilterFiles time.Duration
				totalPCAttribution time.Duration
				totalPCSummary     time.Duration
				totalPCBuildResult time.Duration

				sessionsCondensed int
				sessionsSkipped   int
				batchSize         int
			)

			for _, p := range profiles {
				totalShadowResolve += p.shadowResolve
				totalHasNewContent += p.hasNewContent
				totalStateMachine += p.stateMachine
				totalOverlapCheck += p.overlapCheck
				totalStateUpdate += p.stateUpdate

				if p.condensed {
					sessionsCondensed++
					batchSize++
					totalPrepCondense += p.prepCondense
					totalPCShadowRef += p.pcShadowRef
					totalPCExtractData += p.pcExtractData
					totalPCFilterFiles += p.pcFilterFiles
					totalPCAttribution += p.pcAttribution
					totalPCSummary += p.pcSummary
					totalPCBuildResult += p.pcBuildResult
				} else {
					sessionsSkipped++
				}
			}

			totalPerSession := totalShadowResolve + totalHasNewContent + totalStateMachine +
				totalOverlapCheck + totalPrepCondense + totalStateUpdate
			totalPostCommit := tOpenRepo + tFindSessions + tResolveTreesFiles +
				totalPerSession + tBatchWrite + tFinalize + tCleanup

			// ── Print results ──
			t.Log("")
			t.Log("═══════════════════════════════════════════════════════")
			t.Log("  PostCommit DEEP PROFILE — TransitionAndLog breakdown")
			t.Log("═══════════════════════════════════════════════════════")
			t.Logf("Total sessions:      %d", len(sessions))
			t.Logf("Sessions condensed:  %d", sessionsCondensed)
			t.Logf("Sessions skipped:    %d", sessionsSkipped)
			t.Logf("Batch size:          %d", batchSize)
			t.Logf("Branches cleaned:    %d", cleanedUp)

			t.Log("")
			t.Log("──── One-time setup ────")
			t.Logf("  Open repo + HEAD:    %s", tOpenRepo.Round(time.Microsecond))
			t.Logf("  Find sessions:       %s", tFindSessions.Round(time.Microsecond))
			t.Logf("  Resolve trees/files: %s", tResolveTreesFiles.Round(time.Microsecond))

			t.Log("")
			t.Log("──── Per-session (aggregated) ────")
			n := time.Duration(len(sessions))
			t.Logf("  Shadow ref resolve:     %s  (avg %s)",
				totalShadowResolve.Round(time.Microsecond),
				(totalShadowResolve / n).Round(time.Microsecond))
			t.Logf("  Has new content:        %s  (avg %s)",
				totalHasNewContent.Round(time.Microsecond),
				(totalHasNewContent / n).Round(time.Microsecond))
			t.Logf("  State machine (pure):   %s  (avg %s)",
				totalStateMachine.Round(time.Microsecond),
				(totalStateMachine / n).Round(time.Microsecond))
			t.Logf("  Overlap check:          %s  (avg %s)",
				totalOverlapCheck.Round(time.Microsecond),
				(totalOverlapCheck / n).Round(time.Microsecond))
			t.Logf("  State update (no-cond): %s  (avg %s)",
				totalStateUpdate.Round(time.Microsecond),
				(totalStateUpdate / n).Round(time.Microsecond))

			t.Log("")
			t.Log("──── PrepareCondensation breakdown (condensed sessions only) ────")
			if sessionsCondensed > 0 {
				cn := time.Duration(sessionsCondensed)
				t.Logf("  TOTAL PrepareCondensation: %s  (avg %s)",
					totalPrepCondense.Round(time.Microsecond),
					(totalPrepCondense / cn).Round(time.Microsecond))
				t.Logf("    Shadow ref resolve:     %s  (avg %s)  [%.1f%%]",
					totalPCShadowRef.Round(time.Microsecond),
					(totalPCShadowRef / cn).Round(time.Microsecond),
					pct(totalPCShadowRef, totalPrepCondense))
				t.Logf("    Extract session data:   %s  (avg %s)  [%.1f%%]",
					totalPCExtractData.Round(time.Microsecond),
					(totalPCExtractData / cn).Round(time.Microsecond),
					pct(totalPCExtractData, totalPrepCondense))
				t.Logf("    Filter files:           %s  (avg %s)  [%.1f%%]",
					totalPCFilterFiles.Round(time.Microsecond),
					(totalPCFilterFiles / cn).Round(time.Microsecond),
					pct(totalPCFilterFiles, totalPrepCondense))
				t.Logf("    Attribution:            %s  (avg %s)  [%.1f%%]",
					totalPCAttribution.Round(time.Microsecond),
					(totalPCAttribution / cn).Round(time.Microsecond),
					pct(totalPCAttribution, totalPrepCondense))
				t.Logf("    Summary generation:     %s  (avg %s)  [%.1f%%]",
					totalPCSummary.Round(time.Microsecond),
					(totalPCSummary / cn).Round(time.Microsecond),
					pct(totalPCSummary, totalPrepCondense))
				t.Logf("    Build result struct:    %s  (avg %s)  [%.1f%%]",
					totalPCBuildResult.Round(time.Microsecond),
					(totalPCBuildResult / cn).Round(time.Microsecond),
					pct(totalPCBuildResult, totalPrepCondense))
			}

			t.Log("")
			t.Log("──── Batch + finalize ────")
			t.Logf("  Batch write:         %s", tBatchWrite.Round(time.Microsecond))
			t.Logf("  Finalize all:        %s", tFinalize.Round(time.Microsecond))
			t.Logf("  Shadow cleanup:      %s", tCleanup.Round(time.Microsecond))

			t.Log("")
			t.Logf("──── TOTAL:              %s ────", totalPostCommit.Round(time.Millisecond))

			// Per-phase breakdown
			printPhaseBreakdown(t, profiles)

			// Top 10 slowest sessions with full decomposition
			printSlowestSessions(t, profiles, 10)

			// Skip reason distribution
			printSkipReasons(t, profiles)
		})
	}
}

// overlapCheckForProfile replicates shouldCondenseWithOverlapCheck logic for profiling.
func overlapCheckForProfile(
	ctx context.Context,
	repo *git.Repository,
	shadowBranchName string,
	commitObj *object.Commit,
	headTree, parentTree, shadowTree *object.Tree,
	hasNew bool,
	filesTouchedBefore []string,
	committedFileSet map[string]struct{},
	isActive bool,
) bool {
	if !hasNew {
		return false
	}
	if len(filesTouchedBefore) == 0 {
		return isActive
	}
	var committedTouchedFiles []string
	for _, f := range filesTouchedBefore {
		if _, ok := committedFileSet[f]; ok {
			committedTouchedFiles = append(committedTouchedFiles, f)
		}
	}
	if len(committedTouchedFiles) == 0 {
		return false
	}
	return filesOverlapWithContent(ctx, repo, shadowBranchName, commitObj, committedTouchedFiles, overlapOpts{
		headTree:      headTree,
		shadowTree:    shadowTree,
		parentTree:    parentTree,
		hasParentTree: true,
	})
}

// profilePrepareCondensation runs PrepareCondensation step by step with timing.
// This replicates the logic in manual_commit_condensation.go but with instrumentation.
func profilePrepareCondensation(
	ctx context.Context,
	s *ManualCommitStrategy,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	state *session.State,
	committedFiles map[string]struct{},
	shadowRef *plumbing.Reference,
	headTree *object.Tree,
	headTreeHashes TreeHashIndex,
	prof *perSessionProfile,
) {
	// Sub-step 1: Shadow branch resolution
	ts0 := time.Now()
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	ref := shadowRef
	hasShadowBranch := ref != nil
	if !hasShadowBranch {
		refName := plumbing.NewBranchReferenceName(shadowBranchName)
		var err error
		ref, err = repo.Reference(refName, true)
		hasShadowBranch = err == nil
	}
	prof.pcShadowRef = time.Since(ts0)

	// Sub-step 2: Extract session data
	ts1 := time.Now()
	if hasShadowBranch {
		_, _ = s.extractSessionData(ctx, repo, ref.Hash(), state.SessionID, state.FilesTouched, state.AgentType, state.TranscriptPath, state.CheckpointTranscriptStart, state.Phase.IsActive())
	} else if state.TranscriptPath != "" {
		_, _ = s.extractSessionDataFromLiveTranscript(ctx, state)
	}
	prof.pcExtractData = time.Since(ts1)

	// Sub-step 3: File filtering
	ts2 := time.Now()
	if len(committedFiles) > 0 && len(state.FilesTouched) > 0 {
		for _, f := range state.FilesTouched {
			_ = f // simulate the filtering loop
			if _, ok := committedFiles[f]; ok {
				_ = ok
			}
		}
	}
	prof.pcFilterFiles = time.Since(ts2)

	// Sub-step 4: Attribution
	ts3 := time.Now()
	_ = calculateSessionAttributions(ctx, repo, ref, &ExtractedSessionData{FilesTouched: state.FilesTouched}, state, attributionOpts{headTree: headTree, headTreeHashes: headTreeHashes})
	prof.pcAttribution = time.Since(ts3)

	// Sub-step 5: Summary generation (disabled in test — summarize needs API key)
	// Just measure the check
	ts4 := time.Now()
	// settings.IsSummarizeEnabled() check only
	prof.pcSummary = time.Since(ts4)

	// Sub-step 6: Build result struct
	ts5 := time.Now()
	_ = &cpkg.WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    state.SessionID,
		Strategy:     StrategyNameManualCommit,
	}
	prof.pcBuildResult = time.Since(ts5)

	prof.prepCondense = prof.pcShadowRef + prof.pcExtractData + prof.pcFilterFiles +
		prof.pcAttribution + prof.pcSummary + prof.pcBuildResult
}

func pct(part, total time.Duration) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func printPhaseBreakdown(t *testing.T, profiles []perSessionProfile) {
	t.Helper()
	type phaseStats struct {
		count                                       int
		shadowResolve, hasNewContent                time.Duration
		overlapCheck, prepCondense, stateUpdate     time.Duration
		pcExtractData, pcAttribution, pcFilterFiles time.Duration
		pcShadowRef, pcSummary, pcBuildResult       time.Duration
	}

	byPhase := map[session.Phase]*phaseStats{
		session.PhaseEnded:  {},
		session.PhaseIdle:   {},
		session.PhaseActive: {},
	}

	for _, p := range profiles {
		ps := byPhase[p.phase]
		ps.count++
		ps.shadowResolve += p.shadowResolve
		ps.hasNewContent += p.hasNewContent
		ps.overlapCheck += p.overlapCheck
		ps.prepCondense += p.prepCondense
		ps.stateUpdate += p.stateUpdate
		ps.pcExtractData += p.pcExtractData
		ps.pcAttribution += p.pcAttribution
		ps.pcFilterFiles += p.pcFilterFiles
		ps.pcShadowRef += p.pcShadowRef
		ps.pcSummary += p.pcSummary
		ps.pcBuildResult += p.pcBuildResult
	}

	t.Log("")
	t.Log("──── Per-phase breakdown ────")
	for _, phase := range []session.Phase{session.PhaseEnded, session.PhaseIdle, session.PhaseActive} {
		ps := byPhase[phase]
		if ps.count == 0 {
			continue
		}
		total := ps.shadowResolve + ps.hasNewContent + ps.overlapCheck + ps.prepCondense + ps.stateUpdate
		cn := time.Duration(ps.count)
		t.Logf("  %s (%d sessions):  total=%s  avg=%s/session",
			strings.ToUpper(string(phase)), ps.count,
			total.Round(time.Microsecond),
			(total / cn).Round(time.Microsecond))
		t.Logf("    shadow=%s  content=%s  overlap=%s  prepare=%s  stateUpd=%s",
			ps.shadowResolve.Round(time.Microsecond),
			ps.hasNewContent.Round(time.Microsecond),
			ps.overlapCheck.Round(time.Microsecond),
			ps.prepCondense.Round(time.Microsecond),
			ps.stateUpdate.Round(time.Microsecond))
		if ps.prepCondense > 0 {
			t.Logf("    PrepareCondensation sub-steps:  extract=%s  attrib=%s  filter=%s  shadow=%s",
				ps.pcExtractData.Round(time.Microsecond),
				ps.pcAttribution.Round(time.Microsecond),
				ps.pcFilterFiles.Round(time.Microsecond),
				ps.pcShadowRef.Round(time.Microsecond))
		}
	}
}

func printSlowestSessions(t *testing.T, profiles []perSessionProfile, n int) {
	t.Helper()
	type ranked struct {
		total time.Duration
		idx   int
	}
	ranked_ := make([]ranked, len(profiles))
	for i, p := range profiles {
		ranked_[i] = ranked{
			total: p.shadowResolve + p.hasNewContent + p.overlapCheck + p.prepCondense + p.stateUpdate,
			idx:   i,
		}
	}
	sort.Slice(ranked_, func(i, j int) bool { return ranked_[i].total > ranked_[j].total })

	t.Log("")
	t.Logf("──── Top %d slowest sessions ────", n)
	for i := 0; i < n && i < len(ranked_); i++ {
		p := profiles[ranked_[i].idx]
		total := ranked_[i].total
		if p.condensed {
			t.Logf("  %s (%s, condensed): total=%s",
				p.sessionID, p.phase, total.Round(time.Microsecond))
			t.Logf("      shadow=%s  content=%s  overlap=%s  prepare=%s",
				p.shadowResolve.Round(time.Microsecond),
				p.hasNewContent.Round(time.Microsecond),
				p.overlapCheck.Round(time.Microsecond),
				p.prepCondense.Round(time.Microsecond))
			t.Logf("      [prepare: extract=%s  attrib=%s  filter=%s]",
				p.pcExtractData.Round(time.Microsecond),
				p.pcAttribution.Round(time.Microsecond),
				p.pcFilterFiles.Round(time.Microsecond))
		} else {
			t.Logf("  %s (%s, skipped=%s): total=%s  shadow=%s  content=%s  overlap=%s  stateUpd=%s",
				p.sessionID, p.phase, p.skippedWhy, total.Round(time.Microsecond),
				p.shadowResolve.Round(time.Microsecond),
				p.hasNewContent.Round(time.Microsecond),
				p.overlapCheck.Round(time.Microsecond),
				p.stateUpdate.Round(time.Microsecond))
		}
	}
}

func printSkipReasons(t *testing.T, profiles []perSessionProfile) {
	t.Helper()
	reasons := make(map[string]int)
	for _, p := range profiles {
		if !p.condensed {
			reasons[p.skippedWhy]++
		}
	}
	t.Log("")
	t.Log("──── Skip reason distribution ────")
	for reason, count := range reasons {
		t.Logf("  %-20s %d", reason, count)
	}
}
