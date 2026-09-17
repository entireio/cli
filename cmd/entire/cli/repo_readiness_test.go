package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Not parallel: runCoreCmd replaces the process-global client seam.
func TestRepoGetAuthoritativeSnapshot(t *testing.T) {
	for _, state := range []string{"provisioning", "active", "failed", "", "future"} {
		t.Run(state, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.Method != http.MethodGet || r.URL.Query().Get("authoritative") != "true" {
					t.Errorf("expected authoritative GET, got %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":%q,"provisionReason":"max retries exhausted","capabilities":{"canManage":false,"canPush":false,"canPull":true}}`, testDeleteULID, testProjectULID, state)
			}))
			defer srv.Close()
			for _, args := range [][]string{{testDeleteULID}, {testDeleteULID, "--json"}} {
				out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, append(args, "--authoritative")...)
				require.NoError(t, err)
				require.Contains(t, out, "max retries exhausted")
				if len(args) > 1 {
					var obj map[string]any
					require.NoError(t, json.Unmarshal([]byte(out), &obj))
					require.Equal(t, state, obj["state"])
				}
			}
			require.EqualValues(t, 2, reads.Load(), "one snapshot per invocation")
		})
	}
}

func TestRepoCreateReadinessFlags(t *testing.T) {
	// Not parallel: shared client seam.
	for _, tc := range []struct {
		name        string
		args        []string
		wantErr     bool
		wantCreates int32
	}{
		{name: "no wait", args: []string{"--no-wait"}, wantCreates: 1},
		{name: "zero", args: []string{"--wait-timeout=0"}, wantErr: true},
		{name: "negative", args: []string{"--wait-timeout=-1s"}, wantErr: true},
		{name: "invalid", args: []string{"--wait-timeout=oops"}, wantErr: true},
		{name: "no wait still validates", args: []string{"--no-wait", "--wait-timeout=0"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected %s", r.Method)
				}
				creates.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, testDeleteULID, testProjectULID)
			}))
			defer srv.Close()
			args := append([]string{"web", "--project", testProjectULID, "--json"}, tc.args...)
			out, stderr, err := runCoreCmd(t, func() *cobra.Command {
				cmd := newRepoCreateCmd()
				// The real root delegates error output to main, not Cobra.
				cmd.SilenceErrors = true
				return cmd
			}, srv.URL, args...)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Contains(t, out, testDeleteULID)
				require.Contains(t, stderr, "unconfirmed")
			}
			require.Equal(t, tc.wantCreates, creates.Load())
		})
	}
}

// Not parallel: replaces the repository cadence seam.
func TestRepoCreateReadinessResults(t *testing.T) {
	prev := repoPollInterval
	repoPollInterval = time.Millisecond
	t.Cleanup(func() { repoPollInterval = prev })
	for _, tc := range []struct {
		name, initial, final string
		pollStatus, polls    int
		wantErr              bool
		foreign, mismatched  bool
	}{
		{name: "already active", initial: "active", final: "active", polls: 1},
		{name: "active creation but region still provisioning", initial: "active", final: "active", polls: 3},
		{name: "active creation but failed region", initial: "active", final: "failed", polls: 1, wantErr: true},
		{name: "active creation but unavailable region", initial: "active", pollStatus: 503, polls: 6, wantErr: true},
		{name: "active creation but foreign snapshot", initial: "active", final: "active", foreign: true, polls: 1, wantErr: true},
		{name: "active creation but missing lifecycle", initial: "active", final: "", polls: 1, wantErr: true},
		{name: "multiple pending", initial: "provisioning", final: "active", polls: 3},
		{name: "failed snapshot with restored access", initial: "provisioning", final: "failed", polls: 1, wantErr: true},
		{name: "old server missing state", initial: "", wantErr: true},
		{name: "unknown initial", initial: "future", wantErr: true},
		{name: "missing on poll", initial: "provisioning", final: "", polls: 1, wantErr: true},
		{name: "unknown on poll", initial: "provisioning", final: "future", polls: 1, wantErr: true},
		{name: "creator access removed by cleanup", initial: "provisioning", pollStatus: 403, polls: 2, wantErr: true},
		{name: "foreign registry removed by cleanup", initial: "provisioning", pollStatus: 404, polls: 2, wantErr: true},
		{name: "routing unavailable", initial: "provisioning", pollStatus: 503, polls: 6, wantErr: true},
		{name: "missing home host", initial: "provisioning", pollStatus: 500, polls: 6, wantErr: true},
		{name: "foreign on poll", initial: "provisioning", final: "active", foreign: true, polls: 1, wantErr: true},
		{name: "mismatched ID", initial: "provisioning", final: "active", mismatched: true, polls: 1, wantErr: true},
		{name: "rejected parameter", initial: "provisioning", pollStatus: 422, polls: 2, wantErr: true},
		{name: "rate limited", initial: "provisioning", pollStatus: 429, polls: 6, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, asJSON := range []bool{false, true} {
				var posts, gets atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					state := tc.initial
					switch r.Method {
					case http.MethodPost:
						posts.Add(1)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusCreated)
						fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":%q,"commitToken":"tok-abc","clusterHost":"cell.example","path":"/et/project/web","capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, testDeleteULID, testProjectULID, state)
						return
					case http.MethodGet:
						n := gets.Add(1)
						if r.URL.Query().Get("authoritative") != "true" {
							t.Error("missing authoritative query")
						}
						if tc.pollStatus == http.StatusTooManyRequests {
							http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
							return
						}
						if tc.pollStatus != 0 {
							w.Header().Set("Content-Type", "application/problem+json")
							w.WriteHeader(tc.pollStatus)
							detail := map[int]string{403: "permission denied", 404: "repo not found", 503: "repository lifecycle unavailable on this core", 500: `cluster jurisdiction "eu" has no auth host wired on this core`}[tc.pollStatus]
							if tc.pollStatus == 422 {
								fmt.Fprint(w, `{"detail":"validation failed","errors":[{"message":"unknown query parameter","location":"query.authoritative"}]}`)
							} else {
								fmt.Fprintf(w, `{"status":%d,"title":%q,"detail":%q}`, tc.pollStatus, http.StatusText(tc.pollStatus), detail)
							}
							return
						}
						state = "provisioning"
						if int(n) >= tc.polls {
							state = tc.final
						}
					default:
						t.Errorf("unexpected request: %s", r.Method)
					}
					w.Header().Set("Content-Type", "application/json")
					// Real enrichment can omit remote coordinates and owning project after cleanup.
					snapshotID := testDeleteULID
					if tc.mismatched {
						snapshotID = testProjectULID
					}
					fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":"","provider":"entire","state":%q,"provisionReason":"max retries exhausted","foreign":%t,"capabilities":{"canManage":false,"canPush":false,"canPull":true}}`, snapshotID, state, tc.foreign)
				}))
				args := []string{"web", "--project", testProjectULID}
				if asJSON {
					args = append(args, "--json")
				}
				out, stderr, err := runCoreCmd(t, func() *cobra.Command {
					cmd := newRepoCreateCmd()
					// The real root delegates error output to main, not Cobra.
					cmd.SilenceErrors = true
					return cmd
				}, srv.URL, args...)
				srv.Close()
				if tc.wantErr {
					require.Error(t, err)
					require.Contains(t, stderr, "creation succeeded")
					require.Contains(t, stderr, "repo view "+testDeleteULID)
					require.Contains(t, stderr, "support")
					require.Contains(t, stderr, "--authoritative")
					if tc.pollStatus == 422 {
						require.Contains(t, stderr, "query.authoritative")
						require.Contains(t, stderr, "unknown query parameter")
						require.Contains(t, stderr, "--no-wait")
					}
					if tc.pollStatus == http.StatusForbidden {
						var statusErr *coreapi.ErrorModelStatusCode
						require.ErrorAs(t, err, &statusErr)
						var silent *SilentError
						require.ErrorAs(t, err, &silent)
						// Model main's rendering gate: removing runCoreClient's guard
						// must produce a second copy of the problem detail here.
						if !errors.As(err, &silent) {
							stderr += fmt.Sprintln(renderCoreError(err))
						}
						require.Equal(t, 1, strings.Count(stderr, "permission denied"))
					}
				} else {
					require.NoError(t, err)
				}
				require.Contains(t, out, testDeleteULID)
				require.Contains(t, out, "entire://cell.example/et/project/web")
				require.EqualValues(t, 1, posts.Load())
				require.EqualValues(t, tc.polls, gets.Load())
				if asJSON {
					var obj map[string]any
					require.NoError(t, json.Unmarshal([]byte(out), &obj))
					require.Equal(t, "tok-abc", obj["commitToken"])
					require.Equal(t, testProjectULID, obj["owningProjectId"])
					require.Equal(t, testDeleteULID, obj["id"])
					expectedState := tc.initial
					if tc.polls > 0 && tc.pollStatus == 0 && !tc.mismatched {
						expectedState = tc.final
					}
					require.Equal(t, expectedState, obj["state"])
				}
			}
		})
	}
}

type repoReadFunc func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error)

func (f repoReadFunc) GetRepo(ctx context.Context, p coreapi.GetRepoParams) (*coreapi.Repo, error) {
	return f(ctx, p)
}

func TestAwaitRepoActive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		foreign bool
		state   string
	}{
		{name: "foreign registry", foreign: true},
		{name: "foreign active is not authoritative", foreign: true, state: "active"},
		{name: "unknown", state: "future"},
		{name: "missing"},
		{name: "failed", state: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(tc.state), Foreign: coreapi.NewOptBool(tc.foreign)}
			calls := 0
			err := awaitRepoActive(t.Context(), repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
				calls++
				return nil, errors.New("unexpected")
			}), result, nil)
			require.Error(t, err)
			require.Zero(t, calls)
			require.Equal(t, testDeleteULID, result.ID)
		})
	}
	t.Run("errors reset after successful read", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
			calls := 0
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			err := awaitRepoActive(ctx, repoReadFunc(func(_ context.Context, p coreapi.GetRepoParams) (*coreapi.Repo, error) {
				calls++
				require.True(t, p.Authoritative.Or(false))
				if calls != 3 && calls != 6 {
					return nil, errors.New("temporary read error")
				}
				state := "provisioning"
				if calls == 6 {
					state = "active"
				}
				return &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(state)}, nil
			}), result, nil)
			require.NoError(t, err)
			require.Equal(t, 6, calls)
			require.Equal(t, "active", result.State.Or(""))
		})
	})
	for _, inFlight := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline in flight %v", inFlight), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
				calls := 0
				err := awaitRepoActive(ctx, repoReadFunc(func(ctx context.Context, _ coreapi.GetRepoParams) (*coreapi.Repo, error) {
					calls++
					if inFlight {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return result, nil
				}), result, nil)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, "provisioning", result.State.Or(""))
				if inFlight {
					require.Equal(t, 1, calls)
				} else {
					// The lower jitter bounds allow a third probe at 4.8s.
					require.GreaterOrEqual(t, calls, 2)
					require.LessOrEqual(t, calls, 3)
				}
			})
		})
	}
	t.Run("canceled before polling", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
		err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
			t.Error("read after cancellation")
			return nil, errors.New("unexpected read")
		}), result, nil)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// Not parallel: replaces activeCoreClient. The HTTP request is canceled only
// after POST has succeeded, pinning both output preservation and error identity
// used by main's signal-exit path.
func TestRepoCreateInterruptedAfterCreation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout %v", timeout), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var posts, gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","commitToken":"tok-abc","clusterHost":"cell.example","path":"/et/project/web","capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, testDeleteULID, testProjectULID)
					return
				}
				gets.Add(1)
				if !timeout {
					cancel()
				}
				<-r.Context().Done()
			}))
			defer srv.Close()
			prev := activeCoreClient
			activeCoreClient = func(context.Context) (*coreapi.Client, error) { return coreapi.NewWithBearer(srv.URL, "tok") }
			t.Cleanup(func() { activeCoreClient = prev })
			cmd := newRepoCreateCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			args := []string{"web", "--project", testProjectULID, "--json"}
			if timeout {
				args = append(args, "--wait-timeout=1s")
			}
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			if timeout {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			var obj map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &obj))
			require.Equal(t, testDeleteULID, obj["id"])
			require.Equal(t, "entire://cell.example/et/project/web", obj["remote"])
			require.Contains(t, stderr.String(), "creation succeeded")
			require.EqualValues(t, 1, posts.Load())
			require.EqualValues(t, 1, gets.Load())
		})
	}
}

func TestReportRepoCreationNoWaitReason(t *testing.T) {
	t.Parallel()
	cmd := newRepoCreateCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("failed"), ProvisionReason: coreapi.NewOptString("max retries exhausted")}
	require.NoError(t, reportRepoCreation(cmd, result, true, nil))
	require.Contains(t, out.String(), "max retries exhausted")
	require.Contains(t, stderr.String(), "unconfirmed")
}

func TestRepoCreateAlreadyReportedCoreError(t *testing.T) {
	t.Parallel()
	statusErr := &coreapi.ErrorModelStatusCode{StatusCode: http.StatusForbidden,
		Response: coreapi.ErrorModel{Detail: coreapi.NewOptString("permission denied")}}
	original := fmt.Errorf("command failed: %w", NewSilentError(errors.Join(statusErr, context.Canceled)))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runCoreClient(cmd, func(context.Context) (*coreapi.Client, error) { return &coreapi.Client{}, nil },
		func(context.Context, *coreapi.Client) error { return original })
	var silent *SilentError
	require.ErrorAs(t, err, &silent)
	require.ErrorIs(t, err, statusErr)
	require.ErrorIs(t, err, context.Canceled)
	// Display callers (notably the mirror wizard) still need a plain message.
	rendered := renderCoreError(original)
	require.EqualError(t, rendered, "permission denied")
	require.NotErrorAs(t, rendered, &silent)
}

// Not parallel: replaces the random-sample seam to pin the jittered schedule.
func TestAwaitRepoActiveBackoff(t *testing.T) {
	previous := repoPollRandom
	samples := []float64{0, 0.5, 1, 0, 0.5, 1}
	next := 0
	repoPollRandom = func() float64 {
		sample := samples[next%len(samples)]
		next++
		return sample
	}
	t.Cleanup(func() { repoPollRandom = previous })
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var probes []time.Duration
		result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
			probes = append(probes, time.Since(start))
			state := "provisioning"
			if len(probes) == 7 {
				state = "active"
			}
			return &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(state)}, nil
		}), result, nil)
		require.NoError(t, err)
		require.Zero(t, probes[0], "the first probe must be immediate")
		// Explicit values pin the production backoff and both jitter extremes,
		// independently of repoPollDelay's implementation.
		for i, want := range []time.Duration{
			1600 * time.Millisecond, 4 * time.Second, 9600 * time.Millisecond,
			12800 * time.Millisecond, 30 * time.Second, 36 * time.Second,
		} {
			require.Equal(t, want, probes[i+1]-probes[i])
		}
		require.Equal(t, 6, next, "each sleep gets a fresh jitter sample")
	})
}

func TestAwaitRepoActiveErrorBudget(t *testing.T) {
	t.Parallel()
	for _, status := range []int{403, 404, 408, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls := 0
				problem := &coreapi.ErrorModelStatusCode{StatusCode: status}
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
				defer cancel()
				err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
					calls++
					return nil, problem
				}), result, nil)
				require.ErrorIs(t, err, problem)
				if status == 403 || status == 404 {
					require.Equal(t, 2, calls)
					require.LessOrEqual(t, time.Since(start), 10*time.Second)
				} else {
					require.GreaterOrEqual(t, calls, 5)
					require.LessOrEqual(t, calls, 6)
					require.LessOrEqual(t, time.Since(start), time.Minute)
				}
			})
		})
	}
}

func TestRepoCreateMirrorReadinessFlags(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"-1s", "oops"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			for _, constructor := range []func() *cobra.Command{newRepoCreateCmd, newRepoMirrorAddCmd} {
				cmd := constructor()
				cmd.RunE = func(*cobra.Command, []string) error { t.Error("invalid timeout reached RunE"); return nil }
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				timeoutFlag := "--timeout="
				if cmd.Flags().Lookup("wait-timeout") != nil {
					timeoutFlag = "--wait-timeout="
				}
				args := []string{"foo", timeoutFlag + value}
				if cmd.Flags().Lookup("project") != nil {
					args = append(args, "--project", testProjectULID)
				}
				cmd.SetArgs(args)
				require.Error(t, cmd.ExecuteContext(t.Context()))
				require.False(t, cmd.SilenceUsage, "invalid flags are usage errors on both commands")
			}
		})
	}
}

// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoViewAuthoritativeFlag(t *testing.T) {
	// hint marks the failures a plain read could still answer. Every other
	// status is a statement about the repository, so retrying without the
	// readiness check changes nothing and the hint must stay away.
	for _, tc := range []struct {
		name, flag, query, body string
		status                  int
		hint                    bool
	}{
		{name: "default"},
		{name: "explicit", flag: "--authoritative=true", query: "true"},
		{name: "plain", flag: "--authoritative=false"},
		{name: "unavailable", flag: "--authoritative", query: "true", status: 503, hint: true},
		{name: "rejected parameter", flag: "--authoritative", query: "true", status: 422, hint: true,
			body: `{"detail":"repository read failed","errors":[{"message":"unknown query parameter","location":"query.authoritative"}]}`},
		{name: "unrelated validation", flag: "--authoritative", query: "true", status: 422,
			body: `{"detail":"repository read failed","errors":[{"message":"expected a ULID","location":"path.repo_id"}]}`},
		{name: "forbidden", flag: "--authoritative", query: "true", status: 403},
		{name: "missing", flag: "--authoritative", query: "true", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, tc.query, r.URL.Query().Get("authoritative"))
				assert.Equal(t, tc.query != "", r.URL.Query().Has("authoritative"))
				if tc.status != 0 {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(tc.status)
					if tc.body != "" {
						fmt.Fprint(w, tc.body)
					} else {
						fmt.Fprintf(w, `{"status":%d,"detail":"repository read failed"}`, tc.status)
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"capabilities":{"canManage":false,"canPush":false,"canPull":true}}`, testDeleteULID, testProjectULID)
			}))
			defer srv.Close()
			args := []string{testDeleteULID}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			_, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, args...)
			if tc.status != 0 {
				require.Error(t, err)
				var silent *SilentError
				if !errors.As(err, &silent) {
					stderr += err.Error()
				}
				// The server's own message reaches the user either way.
				require.Contains(t, stderr, "repository read failed")
				if tc.hint {
					require.Contains(t, stderr, "entire repo view "+testDeleteULID+" to inspect")
					require.Contains(t, stderr, "without a readiness check")
				} else {
					require.NotContains(t, stderr, "readiness check")
				}
				if tc.hint && tc.status == 422 {
					require.Contains(t, stderr, "query.authoritative")
					require.Contains(t, stderr, "unknown query parameter")
				}
				// The plain read is the default; naming a flag value would send
				// the user to restate one they never had to pass.
				require.NotContains(t, stderr, "--authoritative=false")
				require.NotContains(t, stderr, "--no-wait")
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, calls.Load(), "no silent fallback")
		})
	}
}

func TestAwaitRepoActiveJitterBounds(t *testing.T) {
	t.Parallel()
	// Pin both ends and the midpoint independently of the random source used
	// by production; the synctest poll test checks the actual timer schedule.
	for _, sample := range []struct {
		value float64
		want  time.Duration
	}{
		{0, 24 * time.Second}, {0.5, 30 * time.Second}, {1, 36 * time.Second},
	} {
		require.Equal(t, sample.want, repoPollDelay(30*time.Second, sample.value))
	}
}

func TestAwaitRepoActivePollingCallback(t *testing.T) {
	t.Parallel()
	for _, initial := range []string{"active", "failed", "", "future", "provisioning"} {
		t.Run(initial, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				started, reads := 0, 0
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(initial)}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
					require.Equal(t, 1, started, "progress starts before the first read")
					reads++
					state := "provisioning"
					if reads == 2 {
						state = "active"
					}
					return &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(state)}, nil
				}), result, func() { started++ })
				if initial == "provisioning" || initial == "active" {
					require.NoError(t, err)
					require.Equal(t, 1, started)
					require.Equal(t, 2, reads)
				} else {
					require.Zero(t, started)
					require.Zero(t, reads)
				}
				if initial == "" {
					require.ErrorContains(t, err, "the server did not return repository readiness information")
				}
			})
		})
	}
}

func TestAwaitRepoActiveErrorWindowBoundsInflight(t *testing.T) {
	t.Parallel()
	for _, status := range []int{404, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				problem := &coreapi.ErrorModelStatusCode{StatusCode: status}
				calls := 0
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
				defer cancel()
				err := awaitRepoActive(ctx, repoReadFunc(func(ctx context.Context, _ coreapi.GetRepoParams) (*coreapi.Repo, error) {
					calls++
					if calls == 1 {
						return nil, problem
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}), result, nil)
				require.ErrorIs(t, err, problem, "keep the observed API failure when its retry window expires")
				require.Equal(t, 2, calls)
				want := time.Minute
				if status == 404 {
					want = 10 * time.Second
				}
				require.Equal(t, want, time.Since(start))
				require.NoError(t, ctx.Err(), "error window is distinct from the command deadline")
			})
		})
	}
}

func TestAwaitRepoActiveRetainsOnlyCreationCoordinates(t *testing.T) {
	t.Parallel()
	result := &coreapi.Repo{ID: testDeleteULID, Name: "web", OwningProjectId: testProjectULID,
		State: coreapi.NewOptString("provisioning"), ProvisionReason: coreapi.NewOptString("stale"),
		ClusterHost: coreapi.NewOptString("cell.example"), Path: coreapi.NewOptString("/et/acme/web"),
		AdditionalProps: coreapi.RepoAdditional{"remote": []byte(`"entire://cell.example/et/acme/web"`)}}
	snapshot := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("active")}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) { return snapshot, nil }), result, nil)
	require.NoError(t, err)
	require.Equal(t, "web", result.Name)
	require.Equal(t, testProjectULID, result.OwningProjectId)
	require.Equal(t, "cell.example", result.ClusterHost.Or(""))
	require.Equal(t, "/et/acme/web", result.Path.Or(""))
	require.JSONEq(t, `"entire://cell.example/et/acme/web"`, string(result.AdditionalProps["remote"]))
	require.Equal(t, "active", result.State.Or(""))
	require.False(t, result.ProvisionReason.IsSet(), "do not preserve stale lifecycle enrichment")
	require.Equal(t, *snapshot, *result)
}

func TestRepoMirrorZeroTimeout(t *testing.T) {
	t.Parallel()
	cmd := newRepoMirrorAddCmd()
	called := false
	cmd.RunE = func(*cobra.Command, []string) error { called = true; return nil }
	cmd.SetArgs([]string{"foo", "--timeout=0"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.True(t, called)
}

func TestRepoPollMixedErrors(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var failures repoPollFailures
		require.False(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 404}))
		time.Sleep(2 * time.Second)
		require.False(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 503}))
		time.Sleep(9 * time.Second)
		require.False(t, failures.expired(), "transient response restores the longer window")
		time.Sleep(49 * time.Second)
		require.True(t, failures.expired(), "window still starts at the first failure")
	})
}

func TestRetainRepoAdditionalProperties(t *testing.T) {
	t.Parallel()
	result := &coreapi.Repo{AdditionalProps: coreapi.RepoAdditional{
		"commitToken": []byte(`"tok-abc"`), "future": []byte(`{"version":1}`),
	}}
	snapshot := &coreapi.Repo{AdditionalProps: coreapi.RepoAdditional{
		"future": []byte(`{"version":2}`),
	}}
	retainRepoCreation(result, snapshot)
	require.JSONEq(t, `"tok-abc"`, string(result.AdditionalProps["commitToken"]))
	require.JSONEq(t, `{"version":2}`, string(result.AdditionalProps["future"]))
}

func TestRepoReadErrorUnrelatedValidation(t *testing.T) {
	t.Parallel()
	problem := &coreapi.ErrorModelStatusCode{StatusCode: 422, Response: coreapi.ErrorModel{
		Detail: coreapi.NewOptString("invalid repository ID"),
		Errors: []coreapi.ErrorDetail{{Location: coreapi.NewOptString("path.repoId"),
			Message: coreapi.NewOptString("invalid value")}},
	}}
	require.EqualError(t, renderRepoReadError(problem), "invalid repository ID")
}

func TestRepoPollTransientThenOrdinaryError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var failures repoPollFailures
		require.False(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 503}))
		time.Sleep(2 * time.Second)
		require.True(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 404}),
			"ordinary failures still enforce the two-attempt limit")
	})
}
