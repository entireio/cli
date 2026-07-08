package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/palette"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// expertsTestRepoULID is the id the fake resolves "acme/widget" to via the
// accessible-repo list, so path assertions can reference the retargeted
// /api/v1/repos/{id}/experts route.
const expertsTestRepoULID = "0123456789ABCDEFGHJKMNPQRS"

var defaultExpertsReposBody = `{"repos":[{"id":"` + expertsTestRepoULID + `","full_name":"acme/widget"}],"from_db":true}`

type fakeExpertsClient struct {
	status     int
	body       string
	reposBody  string   // GET /api/v1/repos body; defaults to acme/widget -> expertsTestRepoULID
	reposPages []string // when set, paginated GET /api/v1/repos responses in order

	gotPath     string
	gotBody     any
	gotGetPath  string
	gotGetPaths []string
}

// Get serves the accessible-repo discovery list used to resolve owner/repo -> ULID.
func (f *fakeExpertsClient) Get(_ context.Context, path string) (*http.Response, error) {
	f.gotGetPath = path
	f.gotGetPaths = append(f.gotGetPaths, path)
	var body string
	switch {
	case len(f.reposPages) > 0:
		body = f.reposPages[0]
		f.reposPages = f.reposPages[1:]
	case f.reposBody != "":
		body = f.reposBody
	default:
		body = defaultExpertsReposBody
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (f *fakeExpertsClient) Post(_ context.Context, path string, body any) (*http.Response, error) {
	f.gotPath = path
	f.gotBody = body
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

func expertsSuccessBody() string {
	return `{
  "repo_full_name": "acme/widget",
  "scopes": ["cmd/entire/cli/experts.go"],
  "query": null,
  "branch": "main",
  "source": "db",
  "profiles": [{
    "agent_id": "codex",
    "agent_label": "Codex",
    "raw_agents": ["codex"],
    "models": ["gpt-5.4"],
    "labels": [{"name": "feature_build", "count": 2}],
    "skills": [{"name": "go-cli", "count": 2}],
    "tool_mix": [{"name": "shell", "count": 4}, {"name": "search", "count": 3}],
    "mcp_servers": [{"name": "github", "count": 1}],
    "transcript_tokens": 12000,
    "files_changed": 5,
    "last_activity_at": "2026-04-29T11:00:00.000Z",
    "session_count": 1,
    "checkpoint_count": 2,
    "step_count": 9,
    "attribution_agent_lines": 120,
    "attribution_total_committed": 140,
    "matched_files": ["cmd/entire/cli/experts.go"],
    "exact_file_matches": 1,
    "prefix_file_matches": 0,
    "sessions": [{
      "session_id": "sess-a",
      "display_name": "feat: experts provenance",
      "agent": "codex",
      "model": "gpt-5.4",
      "first_commit_author_username": "peyton",
      "last_activity_at": "2026-04-29T11:00:00.000Z",
      "checkpoint_count": 2,
      "step_count": 9,
      "attribution_agent_lines": 120,
      "attribution_total_committed": 140,
      "matched_files": ["cmd/entire/cli/experts.go"],
      "exact_file_matches": 1,
      "prefix_file_matches": 0,
      "checkpoint_ids": ["cp-1", "cp-2"]
    }]
  }]
}`
}

func TestExpertsCommandIsHiddenAndListedInLabs(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"experts"})
	if err != nil {
		t.Fatalf("find experts: %v", err)
	}
	if cmd.Name() != "experts" {
		t.Fatalf("found command %q, want experts", cmd.Name())
	}
	if !cmd.Hidden {
		t.Fatal("experts command should be hidden while in labs")
	}
	if !strings.Contains(labsOverview(), "entire experts") {
		t.Fatalf("labs overview missing experts:\n%s", labsOverview())
	}
}

func TestExpertsCommandSendsQueryAndPrintsJSON(t *testing.T) {
	fake := &fakeExpertsClient{body: expertsSuccessBody()}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "stripe webhook retry logic", "--repo", "acme/widget", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts: %v", err)
	}
	if fake.gotPath != expertsReposListPath+"/"+expertsTestRepoULID+"/experts" {
		t.Fatalf("path = %q", fake.gotPath)
	}
	if fake.gotGetPath != expertsReposListPath {
		t.Fatalf("owner/repo should resolve via GET %s, got %q", expertsReposListPath, fake.gotGetPath)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if body.Query == nil || *body.Query != "stripe webhook retry logic" {
		t.Fatalf("query body = %#v", body)
	}
	if body.Scopes != nil {
		t.Fatalf("expected nil scopes for query body, got %#v", body.Scopes)
	}

	var decoded expertsResponse
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if decoded.Profiles[0].AgentID != recapTestAgentCodex {
		t.Fatalf("agent id = %q", decoded.Profiles[0].AgentID)
	}
	if strings.Contains(out.String(), "first_commit_author_username") || strings.Contains(out.String(), "peyton") {
		t.Fatalf("JSON output should not expose human identity fields:\n%s", out.String())
	}
}

func TestExpertsCommandPrintsAgentCenteredEvidence(t *testing.T) {
	fake := &fakeExpertsClient{body: expertsSuccessBody()}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "stripe webhook retry logic", "--repo", "acme/widget"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts: %v", err)
	}
	text := out.String()
	for _, want := range []string{"Codex", "go-cli", "shell", "feat: experts provenance"} {
		if !strings.Contains(text, want) {
			t.Fatalf("human output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Peyton is an expert") {
		t.Fatalf("output should not frame humans as the headline:\n%s", text)
	}
}

func TestRenderExpertsWithStylesUsesEntirePalette(t *testing.T) {
	var resp expertsResponse
	if err := json.Unmarshal([]byte(expertsSuccessBody()), &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	styles := expertsStyles{
		colorEnabled: true,
		title:        lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true),
		agent:        lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true),
		label:        lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Info)),
		facet:        lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Blue)),
		muted:        lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)),
		file:         lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Info)),
		bullet:       lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)),
	}

	var out bytes.Buffer
	renderExpertsWithStyles(&out, resp, styles)
	text := out.String()

	for _, want := range []string{
		styles.title.Render("Agent provenance"),
		styles.agent.Render("Codex"),
		styles.label.Render("skills"),
		styles.facet.Render("go-cli") + styles.muted.Render(" (2)"),
		styles.file.Render("cmd/entire/cli/experts.go"),
		styles.bullet.Render("-"),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled output missing %q:\n%s", want, text)
		}
	}
}

func TestExpertsCommandUsesStagedFilesAsScopes(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	path := filepath.Join(dir, "billing", "webhooks", "sender.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package webhooks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runExpertsGit(t, dir, "add", "billing/webhooks/sender.go")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["billing/webhooks/sender.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "--staged", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts --staged: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "billing/webhooks/sender.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandUsesStagedDeletionsAsScopes(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	path := filepath.Join(dir, "billing", "webhooks", "sender.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package webhooks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runExpertsGit(t, dir, "add", "billing/webhooks/sender.go")
	runExpertsGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "initial")
	runExpertsGit(t, dir, "rm", "billing/webhooks/sender.go")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["billing/webhooks/sender.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "--staged", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts --staged: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "billing/webhooks/sender.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandResolvesRepoRootPathFromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	file := filepath.Join(dir, "cmd", "entire", "cli", "experts.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "cmd")
	t.Chdir(subdir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["cmd/entire/cli/experts.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "cmd/entire/cli/experts.go", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts path from subdir: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "cmd/entire/cli/experts.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
	if body.Query != nil {
		t.Fatalf("expected path scope, got query %q", *body.Query)
	}
}

func TestExpertsCommandResolvesCWDRelativePathFromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	file := filepath.Join(dir, "cmd", "entire", "cli", "experts.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "cmd"))
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["cmd/entire/cli/experts.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "entire/cli/experts.go", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts cwd-relative path from subdir: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "cmd/entire/cli/experts.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandTreatsPathLikeRepoOverrideArgAsScope(t *testing.T) {
	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["api/deleted.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "api/deleted.go", "--repo", "acme/widget", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts deleted path: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "api/deleted.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandRelativizesAbsoluteDeletedPathScope(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	absDeleted := filepath.Join(dir, "api", "deleted.go")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["api/deleted.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", absDeleted, "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts absolute deleted path: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "api/deleted.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandRelativizesDeletedPathScopeFromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	path := filepath.Join(dir, "cmd", "entire", "cli", "deleted.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runExpertsGit(t, dir, "add", "cmd/entire/cli/deleted.go")
	runExpertsGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "initial")
	runExpertsGit(t, dir, "rm", "cmd/entire/cli/deleted.go")
	subdir := filepath.Join(dir, "cmd")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["cmd/entire/cli/deleted.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "entire/cli/deleted.go", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts deleted path from subdir: %v", err)
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "cmd/entire/cli/deleted.go" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandAcceptsCaseInsensitiveRepoOverrideForLocalScope(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	file := filepath.Join(dir, "cmd", "entire", "cli", "experts.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["cmd/"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// "cmd" is a local directory scope (not the --repo+path shortcut), so the
	// origin vs --repo cross-check runs — GitHub names are case-insensitive.
	root.SetArgs([]string{"experts", "cmd", "--repo", "Acme/Widget", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts with case-different --repo: %v\n%s", err, out.String())
	}
	body, ok := fake.gotBody.(expertsRequest)
	if !ok {
		t.Fatalf("body type = %T", fake.gotBody)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "cmd/" {
		t.Fatalf("scopes = %#v", body.Scopes)
	}
}

func TestExpertsCommandRejectsMismatchedRepoOverrideForLocalScope(t *testing.T) {
	dir := t.TempDir()
	runExpertsGit(t, dir, "init")
	runExpertsGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	file := filepath.Join(dir, "cmd", "entire", "cli", "experts.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return &fakeExpertsClient{}, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "cmd", "--repo", "other/repo"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "local path belongs to acme/widget, not --repo other/repo") {
		t.Fatalf("error = %v\nout = %s", err, out.String())
	}
}

func TestExpertsCommandDoesNotRewritePathScope503AsCodeSearch(t *testing.T) {
	fake := &fakeExpertsClient{status: http.StatusServiceUnavailable, body: `{"error":"Database unavailable"}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "api/file.go", "--repo", "acme/widget"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "Database unavailable") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "Code search is not available") {
		t.Fatalf("path-scope 503 should not be rewritten as code search unavailable:\n%s", out.String())
	}
}

// TestExpertsCommandQuery503ShowsCodeSearchMessage covers the real backend
// behaviour observed live: a natural-language query hits a cell without code
// search and gets a bare 503, which must surface as a clean code-search message
// (not the raw "fetch experts: API error" wrap).
func TestExpertsCommandQuery503ShowsCodeSearchMessage(t *testing.T) {
	fake := &fakeExpertsClient{status: http.StatusServiceUnavailable, body: `{"error":"Service Unavailable"}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "some natural language topic", "--repo", "acme/widget"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected a non-nil (silent) error for a 503 query")
	}
	if !strings.Contains(out.String(), "Code search is not available") {
		t.Fatalf("query 503 should show the code-search message:\n%s", out.String())
	}
}

func TestExpertsCommandRejectsRepoWithStaged(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"experts", "--staged", "--repo", "acme/widget"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--staged cannot be used with --repo") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseGitStagedScopeLinesNormalizesCRLF(t *testing.T) {
	t.Parallel()
	got := parseGitStagedScopeLines("billing/foo.go\r\nbilling/bar.go\r\n")
	want := []string{"billing/foo.go", "billing/bar.go"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes[%d] = %q, want %q (full: %#v)", i, got[i], want[i], got)
		}
	}
}

func TestResolveExpertsRepoIDPaginatesAccessibleRepoList(t *testing.T) {
	const otherULID = "0123456789ABCDEFGHJKMNPR"
	fake := &fakeExpertsClient{
		reposPages: []string{
			`{"repos":[{"id":"` + otherULID + `","full_name":"other/repo"}],"next_page_token":"page2"}`,
			`{"repos":[{"id":"` + expertsTestRepoULID + `","full_name":"acme/widget"}]}`,
		},
		body: `{"repo_full_name":"acme/widget","scopes":["api/x.go"],"query":null,"branch":"main","source":"db","profiles":[]}`,
	}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "api/x.go", "--repo", "acme/widget", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts with paginated repo list: %v", err)
	}
	if len(fake.gotGetPaths) != 2 {
		t.Fatalf("GET paths = %#v, want two paginated requests", fake.gotGetPaths)
	}
	if fake.gotGetPaths[0] != expertsReposListPath {
		t.Fatalf("first GET = %q", fake.gotGetPaths[0])
	}
	if fake.gotGetPaths[1] != expertsReposListPath+"?page_token=page2" {
		t.Fatalf("second GET = %q", fake.gotGetPaths[1])
	}
	if fake.gotPath != expertsReposListPath+"/"+expertsTestRepoULID+"/experts" {
		t.Fatalf("path = %q", fake.gotPath)
	}
}

func TestExpertsCommandAcceptsRepoULIDWithoutResolution(t *testing.T) {
	fake := &fakeExpertsClient{body: `{"repo_full_name":"acme/widget","scopes":["api/x.go"],"query":null,"branch":"main","source":"db","profiles":[]}`}
	restore := setExpertsClientFactoryForTest(t, func(context.Context, bool, string, string) (expertsAPIClient, error) {
		return fake, nil
	})
	defer restore()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"experts", "api/x.go", "--repo", expertsTestRepoULID, "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute experts with ULID repo: %v", err)
	}
	// A ULID --repo addresses the data API directly — no accessible-repo lookup.
	if fake.gotGetPath != "" {
		t.Fatalf("a ULID --repo should skip resolution, but GET %q was called", fake.gotGetPath)
	}
	if fake.gotPath != expertsReposListPath+"/"+expertsTestRepoULID+"/experts" {
		t.Fatalf("path = %q, want %s/%s/experts", fake.gotPath, expertsReposListPath, expertsTestRepoULID)
	}
}

func runExpertsGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(context.Background(), "git", cmdArgs...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}
