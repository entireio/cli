package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func TestRepoRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		repo coreapi.Repo
		want string
	}{
		{
			name: "host and path produce an entire:// URL",
			repo: coreapi.Repo{
				ClusterHost: coreapi.NewOptString("aws-us-east-2.entire.io"),
				Path:        coreapi.NewOptString("acme/web"),
			},
			want: "entire://aws-us-east-2.entire.io/acme/web",
		},
		{
			name: "leading slash on path is not doubled",
			repo: coreapi.Repo{
				ClusterHost: coreapi.NewOptString("aws-us-east-2.entire.io"),
				Path:        coreapi.NewOptString("/acme/web"),
			},
			want: "entire://aws-us-east-2.entire.io/acme/web",
		},
		{
			name: "missing host yields no URL",
			repo: coreapi.Repo{Path: coreapi.NewOptString("acme/web")},
			want: "",
		},
		{
			name: "missing path yields no URL",
			repo: coreapi.Repo{ClusterHost: coreapi.NewOptString("aws-us-east-2.entire.io")},
			want: "",
		},
		{
			name: "blank coordinates yield no URL",
			repo: coreapi.Repo{
				ClusterHost: coreapi.NewOptString("  "),
				Path:        coreapi.NewOptString(""),
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := repoRemoteURL(tt.repo); got != tt.want {
				t.Errorf("repoRemoteURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseVisibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    coreapi.SetRepoVisibilityInputBodyVisibility
		wantErr bool
	}{
		{in: "public", want: coreapi.SetRepoVisibilityInputBodyVisibilityPublic},
		{in: "private", want: coreapi.SetRepoVisibilityInputBodyVisibilityPrivate},
		{in: "Public", wantErr: true}, // case-sensitive; the wire enum is lowercase
		{in: "", wantErr: true},
		{in: "world-readable", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseVisibility(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseVisibility(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVisibility(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseVisibility(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRepoDetailRow(t *testing.T) {
	t.Parallel()

	t.Run("includes the entire:// remote", func(t *testing.T) {
		t.Parallel()
		row := repoDetailRow(coreapi.Repo{
			ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
			Name:            "web",
			OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
			ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
			Path:            coreapi.NewOptString("acme/web"),
			State:           coreapi.NewOptString("active"),
		})
		if len(row) != len(repoDetailColumns) {
			t.Fatalf("row has %d cells, want %d (one per column)", len(row), len(repoDetailColumns))
		}
		if want := "entire://aws-us-east-2.entire.io/acme/web"; row[len(row)-1] != want {
			t.Errorf("REMOTE cell = %q, want %q", row[len(row)-1], want)
		}
	})

	t.Run("shows - when the remote is not yet resolvable", func(t *testing.T) {
		t.Parallel()
		row := repoDetailRow(coreapi.Repo{
			ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
			Name:            "web",
			OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
			ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
		})
		if row[len(row)-1] != "-" {
			t.Errorf("REMOTE cell = %q, want %q", row[len(row)-1], "-")
		}
	})
}

func TestRepoCreateOutput_StampsRemote(t *testing.T) {
	t.Parallel()
	repo := &coreapi.Repo{
		ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
		Name:            "web",
		OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
		ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
		Path:            coreapi.NewOptString("acme/web"),
	}
	out, err := repoCreateOutput(repo)
	if err != nil {
		t.Fatalf("repoCreateOutput() error = %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if want := "entire://aws-us-east-2.entire.io/acme/web"; got["remote"] != want {
		t.Errorf("remote = %v, want %q", got["remote"], want)
	}
	// The original repo fields must survive the round-trip alongside the
	// synthesized remote.
	if got["id"] != repo.ID {
		t.Errorf("id = %v, want %q", got["id"], repo.ID)
	}
	if got["name"] != repo.Name {
		t.Errorf("name = %v, want %q", got["name"], repo.Name)
	}
}

func TestRepoCreateOutput_PreservesServerProvidedRemote(t *testing.T) {
	t.Parallel()
	// A server-provided `remote` (here via additional properties, the same
	// path a future first-class field would surface through) must win over
	// the synthesized one — synthesis only fills a gap.
	const serverRemote = "entire://override.entire.io/server/value"
	repo := &coreapi.Repo{
		ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
		Name:            "web",
		OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
		ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
		Path:            coreapi.NewOptString("acme/web"),
		AdditionalProps: coreapi.RepoAdditional{
			"remote": jx.Raw(`"` + serverRemote + `"`),
		},
	}
	out, err := repoCreateOutput(repo)
	if err != nil {
		t.Fatalf("repoCreateOutput() error = %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got["remote"] != serverRemote {
		t.Errorf("remote = %v, want server-provided %q", got["remote"], serverRemote)
	}
}

func TestRepoCreateOutput_NilRepoErrors(t *testing.T) {
	t.Parallel()
	// Defends the contract rather than a real path (the caller only passes a
	// repo after a nil-error create): a nil pointer must return an error, not
	// panic on the later dereference.
	if _, err := repoCreateOutput(nil); err == nil {
		t.Fatal("expected an error for a nil repo, got nil")
	}
}

func TestRepoCreateOutput_OmitsRemoteWhenUnresolvable(t *testing.T) {
	t.Parallel()
	// A still-provisioning repo may lack a path; omit the field rather than
	// emit a half-formed URL.
	repo := &coreapi.Repo{
		ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
		Name:            "web",
		OwningProjectId: "01KS6KFJR2XS6PZ188MVYE07AP",
		ClusterHost:     coreapi.NewOptString("aws-us-east-2.entire.io"),
	}
	out, err := repoCreateOutput(repo)
	if err != nil {
		t.Fatalf("repoCreateOutput() error = %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if _, ok := got["remote"]; ok {
		t.Errorf("expected no remote field, got %v", got["remote"])
	}
}

func TestParseObjectFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    coreapi.CreateRepoInputBodyObjectFormat
		wantErr bool
	}{
		{in: "sha1", want: coreapi.CreateRepoInputBodyObjectFormatSHA1},
		{in: "sha256", want: coreapi.CreateRepoInputBodyObjectFormatSHA256},
		{in: "SHA1", wantErr: true}, // case-sensitive; the wire enum is lowercase
		{in: "", wantErr: true},
		{in: "sha512", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseObjectFormat(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseObjectFormat(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseObjectFormat(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseObjectFormat(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// serveRepoCreate stands up a fake control plane answering POST /repos with a
// minimal created repo and delivers each raw request body on the returned
// channel. Points the active-context client seam at the server.
func serveRepoCreate(t *testing.T) <-chan []byte {
	t.Helper()
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read create body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		bodyCh <- raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := printJSON(w, &coreapi.Repo{
			ID:              "01KS6KFJR2XS6PZ188MVYE07AN",
			Name:            "web",
			OwningProjectId: testProjectULID,
		}); err != nil {
			t.Errorf("encode create response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return bodyCh
}

// execRepoCreate runs `repo create web --project <ulid>` under a parent
// carrying the control-plane persistent flags, mirroring execRepoList.
func execRepoCreate(t *testing.T, args ...string) error {
	t.Helper()
	parent := &cobra.Command{Use: "repo"}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoCreateCmd())
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs(append([]string{"create", "web", "--project", testProjectULID}, args...))
	return parent.ExecuteContext(t.Context())
}

// TestRepoCreate_ObjectFormat pins the --object-format wiring: a set flag
// reaches the wire body, an unset flag leaves the field to the server
// default, and an invalid value fails fast before any request is sent.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_ObjectFormat(t *testing.T) {
	t.Run("--object-format sha256 is sent on the wire", func(t *testing.T) {
		bodyCh := serveRepoCreate(t)
		require.NoError(t, execRepoCreate(t, "--object-format", "sha256"))
		var body map[string]any
		require.NoError(t, json.Unmarshal(<-bodyCh, &body))
		require.Equal(t, "sha256", body["objectFormat"])
	})

	t.Run("unset flag omits the field so the server default applies", func(t *testing.T) {
		bodyCh := serveRepoCreate(t)
		require.NoError(t, execRepoCreate(t))
		var body map[string]any
		require.NoError(t, json.Unmarshal(<-bodyCh, &body))
		require.NotContains(t, body, "objectFormat")
	})

	t.Run("an invalid value fails before any request is sent", func(t *testing.T) {
		bodyCh := serveRepoCreate(t)
		err := execRepoCreate(t, "--object-format", "sha512")
		require.ErrorContains(t, err, "sha512")
		require.ErrorContains(t, err, "sha1")
		require.ErrorContains(t, err, "sha256")
		select {
		case raw := <-bodyCh:
			t.Fatalf("no create request expected, got body %s", raw)
		default:
		}
	})
}

// testProjectULID is a syntactically valid ULID so `repo list <project>` skips
// the by-name resolution round-trip and goes straight to ListProjectRepos.
const testProjectULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// bulkRepos builds n minimal project repos named <prefix>-0000…, for tests
// that need to cross the fetch budget.
func bulkRepos(prefix string, n int) []coreapi.Repo {
	repos := make([]coreapi.Repo, 0, n)
	for i := range n {
		repos = append(repos, coreapi.Repo{
			ID:              fmt.Sprintf("%s-%04d", prefix, i),
			Name:            fmt.Sprintf("%s-%04d", prefix, i),
			OwningProjectId: testProjectULID,
		})
	}
	return repos
}

// serveProjectRepos stands up a fake control-plane serving keyset-paginated
// GET /projects/{id}/repos: each call answers with the page addressed by the
// pageToken query param ("" is the first page). Every request is delivered on
// the returned channel (buffered to the page count so the handler never
// blocks). Points the active-context client seam at the server.
func serveProjectRepos(t *testing.T, pages []coreapi.ListProjectReposOutputBody) <-chan recordedRequest {
	t.Helper()
	tokenToPage := make(map[string]coreapi.ListProjectReposOutputBody, len(pages))
	for i, p := range pages {
		token := ""
		if i > 0 {
			token = pages[i-1].NextPageToken.Or("")
			require.NotEmpty(t, token, "every page but the last needs a NextPageToken linking to the next one")
		}
		tokenToPage[token] = p
	}
	recCh := make(chan recordedRequest, len(pages))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/projects/"+testProjectULID+"/repos" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		page, ok := tokenToPage[r.URL.Query().Get("pageToken")]
		if !ok {
			t.Errorf("unexpected pageToken %q", r.URL.Query().Get("pageToken"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := printJSON(w, &page); err != nil {
			t.Errorf("encode repos response: %v", err)
		}
		recCh <- recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return recCh
}

// execRepoList runs `repo list <project>` under a parent carrying the
// control-plane persistent flags, mirroring execMirrorList.
func execRepoList(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	parent := &cobra.Command{Use: "repo"}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoListCmd())
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs(append([]string{"list", testProjectULID}, args...))
	err = parent.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// TestRepoList_FetchBudget pins the bounded cursor walk on `repo list`: by
// default at most coreListFetchBudget entries are fetched with a stderr
// disclosure, --limit bounds the fetch directly (this list has no local
// filters or sort), and --all lifts the bound.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoList_FetchBudget(t *testing.T) {
	t.Run("the default fetch budget stops the walk and discloses the partial window", func(t *testing.T) {
		recCh := serveProjectRepos(t, []coreapi.ListProjectReposOutputBody{
			{Repos: bulkRepos("bulk", 1000), NextPageToken: coreapi.NewOptString("p2")},
			{Repos: bulkRepos("tail", 1)},
		})
		stdout, stderr, err := execRepoList(t)
		require.NoError(t, err)
		require.NotContains(t, stdout, "tail-0000", "the walk must stop at the budget")
		<-recCh
		select {
		case rec := <-recCh:
			t.Fatalf("no second page request expected, got one with pageToken=%q", rec.query.Get("pageToken"))
		default:
		}
		require.Contains(t, stderr, "first 1000")
		require.Contains(t, stderr, "--all")
	})

	t.Run("--all walks past the budget and prints no note", func(t *testing.T) {
		serveProjectRepos(t, []coreapi.ListProjectReposOutputBody{
			{Repos: bulkRepos("bulk", 1000), NextPageToken: coreapi.NewOptString("p2")},
			{Repos: bulkRepos("tail", 1)},
		})
		stdout, stderr, err := execRepoList(t, "--all")
		require.NoError(t, err)
		require.Contains(t, stdout, "tail-0000", "--all fetches the full list")
		require.NotContains(t, stderr, "--all", "a complete walk needs no note")
	})

	t.Run("--limit bounds the fetch directly and prints no note", func(t *testing.T) {
		// No local filters or sort on this list, so --limit N never needs
		// entries beyond the first N: the walk stops early and, because the
		// user asked for the cap, no partial-window note is printed.
		recCh := serveProjectRepos(t, []coreapi.ListProjectReposOutputBody{
			{Repos: bulkRepos("page1", 2), NextPageToken: coreapi.NewOptString("p2")},
			{Repos: bulkRepos("page2", 2)},
		})
		stdout, stderr, err := execRepoList(t, "--limit", "2")
		require.NoError(t, err)
		require.Contains(t, stdout, "page1-0001")
		require.NotContains(t, stdout, "page2-0000", "the walk stops once --limit is satisfied")
		<-recCh
		select {
		case rec := <-recCh:
			t.Fatalf("no second page request expected, got one with pageToken=%q", rec.query.Get("pageToken"))
		default:
		}
		require.NotContains(t, stderr, "--all", "an explicit --limit is not a surprise; no note")
	})
}

// TestRepoList_PageMode pins the single-page cursor passthrough: --page-size /
// --page-token make exactly one request, --json wraps rows in an envelope
// carrying nextPageToken, the table view prints a resume hint on stderr, and
// page mode excludes the walk flags.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoList_PageMode(t *testing.T) {
	t.Run("--page-size makes one request and passes the size through", func(t *testing.T) {
		recCh := serveProjectRepos(t, []coreapi.ListProjectReposOutputBody{
			{Repos: bulkRepos("page1", 2), NextPageToken: coreapi.NewOptString("p2")},
			{Repos: bulkRepos("page2", 2)},
		})
		stdout, stderr, err := execRepoList(t, "--page-size", "2")
		require.NoError(t, err)
		rec := <-recCh
		require.Equal(t, "2", rec.query.Get("pageSize"))
		select {
		case rec := <-recCh:
			t.Fatalf("page mode must make exactly one request, got a second with pageToken=%q", rec.query.Get("pageToken"))
		default:
		}
		require.Contains(t, stdout, "page1-0001")
		require.NotContains(t, stdout, "page2-0000")
		require.Contains(t, stderr, "--page-token p2", "the table view hints how to resume")
	})

	t.Run("--json page mode emits the envelope with nextPageToken", func(t *testing.T) {
		serveProjectRepos(t, []coreapi.ListProjectReposOutputBody{
			{Repos: bulkRepos("page1", 2), NextPageToken: coreapi.NewOptString("p2")},
			{Repos: bulkRepos("page2", 2)},
		})
		stdout, _, err := execRepoList(t, "--json", "--page-size", "2")
		require.NoError(t, err)
		var envelope struct {
			Items         []coreapi.Repo `json:"items"`
			NextPageToken string         `json:"nextPageToken"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &envelope))
		require.Len(t, envelope.Items, 2)
		require.Equal(t, "p2", envelope.NextPageToken)
	})

	t.Run("page mode excludes the walk flags", func(t *testing.T) {
		serveProjectRepos(t, nil)
		for _, combo := range [][]string{
			{"--page-size", "5", "--all"},
			{"--page-token", "p2", "--all"},
			{"--page-size", "5", "--limit", "3"},
			{"--page-token", "p2", "--limit", "3"},
		} {
			_, _, err := execRepoList(t, combo...)
			require.Error(t, err, "combo %v must be rejected", combo)
		}
	})
}

// TestRepoList_GroupedFlagHelp pins the grouped help layout on `repo list`:
// navigation flags then formatting flags (this list has no filter/sort).
func TestRepoList_GroupedFlagHelp(t *testing.T) {
	stdout, _, err := execRepoList(t, "--help")
	require.NoError(t, err)
	// Anchor past the Long text (which mentions flags by name) so the order
	// assertions see only the flag sections.
	idx := strings.Index(stdout, "Navigation Flags:")
	require.GreaterOrEqual(t, idx, 0, "expected a Navigation Flags section")
	requireOrder(t, stdout[idx:],
		"Navigation Flags:", "--all", "--limit", "--page-size", "--page-token",
		"Formatting Flags:", "--json", "--no-pager",
	)
	require.NotContains(t, stdout, "Filtering & Sorting Flags:")
}
