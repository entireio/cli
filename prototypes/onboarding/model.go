package main

import (
	"fmt"
	"strings"
)

// FolderState is the ground-truth shape of the directory `entire enable` is run
// in — the single input that decides which rows the review screen shows.
type FolderState int

const (
	// StateRepoGitHub: git repo with a GitHub origin — the only state that can
	// mirror directly, so the Web UI row defaults to "Mirror".
	StateRepoGitHub FolderState = iota
	// StateRepoNoOrigin: git repo, no/again non-GitHub origin. Local tracking;
	// no mirror row (nothing to mirror to).
	StateRepoNoOrigin
	// StateEmpty: empty/non-git folder. The Repository row appears; picking
	// "+ GitHub" makes the repo mirrorable and reveals the Web UI row.
	StateEmpty
)

func (s FolderState) String() string {
	switch s {
	case StateRepoGitHub:
		return "repo-gh"
	case StateRepoNoOrigin:
		return "repo-no-origin"
	case StateEmpty:
		return "empty"
	}
	return "unknown"
}

func (s FolderState) found(slug string) string {
	switch s {
	case StateRepoGitHub:
		return "Git repository · GitHub origin " + slug
	case StateRepoNoOrigin:
		return "Git repository · no GitHub origin"
	case StateEmpty:
		return "Empty folder · no git repository yet"
	}
	return "Unknown folder"
}

// Config is the fully-resolved onboarding decision the review screen edits.
// Every field starts at a safe default, so Enter with no edits is valid.
type Config struct {
	State        FolderState
	Slug         string
	Region       string
	Agents       []string
	Telemetry    bool
	RepoMode     string // empty folder only: "local" | "github" | "bare"
	Publish      bool   // repo-no-origin only: publish to GitHub + mirror
	Connect      bool   // mirror to Entire (web UI) — only when mirrorable
	Checkpoints  string // "refs" | "branch"
	ImportAll    bool
	pastSessions int
}

// mirrorable reports whether this config ends with a GitHub repo we can mirror:
// an existing GitHub origin, an empty folder promoted to a GitHub repo, or a
// no-origin repo the user chose to publish.
func (c Config) mirrorable() bool {
	switch c.State {
	case StateRepoGitHub:
		return true
	case StateEmpty:
		return c.RepoMode == "github"
	case StateRepoNoOrigin:
		return c.Publish
	}
	return false
}

// publishesGitHub reports whether Enter will create + push a GitHub repo — a
// consequential action the accept CTA must spell out.
func (c Config) publishesGitHub() bool {
	return (c.State == StateEmpty && c.RepoMode == "github") || (c.State == StateRepoNoOrigin && c.Publish)
}

// showsWebUIRow reports whether the mirror on/off row is a separate control. It
// isn't for repo-no-origin: there the single Publish row already means
// "publish + mirror", so a second toggle would be redundant.
func (c Config) showsWebUIRow() bool {
	return c.State == StateRepoGitHub || (c.State == StateEmpty && c.RepoMode == "github")
}

// defaultConfig seeds both the review screen and the non-interactive path.
// Mirror defaults ON whenever the repo is mirrorable — the one-keypress path
// lands you connected.
func defaultConfig(state FolderState, slug, region string, agents []string, telemetry bool) Config {
	c := Config{
		State: state, Slug: slug, Region: region, Agents: agents,
		Telemetry: telemetry, RepoMode: "local", Checkpoints: "refs",
		pastSessions: 12,
	}
	c.Connect = c.mirrorable()
	return c
}

// StepKind identifies a unit of work in the executed plan.
type StepKind int

const (
	StepGitInit StepKind = iota
	StepInstallHooks
	StepSetBackend
	StepSetTelemetry
	StepCreateGitHubRepo
	StepLogin
	StepCreateMirror
	StepImport
)

// Step is one executed line: what happens, in the user's words.
type Step struct {
	Kind   StepKind
	Label  string
	Detail string
}

// Plan is the ordered steps runPlan executes. Built from a Config.
type Plan struct {
	State   FolderState
	Slug    string
	Region  string
	Steps   []Step
	Mirrors bool
}

func (p *Plan) add(k StepKind, label, detail string) {
	p.Steps = append(p.Steps, Step{Kind: k, Label: label, Detail: detail})
}

// planFromConfig turns the reviewed Config into an ordered, executable plan.
// This is the pure "what will happen" function a web wizard (Path 1) would
// also target.
func planFromConfig(c Config) Plan {
	p := Plan{State: c.State, Slug: c.Slug, Region: c.Region}

	if c.State == StateEmpty {
		detail := "with an initial commit"
		if c.RepoMode == "bare" {
			detail = "no initial commit"
		}
		p.add(StepGitInit, "Initialize a git repository", detail)
	}
	p.add(StepInstallHooks, "Install agent hooks", joinAgents(c.Agents))
	cp := "git-refs (recommended)"
	if c.Checkpoints == "branch" {
		cp = "shared branch"
	}
	p.add(StepSetBackend, "Checkpoint storage", cp)
	telem := "anonymous usage on"
	if !c.Telemetry {
		telem = "off"
	}
	p.add(StepSetTelemetry, "Telemetry", telem)

	if c.State == StateEmpty && c.RepoMode == "github" {
		p.add(StepCreateGitHubRepo, "Create a private GitHub repo", "and set it as origin")
	}
	if c.State == StateRepoNoOrigin && c.Publish {
		p.add(StepCreateGitHubRepo, "Publish to GitHub", "create a private repo and push")
	}
	if c.Connect && c.mirrorable() {
		p.add(StepLogin, "Log in to Entire", "opens your browser")
		p.add(StepCreateMirror, "Mirror this repo to Entire", fmt.Sprintf("region %s", c.Region))
		p.Mirrors = true
	}
	if c.ImportAll {
		p.add(StepImport, "Import past sessions", fmt.Sprintf("%d sessions", c.pastSessions))
	}
	return p
}

func joinAgents(a []string) string {
	if len(a) == 0 {
		return "none"
	}
	return strings.Join(a, ", ")
}
