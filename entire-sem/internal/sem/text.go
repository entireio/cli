package sem

import (
	"fmt"
	"io"
)

func writeText(out io.Writer, result Result) {
	fmt.Fprintf(out, "Semantic changes %s..%s\n\n", result.Base, result.Head)
	if len(result.Files) == 0 {
		fmt.Fprintln(out, "No semantic entity changes detected.")
		return
	}

	for _, file := range result.Files {
		if file.OldPath != "" && file.OldPath != file.Path {
			fmt.Fprintf(out, "%s -> %s", file.OldPath, file.Path)
		} else {
			fmt.Fprint(out, file.Path)
		}
		if file.Language != "" {
			fmt.Fprintf(out, " (%s)", file.Language)
		}
		fmt.Fprintln(out)
		for _, change := range file.Changes {
			fmt.Fprintf(out, "  %s\n", describe(change))
		}
		fmt.Fprintln(out)
	}
}

func describe(change EntityChange) string {
	dependents := dependentSuffix(change)
	switch change.Type {
	case "added":
		return fmt.Sprintf("+ %s %s added", change.Kind, change.Name)
	case "removed":
		return fmt.Sprintf("- %s %s removed%s", change.Kind, change.Name, dependents)
	case "renamed":
		return fmt.Sprintf("~ %s %s renamed from %s%s", change.Kind, change.NewName, change.OldName, dependents)
	case "signature_changed":
		return fmt.Sprintf("~ %s %s signature changed%s", change.Kind, change.Name, dependents)
	case "body_changed":
		return fmt.Sprintf("~ %s %s body changed%s", change.Kind, change.Name, dependents)
	default:
		return fmt.Sprintf("~ %s %s changed%s", change.Kind, change.Name, dependents)
	}
}

func dependentSuffix(change EntityChange) string {
	if change.Type == "added" {
		return ""
	}
	if change.DependentsCount == 1 {
		return " (1 dependent)"
	}
	return fmt.Sprintf(" (%d dependents)", change.DependentsCount)
}
