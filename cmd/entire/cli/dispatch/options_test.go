package dispatch

import (
	"strings"
	"testing"
)

func TestResolveOptions_NormalizesScopeValues(t *testing.T) {
	t.Parallel()

	opts, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		[]string{" gh/entireio/cli ", "", "gh/entireio/cli"},
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.RepoPaths) != 1 || opts.RepoPaths[0] != testRepoSlug {
		t.Fatalf("unexpected normalized repo paths: %v", opts.RepoPaths)
	}
	if opts.Branches != nil {
		t.Fatalf("cloud mode should not implicitly set branches, got %v", opts.Branches)
	}
}

func TestResolveOptions_CloudQualifiesAndDedupesForgeSlugs(t *testing.T) {
	t.Parallel()

	opts, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		[]string{"gh/entireio/cli", " gh/EntireIO/cli ", "et/myproject/service", " et/myproject/service "},
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opts.RepoPaths, ","); got != testRepoSlug+",et/myproject/service" {
		t.Fatalf("expected forge-qualified, deduped repo paths, got %q", got)
	}
}

func TestResolveOptions_CloudCapCountsDedupedSlugs(t *testing.T) {
	t.Parallel()

	// Five distinct repos spelled six ways stay under the cap.
	repos := []string{"gh/a/b", "gh/A/B", "gh/c/d", "gh/e/f", "gh/g/h", "gh/i/j"}
	opts, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		repos,
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.RepoPaths) != CloudRepoLimit {
		t.Fatalf("expected %d deduped repos, got %v", CloudRepoLimit, opts.RepoPaths)
	}
}

func TestResolveOptions_CloudRejectsAllBranches(t *testing.T) {
	t.Parallel()

	_, err := ResolveOptions(
		false,
		"7d",
		"",
		true,
		[]string{"gh/entireio/cli"},
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "--all-branches only applies to --local") {
		t.Fatalf("expected all-branches rejection, got %v", err)
	}
}

func TestResolveOptions_CloudCapsReposAtFive(t *testing.T) {
	t.Parallel()

	repos := []string{"gh/a/b", "gh/c/d", "gh/e/f", "gh/g/h", "gh/i/j", "gh/k/l"}
	_, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		repos,
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "supports at most 5") {
		t.Fatalf("expected 5-repo cap rejection, got %v", err)
	}
}

func TestResolveOptions_LocalSetsImplicitCurrentBranch(t *testing.T) {
	t.Parallel()

	opts, err := ResolveOptions(
		true,
		"7d",
		"",
		false,
		nil,
		"",
		"",
		false,
		func() (string, error) { return "my-feature", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.ImplicitCurrentBranch {
		t.Fatal("expected ImplicitCurrentBranch to be true in local default")
	}
	if len(opts.Branches) != 1 || opts.Branches[0] != "my-feature" {
		t.Fatalf("expected implicit branches=[my-feature], got %v", opts.Branches)
	}
}

func TestResolveOptions_ForwardsInsecureHTTPAuth(t *testing.T) {
	t.Parallel()

	opts, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		[]string{"gh/entireio/cli"},
		"",
		"",
		true,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.InsecureHTTPAuth {
		t.Fatal("expected InsecureHTTPAuth=true to propagate into Options")
	}
}

func TestResolveOptions_LocalAllBranchesSkipsImplicit(t *testing.T) {
	t.Parallel()

	opts, err := ResolveOptions(
		true,
		"7d",
		"",
		true,
		nil,
		"",
		"",
		false,
		func() (string, error) { return "", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.AllBranches {
		t.Fatal("expected AllBranches=true")
	}
	if opts.ImplicitCurrentBranch {
		t.Fatal("expected ImplicitCurrentBranch=false when AllBranches is set")
	}
	if opts.Branches != nil {
		t.Fatalf("expected nil branches when AllBranches is set, got %v", opts.Branches)
	}
}

func TestResolveOptions_CloudRejectsInvalidRepoSlug(t *testing.T) {
	t.Parallel()

	_, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		[]string{"../../etc/passwd"},
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err == nil || !strings.Contains(err.Error(), `invalid repo "../../etc/passwd": expected gh/<owner>/<repo> or et/<project>/<repo>`) {
		t.Fatalf("expected repo slug validation error, got %v", err)
	}
}

func TestResolveOptions_CloudRejectsUnknownForge(t *testing.T) {
	t.Parallel()

	_, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		[]string{"gl/entireio/cli"},
		"",
		"",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err == nil || !strings.Contains(err.Error(), `invalid repo "gl/entireio/cli"`) {
		t.Fatalf("expected unknown-forge rejection, got %v", err)
	}
}

func TestResolveOptions_JurisdictionNormalizedForCloud(t *testing.T) {
	t.Parallel()

	opts, err := ResolveOptions(
		false,
		"7d",
		"",
		false,
		[]string{"gh/entireio/cli"},
		"",
		"  US ",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Jurisdiction != "us" {
		t.Fatalf("expected lowercased jurisdiction slug, got %q", opts.Jurisdiction)
	}
}

func TestResolveOptions_JurisdictionRejectedWithLocal(t *testing.T) {
	t.Parallel()

	_, err := ResolveOptions(
		true,
		"7d",
		"",
		false,
		nil,
		"",
		"us",
		false,
		func() (string, error) { return testDefaultBranchName, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "--jurisdiction cannot be used with --local") {
		t.Fatalf("expected local rejection, got %v", err)
	}
}

func TestNormalizeJurisdiction(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "us", want: "us"},
		{in: " EU ", want: "eu"},
		{in: "ap-southeast-2", want: "ap-southeast-2"},
		{in: "-us", wantErr: true},
		{in: "us east", wantErr: true},
		{in: "us.entire.io", wantErr: true},
		{in: "us-", wantErr: true},
		{in: strings.Repeat("a", 41), wantErr: true},
	} {
		got, err := normalizeJurisdiction(tc.in)
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "invalid --jurisdiction") {
				t.Errorf("normalizeJurisdiction(%q): expected slug error, got %q, %v", tc.in, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeJurisdiction(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
