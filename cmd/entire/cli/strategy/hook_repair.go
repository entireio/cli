package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

var errModifiedNativeHook = errors.New("unrecognized content in Entire-marked Git hook")

// Preflight every hook before writing any of them. A marker establishes that
// Entire participated, not that every command in the file belongs to Entire.
func checkNativeHookRepair(root *os.Root) error {
	for _, hook := range gitHookNames {
		data, err := osroot.ReadFileNoFollow(root, hook)
		if os.IsNotExist(err) || errors.Is(err, osroot.ErrSymlinkedPath) {
			continue // Missing/foreign hooks retain the installer's backup policy.
		}
		if err != nil {
			return unclassifiableHookError(root.Name(), hook, err)
		}
		content := string(data)
		if strings.Contains(content, entireHookMarker) && !recognizedNativeHookContent(content, hook) {
			return fmt.Errorf("%w: %s. Entire left it unchanged. Preserve your custom commands outside the Entire-managed hook, then run 'entire doctor' to reinstall it", errModifiedNativeHook, hook)
		}
	}
	return nil
}

func checkNativeHookRepairInDir(ctx context.Context, repoRoot string) error {
	hooksDir, err := getHooksDirInPath(ctx, repoRoot)
	if err != nil {
		return err
	}
	root, err := hooksRootForRemoval(hooksDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return checkNativeHookRepair(root)
}

// Only complete known templates are safe to migrate. Unknown historical
// variants deliberately require user attention rather than risking user edits.
func recognizedNativeHookContent(content, hook string) bool {
	const prePush = "pre-push"
	if currentNativeHookContent(content, hook) {
		return true
	}
	// The preceding release passed only the remote name to pre-push.
	if hook == prePush && currentNativeHookContent(strings.Replace(content, `pre-push "$1"`, `pre-push "$1" "$2"`, 1), hook) {
		return true
	}
	args := map[string]string{
		"prepare-commit-msg": `"$1" "$2" 2>/dev/null`,
		"commit-msg":         `"$1"`,
		"post-commit":        `2>/dev/null`,
		"post-rewrite":       `"$1" 2>/dev/null`,
		prePush:              `"$1"`,
	}
	// These are the two retired local-development launcher formats. Match the
	// whole file (including an optional generated backup chain), never merely
	// the invocation line: user additions around a legacy command matter too.
	for _, command := range []string{
		"./scripts/entire-dev hooks git " + hook,
		"./scripts/entire-dev hooks git " + hook + " " + args[hook],
		"go run ./cmd/entire/main.go hooks git " + hook + " " + args[hook] + " || true",
	} {
		base := "#!/bin/sh\n# " + entireHookMarker + "\n" + command + "\n"
		if content == base || content == generateChainedContent(base, hook) {
			return true
		}
	}
	return false
}
