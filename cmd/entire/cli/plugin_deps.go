package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// Dependency handling is install-time only. Dispatch stays zero-cost: a
// plugin invokes its deps as entire-<dep> via PATH at runtime (the managed
// bin dir is prepended for plugin children), and `entire plugin doctor`
// reports drift after the fact. There is deliberately no version-range
// solver — requirements carry at most a minimum version.

// maxDepDepth bounds transitive resolution. Plugin graphs are shallow in
// practice; the bound turns a metadata cycle that slips past the visited
// set (e.g. two names aliasing one repo) into an error instead of a hang.
const maxDepDepth = 10

// DepAction is one planned dependency install or upgrade.
type DepAction struct {
	Name       string
	RepoURL    string
	MinVersion string
	// Upgrade is true when the dependency is installed but below
	// MinVersion; the action reinstalls at the latest tag.
	Upgrade bool
	// CurrentTag is the installed tag for upgrades.
	CurrentTag string
}

// DepPlan is the result of resolving a plugin's transitive requirements.
type DepPlan struct {
	Actions []DepAction
	// Warnings are non-blocking observations, e.g. a dependency satisfied
	// from raw $PATH whose version cannot be verified.
	Warnings []string
}

// PlanDependencyInstalls resolves the transitive requirements of rootReqs
// into ordered install/upgrade actions. Missing dependencies must be
// resolvable to a repo URL via the requirement itself or the index. The
// visited set plus a depth bound make cycles an error path, not a hang.
func PlanDependencyInstalls(ctx context.Context, rootReqs []PluginRequirement, idx *PluginIndex) (*DepPlan, error) {
	plan := &DepPlan{}
	// name -> strictest min_version considered so far, not a plain seen-set:
	// see planDeps.
	considered := map[string]string{}
	if err := planDeps(ctx, rootReqs, idx, plan, considered, 0); err != nil {
		return nil, err
	}
	return plan, nil
}

// stricterMinVersion reports whether a demands a higher minimum than b.
// An empty string means "no minimum": nothing is stricter than it, and any
// concrete minimum is stricter than it.
func stricterMinVersion(a, b string) bool {
	switch {
	case a == "":
		return false
	case b == "":
		return true
	default:
		return semver.Compare(canonicalSemver(a), canonicalSemver(b)) > 0
	}
}

// upsertDepAction keeps one action per plugin name, replacing an earlier
// entry when a stricter requirement supersedes it.
func upsertDepAction(plan *DepPlan, action DepAction) {
	for i := range plan.Actions {
		if plan.Actions[i].Name == action.Name {
			plan.Actions[i] = action
			return
		}
	}
	plan.Actions = append(plan.Actions, action)
}

// addDepWarning appends a warning unless it is already present. Re-visiting a
// name under a stricter constraint can re-derive the same observation, and
// printing it twice would just look like a bug.
func addDepWarning(plan *DepPlan, warning string) {
	if warning == "" || slices.Contains(plan.Warnings, warning) {
		return
	}
	plan.Warnings = append(plan.Warnings, warning)
}

func planDeps(ctx context.Context, reqs []PluginRequirement, idx *PluginIndex, plan *DepPlan, considered map[string]string, depth int) error {
	if depth > maxDepDepth {
		return fmt.Errorf("dependency chain deeper than %d; cycle in plugin metadata?", maxDepDepth)
	}
	for _, req := range reqs {
		// Keyed by name *and* gated on the strictest min_version seen so far,
		// not a plain seen-set. In a diamond where two requirers demand
		// different minimums of the same plugin (A needs C >= v1, B needs
		// C >= v2), a name-only set skipped B's stricter constraint entirely:
		// an installed C satisfying v1 produced no action and no warning, so
		// the install completed leaving B running against a too-old C. Doctor
		// caught it afterwards — it walks each manifest's requirements
		// independently — but the install should plan the upgrade, not defer
		// the discovery.
		if prev, seen := considered[req.Name]; seen && !stricterMinVersion(req.MinVersion, prev) {
			continue
		}
		// Past the guard this name is either new or strictly stricter than
		// what was recorded, so this requirement's minimum is the one to keep.
		considered[req.Name] = req.MinVersion

		status, err := dependencySatisfied(req)
		if err != nil {
			return err
		}
		addDepWarning(plan, status.Warning)
		if status.Satisfied {
			// An already-satisfied managed dependency can itself have
			// gaps (e.g. its own dep was removed with --force since
			// install). Walk its recorded requirements — offline, from
			// the manifest — so installing a parent repairs the whole
			// chain instead of stopping at the first satisfied node.
			// PATH/local-dev installs have no manifest; doctor covers
			// those.
			if status.Manifest != nil {
				if err := planDeps(ctx, status.Manifest.Requires, idx, plan, considered, depth+1); err != nil {
					return err
				}
			}
			continue
		}

		action := DepAction{Name: req.Name, MinVersion: req.MinVersion}
		if m := status.Manifest; m != nil {
			// Installed but below min_version: upgrade in place from its
			// recorded repo.
			action.Upgrade = true
			action.CurrentTag = m.Tag
			action.RepoURL = m.RepoURL
		} else {
			// Missing: resolve by name through the index, which is the only
			// source. See PluginRequirement for why an author-supplied URL is
			// not accepted here.
			e := idx.Find(req.Name)
			if e == nil {
				return fmt.Errorf("dependency %q is not installed and is not in the plugin index; ask the plugin author to get it listed (try 'entire plugin search %s'), or install it yourself from its repository URL first", req.Name, req.Name)
			}
			action.RepoURL = e.RepoURL
		}
		upsertDepAction(plan, action)

		// Recurse into what the dependency itself requires, using metadata
		// only — nothing is downloaded during planning. Inspection
		// failures don't fail the plan (the install will surface hard
		// errors itself), but they must not be silent either: a confirmed
		// plan that quietly omitted nested dependencies would look
		// complete while leaving gaps for doctor to find.
		tag := ""
		if tags, err := listRemoteSemverTags(ctx, action.RepoURL); err == nil && len(tags) > 0 {
			tag = tags[0]
		}
		if tag == "" {
			addDepWarning(plan, fmt.Sprintf("could not list tags for %q (%s); its own dependencies were not inspected — 'entire plugin doctor' will report any gaps", req.Name, redactURL(action.RepoURL)))
			continue
		}
		meta, err := fetchPluginMetadataAtTag(ctx, action.RepoURL, tag)
		if err != nil {
			addDepWarning(plan, fmt.Sprintf("could not read %s for %q at %s; its own dependencies were not inspected — 'entire plugin doctor' will report any gaps", pluginMetadataFileName, req.Name, tag))
			continue
		}
		if meta == nil {
			continue // no metadata file: no declared dependencies
		}
		if err := planDeps(ctx, meta.Requires, idx, plan, considered, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// dependencySatisfied checks whether a requirement is already met, in
// order: managed install with manifest (version-checkable) → managed entry
// without manifest (local-dev, version unknown) → raw $PATH (version
// unknown). Note `entire plugin <verb>` runs with the user's original PATH
// (main.go restores it for built-ins), so LookPath here sees only
// raw-PATH plugins — managed entries are checked explicitly first.
// depStatus is what dependencySatisfied concluded about one requirement.
type depStatus struct {
	// Satisfied is true when nothing needs installing.
	Satisfied bool
	// Manifest is the dependency's install manifest, or nil for a local-dev
	// install or a raw-$PATH binary. Returned rather than re-loaded by callers:
	// every one of them needed it, so the file was being read three times per
	// requirement.
	Manifest *PluginManifest
	// Warning is a non-blocking observation, e.g. a min_version that cannot be
	// verified because the dependency came from $PATH.
	Warning string
}

func dependencySatisfied(req PluginRequirement) (depStatus, error) {
	m, err := LoadPluginManifest(req.Name)
	if err != nil {
		return depStatus{}, err
	}
	if m != nil {
		satisfied := req.MinVersion == "" ||
			semver.Compare(canonicalSemver(m.Tag), canonicalSemver(req.MinVersion)) >= 0
		// Not satisfied here means installed-but-too-old, which is the upgrade
		// path in planDeps.
		return depStatus{Satisfied: satisfied, Manifest: m}, nil
	}
	installed, err := FindInstalledPlugin(req.Name)
	if err != nil {
		return depStatus{}, err
	}
	if installed != nil {
		return depStatus{Satisfied: true, Warning: unverifiableVersionWarning(req, "a local-dev install")}, nil
	}
	if _, err := exec.LookPath(pluginBinaryPrefix + req.Name); err == nil {
		return depStatus{Satisfied: true, Warning: unverifiableVersionWarning(req, "$PATH")}, nil
	}
	return depStatus{}, nil
}

func unverifiableVersionWarning(req PluginRequirement, source string) string {
	if req.MinVersion == "" {
		return ""
	}
	return fmt.Sprintf("dependency %q is satisfied from %s; cannot verify min_version %s", req.Name, source, req.MinVersion)
}

// ExecuteDepPlan runs the planned installs. Upgrades pass Force.
// allowUnverified is inherited from the root install so a single
// --allow-unverified covers the whole transitive set the user confirmed,
// rather than failing partway through on the first dependency without
// published checksums.
func ExecuteDepPlan(ctx context.Context, plan *DepPlan, allowUnverified bool) ([]*RemoteInstallResult, error) {
	results := make([]*RemoteInstallResult, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		// The plan named this dependency and the user confirmed that name; an
		// install landing under a different one would never satisfy the
		// requirement, leaving doctor to report it missing forever.
		res, err := InstallPluginFromRepo(ctx, a.RepoURL, a.Name, RemoteInstallOptions{
			Force: a.Upgrade, AllowUnverified: allowUnverified,
		})
		if err != nil {
			return results, fmt.Errorf("install dependency %q: %w", a.Name, err)
		}
		results = append(results, res)
		// Verify the outcome, not just that an install happened. The install
		// takes the newest published tag, which may still be below the minimum
		// the plan computed — in which case reporting success would silently
		// defeat the guarantee the plan was built on, and leave doctor
		// complaining forever about a dependency we just "installed".
		if a.MinVersion != "" &&
			semver.Compare(canonicalSemver(res.Manifest.Tag), canonicalSemver(a.MinVersion)) < 0 {
			return results, fmt.Errorf("dependency %q: newest release is %s but %s or later is required; ask the plugin author to publish a release that meets it",
				a.Name, res.Manifest.Tag, a.MinVersion)
		}
	}
	return results, nil
}

// DependentsOf returns the names of managed plugins whose manifests list
// name as a requirement — the remove guard. Computed by scanning
// pkg/*/manifest.yml; no extra state to maintain.
func DependentsOf(name string) ([]string, error) {
	manifests, err := ListPluginManifests()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range manifests {
		for _, req := range m.Requires {
			if req.Name == name {
				out = append(out, m.Name)
				break
			}
		}
	}
	return out, nil
}

// PluginDoctorIssue is one problem found by RunPluginDoctor.
type PluginDoctorIssue struct {
	Plugin  string
	Problem string
	Fix     string
}

// RunPluginDoctor checks every managed plugin: bin entry present, dangling
// local-dev symlinks, dependency presence and min_versions, and (macOS) a
// quarantine attribute that would block execution.
func RunPluginDoctor(ctx context.Context) ([]PluginDoctorIssue, error) {
	var issues []PluginDoctorIssue

	installed, err := ListInstalledPlugins()
	if err != nil {
		return nil, err
	}
	installedByName := map[string]*InstalledPlugin{}
	for _, p := range installed {
		installedByName[p.Name] = p
		if p.Symlink {
			if _, err := exec.LookPath(p.Path); err != nil {
				issues = append(issues, PluginDoctorIssue{
					Plugin:  p.Name,
					Problem: "managed entry is a dangling or non-executable link to " + p.LinkTarget,
					Fix:     "rebuild the target or run: entire plugin remove " + p.Name,
				})
			}
		}
		if q := quarantinedPath(ctx, p.Path); q != "" {
			issues = append(issues, PluginDoctorIssue{
				Plugin:  p.Name,
				Problem: "binary carries the macOS quarantine attribute; Gatekeeper will block it",
				Fix:     "run: xattr -d com.apple.quarantine " + q,
			})
		}
	}

	manifests, err := ListPluginManifests()
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		if installedByName[m.Name] == nil {
			issues = append(issues, PluginDoctorIssue{
				Plugin:  m.Name,
				Problem: "has an install manifest but no entry in the managed bin dir",
				Fix:     "reinstall: " + reinstallCommand(m),
			})
		}
		issues = append(issues, checkManagedBinaryIntegrity(m)...)
		for _, req := range m.Requires {
			status, err := dependencySatisfied(req)
			if err != nil {
				return nil, err
			}
			switch {
			case status.Satisfied && status.Warning != "":
				issues = append(issues, PluginDoctorIssue{Plugin: m.Name, Problem: status.Warning, Fix: ""})
			case !status.Satisfied:
				if dep := status.Manifest; dep != nil {
					issues = append(issues, PluginDoctorIssue{
						Plugin:  m.Name,
						Problem: fmt.Sprintf("requires %s >= %s but %s is installed", req.Name, req.MinVersion, dep.Tag),
						Fix:     "run: entire plugin upgrade " + req.Name,
					})
				} else {
					issues = append(issues, PluginDoctorIssue{
						Plugin:  m.Name,
						Problem: fmt.Sprintf("requires %q, which is not installed", req.Name),
						Fix:     "run: entire plugin install " + req.Name,
					})
				}
			}
		}
	}
	return issues, nil
}

// reinstallCommand is the suggested repair for a broken managed install. It
// carries --allow-unverified for a plugin installed that way, because the
// reinstall re-runs the checksum requirement and would otherwise fail with
// errUnverifiedAsset — handing the user a command that cannot succeed on
// exactly the plugins doctor flags.
func reinstallCommand(m *PluginManifest) string {
	cmd := fmt.Sprintf("entire plugin install %s --force", redactURL(m.RepoURL))
	if m.Unverified {
		cmd += " --allow-unverified"
	}
	if m.Pinned {
		// Without this the repair silently unpins: the reinstall would take the
		// newest tag and write a manifest with Pinned cleared, so a plugin the
		// user deliberately held at a version would start tracking latest as a
		// side effect of fixing something unrelated.
		cmd += " --pin " + m.Tag
	}
	return cmd
}

// checkManagedBinaryIntegrity re-hashes a managed plugin's binary and
// compares it to the digest recorded at install time, catching a binary
// swapped out under the managed directory after install.
//
// This checks the pkg/ binary, which is the copy the manifest describes and
// the target bin/ links to. Where the bin/ entry is a copy rather than a link
// (Windows without Developer Mode), a tampered copy is not covered — the
// dangling/non-executable link check above is what guards that surface.
//
// A manifest without BinarySHA256 predates integrity recording, so there is
// nothing to compare and silence is correct; nagging about it would only tell
// the user to reinstall a plugin that is probably fine.
func checkManagedBinaryIntegrity(m *PluginManifest) []PluginDoctorIssue {
	var issues []PluginDoctorIssue
	if m.Unverified {
		issues = append(issues, PluginDoctorIssue{
			Plugin:  m.Name,
			Problem: fmt.Sprintf("installed from %s without checksum verification (the release published no %s)", m.Tag, checksumsFileName),
			Fix:     "if the author has since published checksums, re-run: entire plugin upgrade " + m.Name,
		})
	}
	if m.BinarySHA256 == "" {
		return issues
	}
	dir, err := PluginPkgDir(m.Name)
	if err != nil {
		return issues
	}
	binPath := filepath.Join(dir, pluginBinaryName(m.Name))
	got, err := fileSHA256(binPath)
	if err != nil {
		issues = append(issues, PluginDoctorIssue{
			Plugin:  m.Name,
			Problem: "managed binary is missing or unreadable: " + err.Error(),
			Fix:     "reinstall: " + reinstallCommand(m),
		})
		return issues
	}
	if !strings.EqualFold(got, m.BinarySHA256) {
		issues = append(issues, PluginDoctorIssue{
			Plugin:  m.Name,
			Problem: "managed binary no longer matches the digest recorded at install; it was modified or replaced outside entire",
			Fix:     "reinstall from the recorded source: " + reinstallCommand(m),
		})
	}
	return issues
}

// quarantinedPath returns the path to flag when the file (or its symlink
// target) carries com.apple.quarantine. Best-effort, macOS only; any error
// means "not quarantined". The attribute appears when a user manually
// drops a browser-downloaded binary into the managed dir — CLI downloads
// don't set it.
//
// Checking the bin entry is sufficient even though remote installs link
// bin/entire-<name> to the real binary under pkg/: macOS xattr follows
// symlinks by default (-s is the flag to act on the link itself), so both
// the probe here and the suggested `xattr -d` fix operate on the target.
func quarantinedPath(ctx context.Context, path string) string {
	if runtime.GOOS != darwinGOOS {
		return ""
	}
	out, err := exec.CommandContext(ctx, "xattr", "-p", "com.apple.quarantine", path).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return ""
	}
	return path
}
