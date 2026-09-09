package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures for the planner tables. Remote names and counts match the design
// plan's state table so the pinned strings read the same as the spec.
var (
	planRefA = plumbing.ReferenceName("refs/entire/checkpoints/aa/01HZAAAAAAAAAAAAAAAAAAAAAA")
	planRefB = plumbing.ReferenceName("refs/entire/checkpoints/bb/01HZBBBBBBBBBBBBBBBBBBBBBB")
)

// testEntireRemote is the Entire remote's name in these fixtures.
const testEntireRemote = "entire"

func planRemote(name, url string, entire bool, inv *syncInventory) syncRemoteInfo {
	return syncRemoteInfo{Name: name, PushURLs: []string{url}, IsEntire: entire, Inventory: inv}
}

func refsInventory(n int, v1 bool) *syncInventory {
	inv := &syncInventory{HasV1Branch: v1}
	for i := range n {
		inv.Refs = append(inv.Refs, plumbing.ReferenceName("refs/entire/checkpoints/aa/"+strings.Repeat("A", 20)+string(rune('A'+i%26))+string(rune('A'+(i/26)%26))+"0000"))
	}
	return inv
}

func emptyInventory() *syncInventory { return &syncInventory{} }

// migrateInputs is state (c): entire elected, origin holds 128 refs + v1.
func migrateInputs() syncInputs {
	return syncInputs{
		Remotes: []syncRemoteInfo{
			planRemote("origin", "https://github.com/o/r", false, refsInventory(128, true)),
			planRemote(testEntireRemote, "entire://cluster.test/gh/o/r", true, emptyInventory()),
		},
		Elected:          syncElection{Name: testEntireRemote, Source: syncSourceEntire},
		EntireRemotes:    []string{testEntireRemote},
		PrimaryIsRefs:    true,
		LocalCheckpoints: 142,
		LocalRefs:        []plumbing.ReferenceName{planRefA, planRefB},
		QueuedRefs:       3,
	}
}

// planCase is one row of the planner tables below. The state table is split
// across two tests only to keep each function within the maintainability lint;
// both run the same loop via runPlanCases.
type planCase struct {
	name  string
	in    syncInputs
	check func(t *testing.T, plan syncPlan)
}

func runPlanCases(t *testing.T, tests []planCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.check(t, planCheckpointSync(tt.in))
		})
	}
}

// TestPlanCheckpointSync_Destinations covers the rows that decide WHERE
// checkpoints go: no remotes, dedicated store, fail-closed, pickers, and the
// already-home and fresh-repo answers (a, b, e, g, h, n).
func TestPlanCheckpointSync_Destinations(t *testing.T) {
	t.Parallel()

	runPlanCases(t, []planCase{
		{
			name: "no_remotes",
			in:   syncInputs{},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateNoRemotes, plan.State)
				assert.Equal(t, syncReasonNoRemotes, plan.Reason)
				assert.Empty(t, plan.Steps)
				require.NoError(t, plan.ExitError)
			},
		},
		{
			name: "dedicated_out_of_scope (h)",
			in: syncInputs{
				Remotes:         []syncRemoteInfo{planRemote("origin", "https://github.com/o/r", false, nil), planRemote(testEntireRemote, "entire://c/gh/o/r", true, nil)},
				DedicatedRemote: true,
				DedicatedRepo:   "org/checkpoints",
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateDedicatedRemote, plan.State)
				assert.Equal(t, "This repo stores checkpoints in a dedicated checkpoint repository (org/checkpoints, strategy_options.checkpoint_remote). "+
					"checkpoint migrate manages the single-remote setup and does not move data out of a dedicated checkpoint repository. "+
					"Remove checkpoint_remote from your settings first if you want checkpoints to live on one of this repo's remotes instead.", plan.Reason)
				assert.Empty(t, plan.Steps)
			},
		},
		{
			name: "fail_closed_without_to (g) non-interactive exits 1",
			in: syncInputs{
				Remotes:     []syncRemoteInfo{planRemote("origin", "https://github.com/o/r", false, nil), planRemote("fork", "https://github.com/me/r", false, nil)},
				ElectionErr: errors.New(`checkpoint_push_remote "gone" is not a configured git remote; checkpoint sync disabled until fixed`),
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateFailClosed, plan.State)
				assert.True(t, plan.Warn)
				assert.Equal(t, `Checkpoints are NOT syncing: checkpoint_push_remote "gone" is not a configured git remote; checkpoint sync disabled until fixed`, plan.Reason)
				assert.Equal(t, []string{syncNoteRemoveKey}, plan.Notes)
				require.Len(t, plan.Next, 1)
				assert.Equal(t, "Fix", plan.Next[0].Label)
				assert.Equal(t, "entire checkpoint migrate --to <remote>   (rewrites the setting in .entire/settings.local.json)", plan.Next[0].Command)
				require.Error(t, plan.ExitError)
				assert.Empty(t, plan.Candidates)
			},
		},
		{
			name: "fail_closed_interactive offers picker, no exit error",
			in: syncInputs{
				Remotes:     []syncRemoteInfo{planRemote("origin", "https://github.com/o/r", false, nil), planRemote("fork", "https://github.com/me/r", false, nil)},
				ElectionErr: errors.New("gone"),
				Interactive: true,
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateFailClosed, plan.State)
				require.NoError(t, plan.ExitError)
				assert.Equal(t, []string{"origin", "fork"}, plan.Candidates)
			},
		},
		{
			name: "fail_closed_with_to writes setting and proceeds",
			in: syncInputs{
				Remotes:          []syncRemoteInfo{planRemote("origin", "https://github.com/o/r", false, emptyInventory()), planRemote("fork", "https://github.com/me/r", false, emptyInventory())},
				ElectionErr:      errors.New("gone"),
				PrimaryIsRefs:    true,
				LocalCheckpoints: 2,
				LocalRefs:        []plumbing.ReferenceName{planRefA, planRefB},
				Opts:             checkpointMigrateOptions{To: "fork"},
			},
			check: func(t *testing.T, plan syncPlan) {
				require.NoError(t, plan.ExitError)
				assert.Equal(t, "fork", plan.Destination)
				assert.Equal(t, syncSourceFlag, plan.DestinationSource)
				assert.True(t, plan.WriteDestinationSetting)
				assert.Equal(t, syncStep("write_destination"), plan.Steps[0])
				assert.Equal(t, syncStatePublishOnly, plan.State)
			},
		},
		{
			name: "single_entire_already_home (a)",
			in: syncInputs{
				Remotes:          []syncRemoteInfo{planRemote("origin", "entire://c/gh/o/r", true, &syncInventory{Refs: []plumbing.ReferenceName{planRefA, planRefB}})},
				Elected:          syncElection{Name: "origin", Source: syncSourceSole},
				EntireRemotes:    []string{"origin"},
				PrimaryIsRefs:    true,
				LocalCheckpoints: 2,
				LocalRefs:        []plumbing.ReferenceName{planRefA, planRefB},
				Marker:           syncMarkerDone,
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateAlreadyHome, plan.State)
				assert.Equal(t, "✓ Checkpoints live on origin. Nothing to do.", plan.Reason)
				assert.Empty(t, plan.Steps)
				assert.Empty(t, plan.NextCommand)
			},
		},
		{
			name: "marker_pending_already_home records marker (n)",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes[0].Inventory = emptyInventory()
				in.Remotes[1].Inventory = &syncInventory{Refs: []plumbing.ReferenceName{planRefA, planRefB}}
				in.QueuedRefs = 0
				in.Opts.Yes = true
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateAlreadyHome, plan.State)
				assert.Equal(t, "✓ Checkpoints live on entire (your Entire remote). Nothing to do.", plan.Reason)
				assert.Equal(t, []syncStep{stepRecordMarker}, plan.Steps, "with --yes the verdict is recorded")
				assert.Empty(t, plan.NextCommand, "a marker write alone is not a runnable plan")
			},
		},
		{
			name: "marker pending already home without consent writes nothing (n)",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes[0].Inventory = emptyInventory()
				in.Remotes[1].Inventory = &syncInventory{Refs: []plumbing.ReferenceName{planRefA, planRefB}}
				in.QueuedRefs = 0
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateAlreadyHome, plan.State)
				assert.Empty(t, plan.Steps, "a bare non-interactive run writes nothing, the ledger included")
				assert.Contains(t, plan.Notes, "Run entire checkpoint migrate --yes to record this, so entire status stops suggesting it.")
			},
		},
		{
			name: "multi_remote_no_entire_needs_choice (b) non-interactive exit 0",
			in: syncInputs{
				Remotes: []syncRemoteInfo{
					planRemote("origin", "https://github.com/o/r", false, nil),
					planRemote("fork", "https://github.com/me/r", false, nil),
					planRemote("deploy", "https://deploy.example/r", false, nil),
				},
				Elected: syncElection{Name: "origin", Source: syncSourceDefault},
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateChooseDestination, plan.State)
				assert.Equal(t, "This repo has 3 remotes (origin, fork, deploy) and none is an Entire remote. Checkpoints go to exactly one; today that is origin (the default).", plan.Reason)
				assert.Equal(t, "entire checkpoint migrate --to <remote>", plan.NextCommand)
				assert.Equal(t, []syncAdvice{{Label: "To choose one", Command: "entire checkpoint migrate --to <remote>"}}, plan.Next)
				assert.Equal(t, []string{syncNoteLocalFile}, plan.Notes)
				require.NoError(t, plan.ExitError)
				assert.Empty(t, plan.Candidates)
			},
		},
		{
			name: "multi_remote_no_entire interactive picker puts current election first",
			in: syncInputs{
				Remotes: []syncRemoteInfo{
					planRemote("fork", "https://github.com/me/r", false, nil),
					planRemote("origin", "https://github.com/o/r", false, nil),
				},
				Elected:     syncElection{Name: "origin", Source: syncSourceDefault},
				Interactive: true,
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, []string{"origin", "fork"}, plan.Candidates)
			},
		},
		{
			name: "entire_elected_origin_holds_artifacts (c)",
			in:   migrateInputs(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateMigrate, plan.State)
				assert.Equal(t, testEntireRemote, plan.Destination)
				assert.Equal(t, syncSourceEntire, plan.DestinationSource)
				assert.False(t, plan.WriteDestinationSetting)
				assert.Equal(t, []string{"origin"}, plan.Sources)
				assert.Equal(t, []syncStep{stepHydrate, stepConvertV1, stepRequeue, stepPublish, stepVerify, stepRemove, stepRecordMarker}, plan.Steps)
				assert.Equal(t, "The entire/checkpoints/v1 branch is converted to refs first. "+
					"origin (github.com/o/r) holds 128 checkpoint refs and the entire/checkpoints/v1 branch. "+
					"They will be copied to entire, verified, and — if you agree — removed from origin.", plan.Reason)
				assert.Equal(t, "entire checkpoint migrate --yes", plan.NextCommand)
				assert.Equal(t, []syncAdvice{
					{Label: "Nothing was changed. To run this plan", Command: "entire checkpoint migrate --yes"},
					{Label: "To also delete the old copies from origin after verification", Command: "entire checkpoint migrate --yes --remove"},
				}, plan.Next)
			},
		},
		{
			name: "entire_elected_publish_only (d)",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes[0].Inventory = emptyInventory()
				in.LocalCheckpoints = 14
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStatePublishOnly, plan.State)
				assert.Empty(t, plan.Sources)
				assert.Equal(t, []syncStep{stepRequeue, stepPublish, stepVerify, stepRecordMarker}, plan.Steps)
				assert.Equal(t, "14 checkpoints exist only in this clone. They will be pushed to entire (your Entire remote).", plan.Reason)
				assert.Equal(t, []syncAdvice{{Label: "Nothing was changed. To run this plan", Command: "entire checkpoint migrate --yes"}}, plan.Next)
			},
		},
		{
			name: "publish_only singular",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes[0].Inventory = emptyInventory()
				in.LocalCheckpoints = 1
				in.LocalRefs = in.LocalRefs[:1]
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, "1 checkpoint exists only in this clone. It will be pushed to entire (your Entire remote).", plan.Reason)
			},
		},
		{
			name: "fresh_repo_zero_checkpoints entire (e)",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes[0].Inventory = emptyInventory()
				in.LocalCheckpoints, in.LocalRefs, in.QueuedRefs = 0, nil, 0
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateSetDestination, plan.State)
				assert.Equal(t, "✓ Checkpoints will sync to entire (your Entire remote) from your first commit.", plan.Reason)
				assert.Equal(t, []syncStep{stepRecordMarker}, plan.Steps)
				assert.Empty(t, plan.NextCommand)
			},
		},
		{
			name: "fresh_repo_zero_checkpoints with --to (e)",
			in: syncInputs{
				Remotes:       []syncRemoteInfo{planRemote("origin", "https://github.com/o/r", false, emptyInventory()), planRemote("fork", "https://github.com/me/r", false, emptyInventory())},
				Elected:       syncElection{Name: "origin", Source: syncSourceDefault},
				PrimaryIsRefs: true,
				Opts:          checkpointMigrateOptions{To: "fork"},
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateSetDestination, plan.State)
				assert.True(t, plan.WriteDestinationSetting)
				assert.Equal(t, "Checkpoints will sync to fork once recorded as strategy_options.checkpoint_push_remote in .entire/settings.local.json.", plan.Reason)
				assert.Equal(t, []syncStep{stepWriteDestination, stepRecordMarker}, plan.Steps)
				assert.Equal(t, "entire checkpoint migrate --to fork --yes", plan.NextCommand)
			},
		},
	})
}

// TestPlanCheckpointSync_Migrations covers the rows that move data and the
// flag interactions: several Entire remotes, pinned elsewhere, conversion,
// sources, --from/--to validation, push_sessions, and unknown inventories
// (c, d, f, i, l, o).
func TestPlanCheckpointSync_Migrations(t *testing.T) {
	t.Parallel()

	runPlanCases(t, []planCase{
		{
			name: "two_entire_remotes_non_interactive (f) exits 1",
			in: syncInputs{
				Remotes: []syncRemoteInfo{
					planRemote("origin", "https://github.com/o/r", false, nil),
					planRemote(testEntireRemote, "entire://c/gh/o/r", true, nil),
					planRemote("entire-eu", "entire://eu/gh/o/r", true, nil),
				},
				Elected:       syncElection{Name: "origin", Source: syncSourceDefault},
				EntireRemotes: []string{testEntireRemote, "entire-eu"},
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateChooseDestination, plan.State)
				assert.Equal(t, "This repo has 2 Entire remotes (entire, entire-eu). Checkpoints can go to only one.", plan.Reason)
				assert.Equal(t, []syncAdvice{{Label: "Pass --to <remote> to choose", Command: "entire checkpoint migrate --to entire"}}, plan.Next)
				assert.EqualError(t, plan.ExitError, "several Entire remotes (entire, entire-eu); pass --to <remote>")
			},
		},
		{
			name: "two_entire_remotes interactive lists them",
			in: syncInputs{
				Remotes: []syncRemoteInfo{
					planRemote(testEntireRemote, "entire://c/gh/o/r", true, nil),
					planRemote("entire-eu", "entire://eu/gh/o/r", true, nil),
				},
				Elected:       syncElection{Name: testEntireRemote, Source: syncSourceFirst},
				EntireRemotes: []string{testEntireRemote, "entire-eu"},
				Interactive:   true,
			},
			check: func(t *testing.T, plan syncPlan) {
				require.NoError(t, plan.ExitError)
				assert.Equal(t, []string{testEntireRemote, "entire-eu"}, plan.Candidates)
			},
		},
		{
			name: "two_entire_remotes_with_to",
			in: syncInputs{
				Remotes: []syncRemoteInfo{
					planRemote(testEntireRemote, "entire://c/gh/o/r", true, emptyInventory()),
					planRemote("entire-eu", "entire://eu/gh/o/r", true, emptyInventory()),
				},
				Elected:          syncElection{Name: testEntireRemote, Source: syncSourceFirst},
				EntireRemotes:    []string{testEntireRemote, "entire-eu"},
				PrimaryIsRefs:    true,
				LocalCheckpoints: 2,
				LocalRefs:        []plumbing.ReferenceName{planRefA, planRefB},
				Opts:             checkpointMigrateOptions{To: "entire-eu"},
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStatePublishOnly, plan.State)
				assert.Equal(t, "entire-eu", plan.Destination)
				assert.True(t, plan.WriteDestinationSetting)
			},
		},
		{
			name: "pinned_elsewhere (o)",
			in: syncInputs{
				Remotes: []syncRemoteInfo{
					planRemote("origin", "https://github.com/o/r", false, nil),
					planRemote("fork", "https://github.com/me/r", false, nil),
					planRemote(testEntireRemote, "entire://c/gh/o/r", true, nil),
				},
				Elected:       syncElection{Name: "fork", Source: syncSourceConfig},
				EntireRemotes: []string{testEntireRemote},
			},
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStatePinnedElsewhere, plan.State)
				assert.Equal(t, "Checkpoints sync to fork (set by checkpoint_push_remote), but this repo also has an Entire remote (entire). Entire is built to hold them.", plan.Reason)
				assert.Equal(t, []syncAdvice{{Label: "To switch", Command: "entire checkpoint migrate --to entire"}}, plan.Next)
				assert.Equal(t, []string{testEntireRemote}, plan.Candidates)
				require.NoError(t, plan.ExitError)
			},
		},
		{
			name: "git_branch_adds_convert_first (i)",
			in: func() syncInputs {
				in := migrateInputs()
				in.PrimaryIsRefs = false
				in.LocalRefs = nil
				in.QueuedRefs = 0
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, syncStateMigrate, plan.State)
				assert.Equal(t, []syncStep{stepHydrate, stepConvertV1, stepConvertBackend, stepRequeue, stepPublish, stepVerify, stepRemove, stepRecordMarker}, plan.Steps)
				assert.True(t, strings.HasPrefix(plan.Reason, "This repo still uses the shared entire/checkpoints/v1 branch. Each checkpoint will be converted to its own ref and git-refs made the primary store (.entire/settings.json). "), plan.Reason)
			},
		},
		{
			name: "git_branch_env_override_skips_write",
			in: func() syncInputs {
				in := migrateInputs()
				in.PrimaryIsRefs = false
				in.BackendEnvOverride = true
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Contains(t, plan.Reason, "ENTIRE_CHECKPOINTS_PRIMARY is set, so the backend is not written to settings and the old entire/checkpoints/v1 branch is left in place.")
				assert.True(t, plan.hasStep(stepConvertBackend))
			},
		},
		{
			name: "multiple_holders_all_sources",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes = append(in.Remotes, planRemote("fork", "https://github.com/me/r", false, refsInventory(3, false)))
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, []string{"origin", "fork"}, plan.Sources)
				assert.Contains(t, plan.Reason, "fork (github.com/me/r) holds 3 checkpoint refs.")
				assert.Contains(t, plan.Reason, "removed from origin, fork.")
				assert.Equal(t, "To also delete the old copies from origin, fork after verification", plan.Next[1].Label)
			},
		},
		{
			name: "from_narrows_sources",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes = append(in.Remotes, planRemote("fork", "https://github.com/me/r", false, refsInventory(3, false)))
				in.Opts.From = []string{"fork"}
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, []string{"fork"}, plan.Sources)
				assert.Equal(t, "entire checkpoint migrate --from fork --yes", plan.NextCommand)
			},
		},
		{
			name: "from_equals_destination_error",
			in: func() syncInputs {
				in := migrateInputs()
				in.Opts.From = []string{testEntireRemote}
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.EqualError(t, plan.ExitError, `--from "entire" is the destination; pass a different remote`)
			},
		},
		{
			name: "from_unconfigured_error",
			in: func() syncInputs {
				in := migrateInputs()
				in.Opts.From = []string{"nope"}
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.EqualError(t, plan.ExitError, `--from "nope" is not a configured git remote`)
			},
		},
		{
			name: "to_unconfigured_error",
			in: func() syncInputs {
				in := migrateInputs()
				in.Opts.To = "nope"
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.EqualError(t, plan.ExitError, `--to "nope" is not a configured git remote`)
			},
		},
		{
			name: "to_already_elected_no_write",
			in: func() syncInputs {
				in := migrateInputs()
				in.Opts.To = testEntireRemote
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.False(t, plan.WriteDestinationSetting)
				assert.Equal(t, syncSourceEntire, plan.DestinationSource)
				assert.False(t, plan.hasStep(stepWriteDestination))
				assert.Equal(t, "entire checkpoint migrate --to entire --yes", plan.NextCommand)
			},
		},
		{
			name: "remove_flag_carried_into_next_command",
			in: func() syncInputs {
				in := migrateInputs()
				in.Opts.Remove = true
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, "entire checkpoint migrate --yes --remove", plan.NextCommand)
				assert.Len(t, plan.Next, 1, "no second suggestion when --remove is already given")
			},
		},
		{
			name: "push_sessions_disabled_annotates (l)",
			in: func() syncInputs {
				in := migrateInputs()
				in.PushSessionsDisabled = true
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.True(t, plan.Warn)
				assert.Equal(t, formatPushSessionsDisabled(), plan.Notes)
				assert.Equal(t, syncStateMigrate, plan.State, "the plan still shows what would happen")
			},
		},
		{
			name: "inventory_error_goes_to_unknown",
			in: func() syncInputs {
				in := migrateInputs()
				in.Remotes = append(in.Remotes, planRemote("fork", "https://github.com/me/r", false, &syncInventory{Err: errors.New("auth failed")}))
				return in
			}(),
			check: func(t *testing.T, plan syncPlan) {
				assert.Equal(t, []string{"origin"}, plan.Sources)
				assert.Equal(t, []string{"fork"}, plan.UnknownSources)
				assert.Contains(t, plan.Notes, "Could not check fork (github.com/me/r): auth failed. It is left untouched.")
			},
		},
	})
}

func TestNextSyncCommand(t *testing.T) {
	t.Parallel()

	in := migrateInputs()
	in.Opts = checkpointMigrateOptions{To: testEntireRemote, From: []string{"origin", "fork"}, Remove: true}
	plan := syncPlan{Steps: []syncStep{stepHydrate, stepPublish, stepRemove}}
	assert.Equal(t, "entire checkpoint migrate --to entire --from origin --from fork --yes --remove", nextSyncCommand(plan, in))

	assert.Empty(t, nextSyncCommand(syncPlan{}, in))
	assert.Empty(t, nextSyncCommand(syncPlan{Steps: []syncStep{stepRecordMarker}}, in))
	in.Opts.Remove = false
	assert.Equal(t, "entire checkpoint migrate --to entire --from origin --from fork --yes", nextSyncCommand(plan, in))
}

func TestDescribeDestination(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "entire (your Entire remote)", describeDestination(testEntireRemote, syncSourceEntire))
	assert.Equal(t, "fork (set by checkpoint_push_remote)", describeDestination("fork", syncSourceConfig))
	assert.Equal(t, "fork (follows your branch's push destination)", describeDestination("fork", syncSourceObserved))
	assert.Equal(t, "origin (the default)", describeDestination("origin", syncSourceDefault))
	assert.Equal(t, "fork (--to)", describeDestination("fork", syncSourceFlag))
	assert.Equal(t, "origin", describeDestination("origin", syncSourceSole))
	assert.Equal(t, "origin", describeDestination("origin", syncSourceFirst))
}

func TestSyncRuntimeCopy(t *testing.T) {
	t.Parallel()

	origin := planRemote("origin", "https://user:secret@github.com/o/r", false, nil)

	assert.Equal(t, []string{
		"3 of 128 checkpoint refs did not arrive on entire. Nothing was removed from origin.",
		"Run again to retry:  entire checkpoint migrate",
	}, formatVerifyMissing(3, 128, testEntireRemote, "origin"))
	assert.Equal(t, "2 of 14 checkpoint refs did not arrive on entire.", formatVerifyMissing(2, 14, testEntireRemote, "")[0])

	assert.Equal(t, []string{
		"Could not remove old copies from origin: boom. Your checkpoints are safe on entire.",
		"Retry later with:  entire checkpoint migrate --remove",
	}, formatRemoveFailed("origin", errors.New("boom"), testEntireRemote))

	assert.Equal(t, []string{
		"strategy_options.push_sessions is false, so nothing can be pushed. Refs stay queued.",
		"Set push_sessions to true (or remove it) and rerun.",
	}, formatPushSessionsDisabled())

	assert.Equal(t, "Older copies stay on origin. Remove them any time with:  entire checkpoint migrate --remove", formatRemovalDeclined("origin"))
	assert.Equal(t, "Could not push to entire: boom. Refs stay queued and will go with your next git push to entire.", formatPublishFailed(testEntireRemote, errors.New("boom")))
	assert.Equal(t, "Could not fetch checkpoints from origin: boom", formatHydrateFailed("origin", errors.New("boom")))
	assert.Equal(t, "Could not check origin (github.com/o/r): boom. It is left untouched.", formatUnknownSource(origin, errors.New("boom")),
		"credentials and scheme are dropped from the displayed URL")

	assert.Equal(t, "Delete 128 checkpoint refs and the entire/checkpoints/v1 branch from origin (github.com/o/r)? Your code is untouched.",
		formatRemovalPrompt(128, true, origin))
	assert.Equal(t, "Delete 1 checkpoint ref from origin (github.com/o/r)? Your code is untouched.", formatRemovalPrompt(1, false, origin))
	assert.Equal(t, "Delete the entire/checkpoints/v1 branch from origin (github.com/o/r)? Your code is untouched.", formatRemovalPrompt(0, true, origin))

	assert.Equal(t, "Switch checkpoint sync to entire?", formatSwitchToEntirePrompt(testEntireRemote))
	assert.Equal(t, "✓ Checkpoints will sync to fork. Recorded strategy_options.checkpoint_push_remote in .entire/settings.local.json.", formatDestinationRecorded("fork"))
	assert.Equal(t, "✓ Checkpoints already sync to entire (your Entire remote). Nothing to record.", formatDestinationUnchanged(testEntireRemote, syncSourceEntire))
	assert.Equal(t, []string{
		"Warning: checkpoint_push_remote could not be set: the local settings file is tracked.",
		"If .entire/settings.local.json is tracked by git, run `git rm --cached .entire/settings.local.json` and keep it out of version control.",
	}, formatDestinationWriteFailed("the local settings file is tracked"))
	assert.Equal(t, "Commit .entire/settings.json so everyone writes the same store.", formatCommitSettingsHint())
	assert.Equal(t, "ENTIRE_CHECKPOINTS_PRIMARY is set, so the backend was not written to settings.", formatBackendEnvOverride())
	assert.Equal(t, "Could not record the sync result in .git (boom); entire status may keep suggesting this command.", formatMarkerWriteFailed(errors.New("boom")))
	assert.Equal(t, "OPF cancelled; refs stay queued for the next push.", formatOPFCancelled())
	assert.Equal(t, syncNothingChanged, "Nothing changed. Run entire checkpoint migrate when you are ready.")
}

func TestSyncSpinnerCopy(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Converting 128 checkpoints to refs", formatConvertStart(128))
	assert.Equal(t, "Converted 116 checkpoints to refs (12 already converted)", formatConvertDone(116, 12))
	assert.Equal(t, "Converted 1 checkpoint to refs", formatConvertDone(1, 0))
	assert.Equal(t, "Fetching checkpoints from origin", formatFetchStart("origin"))
	assert.Equal(t, "Fetched 128 checkpoint refs from origin", formatFetchDone(128, "origin"))
	assert.Equal(t, "Queued 142 checkpoint refs for push", formatQueued(142))
	assert.Equal(t, "Pushing 142 checkpoint refs to entire", formatPushStart(142, testEntireRemote))
	assert.Equal(t, "Pushed 142 checkpoint refs to entire", formatPushDone(142, testEntireRemote))
	assert.Equal(t, "Verifying on entire", formatVerifyStart(testEntireRemote))
	assert.Equal(t, "Verified 142 checkpoint refs on entire", formatVerifyDone(142, testEntireRemote))
	assert.Equal(t, "Removing 128 checkpoint refs from origin (12/128)", formatRemoveProgress(12, 128, "origin"))
	assert.Equal(t, "Removed 128 checkpoint refs and entire/checkpoints/v1 from origin", formatRemoveDone(128, true, "origin"))
	assert.Equal(t, "Removed 3 checkpoint refs from fork", formatRemoveDone(3, false, "fork"))
}

func TestFormatCombinedConfirm(t *testing.T) {
	t.Parallel()

	in := migrateInputs()
	in.PrimaryIsRefs = false
	plan := planCheckpointSync(in)
	assert.Equal(t, "Convert to refs, copy 128 checkpoints from origin to entire, and verify? (writes .entire/settings.json)", formatCombinedConfirm(in, plan))

	in = migrateInputs()
	in.Remotes[0].Inventory = emptyInventory()
	in.LocalCheckpoints = 14
	plan = planCheckpointSync(in)
	assert.Equal(t, "Push 14 checkpoints to entire and verify?", formatCombinedConfirm(in, plan))

	in = syncInputs{
		Remotes:       []syncRemoteInfo{planRemote("origin", "https://github.com/o/r", false, emptyInventory()), planRemote("fork", "https://github.com/me/r", false, emptyInventory())},
		Elected:       syncElection{Name: "origin", Source: syncSourceDefault},
		PrimaryIsRefs: true,
		Opts:          checkpointMigrateOptions{To: "fork"},
	}
	plan = planCheckpointSync(in)
	assert.Equal(t, "Record fork as the checkpoint sync remote? (writes .entire/settings.local.json)", formatCombinedConfirm(in, plan))

	assert.Empty(t, formatCombinedConfirm(in, syncPlan{}))
}

func TestRenderSyncHeader(t *testing.T) {
	t.Parallel()

	in := migrateInputs()
	in.PrimaryIsRefs = false
	in.Remotes = append(in.Remotes,
		planRemote("fork", "https://github.com/me/r", false, emptyInventory()),
		planRemote("broken", "https://example.com/x", false, &syncInventory{Err: errors.New("down")}),
	)
	plan := planCheckpointSync(in)

	var buf bytes.Buffer
	renderSyncHeader(&buf, newStatusStyles(&buf), in, plan)
	want := "Checkpoint migration\n\n" +
		"  Destination  entire (your Entire remote)\n" +
		"  Store        git-branch → git-refs\n" +
		"  Local        142 checkpoints, 3 not yet pushed\n" +
		"  Elsewhere    origin (github.com/o/r): 128 refs + entire/checkpoints/v1\n" +
		"    fork (github.com/me/r): none\n" +
		"    broken (example.com/x): could not check\n"
	assert.Equal(t, want, buf.String())
}

func TestRenderSyncReasonAndNext(t *testing.T) {
	t.Parallel()

	in := migrateInputs()
	plan := planCheckpointSync(in)
	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	renderSyncReason(&buf, sty, plan)
	renderSyncNext(&buf, sty, plan)
	got := buf.String()
	assert.Contains(t, got, "origin (github.com/o/r) holds 128 checkpoint refs")
	assert.Contains(t, got, "\n  Nothing was changed. To run this plan:\n    entire checkpoint migrate --yes\n")
	assert.Contains(t, got, "  To also delete the old copies from origin after verification:\n    entire checkpoint migrate --yes --remove\n")

	buf.Reset()
	renderSyncReason(&buf, sty, syncPlan{Reason: "boom", Warn: true, Notes: []string{"note"}})
	assert.Equal(t, "\n  ! boom\n    note\n", buf.String())

	buf.Reset()
	renderSyncNext(&buf, sty, syncPlan{})
	assert.Empty(t, buf.String())
}

func TestRenderDryRunSteps(t *testing.T) {
	t.Parallel()

	in := migrateInputs()
	in.PrimaryIsRefs = false
	in.Opts.To = testEntireRemote
	in.Elected = syncElection{Name: "origin", Source: syncSourceConfig}
	plan := planCheckpointSync(in)
	require.Equal(t, []syncStep{stepWriteDestination, stepHydrate, stepConvertV1, stepConvertBackend, stepRequeue, stepPublish, stepVerify, stepRemove, stepRecordMarker}, plan.Steps)

	var buf bytes.Buffer
	renderDryRunSteps(&buf, in, plan)
	assert.Equal(t, "\n"+
		"  Would record entire as strategy_options.checkpoint_push_remote in .entire/settings.local.json\n"+
		"  Would fetch 128 checkpoint refs from origin\n"+
		"  Would convert 142 checkpoints to refs\n"+
		"  Would set the checkpoint store to git-refs (.entire/settings.json)\n"+
		"  Would queue every local checkpoint ref for push\n"+
		"  Would push 142 checkpoint refs to entire\n"+
		"  Would verify them on entire\n"+
		"  Would delete 128 checkpoint refs and the entire/checkpoints/v1 branch from origin (github.com/o/r) only with --remove\n",
		buf.String())

	in.BackendEnvOverride = true
	in.Remotes[0].Inventory = emptyInventory()
	plan = planCheckpointSync(in)
	buf.Reset()
	renderDryRunSteps(&buf, in, plan)
	assert.Contains(t, buf.String(), "  Would leave the store on git-branch (ENTIRE_CHECKPOINTS_PRIMARY is set) and keep the old entire/checkpoints/v1 branch\n")
	assert.NotContains(t, buf.String(), "Would convert", "no v1 anywhere once the old remote's inventory is empty")

	in.PrimaryIsRefs = true
	plan = planCheckpointSync(in)
	buf.Reset()
	renderDryRunSteps(&buf, in, plan)
	assert.Contains(t, buf.String(), "  Would queue every local checkpoint ref for push\n", "requeue is planned whenever data moves")
	assert.NotContains(t, buf.String(), "Would convert")

	// A git-refs repo with a leftover local v1 branch: the conversion is a
	// listed step, not a surprise at run time.
	in.HasLocalV1 = true
	plan = planCheckpointSync(in)
	buf.Reset()
	renderDryRunSteps(&buf, in, plan)
	assert.Contains(t, buf.String(), "  Would convert 142 checkpoints to refs\n")
	assert.True(t, plan.hasStep(stepConvertV1))
	assert.False(t, plan.hasStep(stepConvertBackend))
	assert.True(t, strings.HasPrefix(plan.Reason, "The entire/checkpoints/v1 branch is converted to refs first. "), plan.Reason)
}

func TestRenderSyncCelebration(t *testing.T) {
	t.Parallel()

	v := checkpointValue{Checkpoints: 142, Sessions: 37, Agents: []string{"Claude Code", "Codex"}, Tokens: 18_400_000, TokensSampled: 142}
	var buf bytes.Buffer
	renderSyncCelebration(&buf, newStatusStyles(&buf), testEntireRemote, true, v, "")
	got := buf.String()
	assert.True(t, strings.HasPrefix(got, "\n✓ Your checkpoints now live on Entire.\n\n"), got)
	assert.Contains(t, got, "  Checkpoints  142\n  Sessions     37\n  Agents       Claude Code, Codex\n  Agent work   18.4M tokens across your 142 checkpoints\n")
	assert.Contains(t, got, "\n  From now on, every commit's session history syncs to entire automatically,\n  whichever remote you push your code to.\n")
	assert.Contains(t, got, "  Next\n")
	assert.Contains(t, got, "entire checkpoint list")
	assert.Contains(t, got, `entire search "<what you did>"`)
	assert.Contains(t, got, "entire status")
	assert.NotContains(t, got, "Older copies")

	buf.Reset()
	renderSyncCelebration(&buf, newStatusStyles(&buf), "fork", false, v, formatRemovalDeclined("origin"))
	got = buf.String()
	assert.True(t, strings.HasPrefix(got, "\n✓ Your checkpoints now live on fork.\n"), got)
	assert.Contains(t, got, "syncs to fork with your pushes to it.")
	assert.Contains(t, got, "A push to any other remote carries your code but no session history.")
	assert.NotContains(t, got, "whichever remote you push your code to")
	assert.True(t, strings.HasSuffix(got, "\n  Older copies stay on origin. Remove them any time with:  entire checkpoint migrate --remove\n"), got)
}

func TestRenderAlreadyHome(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	renderAlreadyHome(&buf, newStatusStyles(&buf), "entire (your Entire remote)", checkpointValue{Checkpoints: 3, Sessions: 2})
	assert.Equal(t, "\n✓ Checkpoints live on entire (your Entire remote).\n\n  Checkpoints  3\n  Sessions     2\n", buf.String())

	buf.Reset()
	renderAlreadyHome(&buf, newStatusStyles(&buf), "origin", checkpointValue{})
	assert.Equal(t, "\n✓ Checkpoints live on origin.\n", buf.String())
}

func TestJoinClauses(t *testing.T) {
	t.Parallel()

	assert.Empty(t, joinClauses(nil))
	assert.Equal(t, "a", joinClauses([]string{"a"}))
	assert.Equal(t, "a and b", joinClauses([]string{"a", "b"}))
	assert.Equal(t, "a, b, and c", joinClauses([]string{"a", "b", "c"}))
}
