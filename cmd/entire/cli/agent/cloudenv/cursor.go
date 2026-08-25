package cloudenv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// EnvironmentJSONRel is Cursor Cloud Agents' repo-managed environment file.
// A committed copy overrides dashboard-managed personal and team environments,
// so Entire never creates this file — it only patches `install` when the file
// already exists.
const EnvironmentJSONRel = ".cursor/environment.json"

const installField = "install"

// CursorEnvironmentResult is the enable-time outcome of wiring Entire into
// Cursor Cloud Agents.
type CursorEnvironmentResult struct {
	// Changed is true when environment.json was rewritten.
	Changed bool
	// Message is printed to the user. Empty means stay silent (already wired,
	// or the referenced install already puts entire on PATH).
	Message string
}

// EnsureCursorEnvironment appends the Entire CLI install step to an existing
// `.cursor/environment.json` `install` field. It does not create the file:
// doing so would shadow a dashboard-managed Cloud Agent environment.
func EnsureCursorEnvironment(ctx context.Context) (CursorEnvironmentResult, error) {
	if err := WriteInstallScript(ctx); err != nil {
		return CursorEnvironmentResult{}, err
	}

	root, envPath := cursorEnvPath(ctx)

	data, err := os.ReadFile(envPath) //nolint:gosec // constructed from worktree root + fixed relative path
	if os.IsNotExist(err) {
		return CursorEnvironmentResult{
			Message: cursorMissingEnvHint(),
		}, nil
	}
	if err != nil {
		return CursorEnvironmentResult{
			Message: fmt.Sprintf("  Could not read %s: %v", EnvironmentJSONRel, err),
		}, nil
	}

	raw, install, err := parseCursorEnv(data)
	if err != nil {
		return CursorEnvironmentResult{
			Message: fmt.Sprintf("  Could not parse %s: %v (left unchanged)", EnvironmentJSONRel, err),
		}, nil
	}

	if MentionsEntireInstall(install, root) {
		return CursorEnvironmentResult{}, nil
	}

	raw[installField] = json.RawMessage(mustJSONString(AppendInstallStep(install, InstallCLIStep)))
	output, err := jsonutil.MarshalIndentWithNewline(raw, "", "  ")
	if err != nil {
		return CursorEnvironmentResult{}, fmt.Errorf("marshal %s: %w", EnvironmentJSONRel, err)
	}
	if err := os.WriteFile(envPath, output, 0o644); err != nil { //nolint:gosec // project JSON, same as hooks.json
		return CursorEnvironmentResult{}, fmt.Errorf("write %s: %w", EnvironmentJSONRel, err)
	}
	return CursorEnvironmentResult{
		Changed: true,
		Message: "  ✓ Cursor Cloud Agent install includes Entire CLI (" + EnvironmentJSONRel + ")",
	}, nil
}

// RemoveCursorEnvironment strips the Entire install step from an existing
// `.cursor/environment.json` without deleting the file or other fields.
func RemoveCursorEnvironment(ctx context.Context) error {
	_, envPath := cursorEnvPath(ctx)
	data, err := os.ReadFile(envPath) //nolint:gosec // constructed from worktree root + fixed relative path
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return nil //nolint:nilerr // uninstall is best-effort on a file we may not own
	}
	raw, install, err := parseCursorEnv(data)
	if err != nil {
		return nil //nolint:nilerr // do not fail uninstall on a foreign JSON file
	}
	stripped := StripInstallStep(install, InstallCLIStep)
	if stripped == install {
		return nil
	}
	if stripped == "" {
		delete(raw, installField)
	} else {
		raw[installField] = json.RawMessage(mustJSONString(stripped))
	}
	output, err := jsonutil.MarshalIndentWithNewline(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", EnvironmentJSONRel, err)
	}
	return os.WriteFile(envPath, output, 0o644) //nolint:gosec // project JSON
}

func cursorEnvPath(ctx context.Context) (root, envPath string) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		root = "."
	}
	return root, filepath.Join(root, ".cursor", "environment.json")
}

func parseCursorEnv(data []byte) (map[string]json.RawMessage, string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", fmt.Errorf("invalid JSON: %w", err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	var install string
	if field, ok := raw[installField]; ok {
		if err := json.Unmarshal(field, &install); err != nil {
			return nil, "", fmt.Errorf("`install` is not a string: %w", err)
		}
	}
	return raw, install, nil
}

func mustJSONString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail.
		return []byte(`""`)
	}
	return b
}

func cursorMissingEnvHint() string {
	return "  Cursor Cloud Agents: no " + EnvironmentJSONRel + " in this repo.\n" +
		"    Entire did not create one (a committed file overrides dashboard-managed environments).\n" +
		"    Add this to the Cloud Agent environment install command so hooks can run:\n" +
		"      " + InstallCLIStep
}
