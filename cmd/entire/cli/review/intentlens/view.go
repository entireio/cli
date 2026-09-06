package intentlens

import (
	"fmt"
	"io"
	"strings"
)

const DemoNotice = "Demo evidence fixture — backend not connected"

type ViewState struct {
	Audit   *Audit
	Loading bool
	Demo    bool
	Err     error
}

func Render(w io.Writer, state ViewState) {
	if state.Loading {
		fmt.Fprintln(w, "Loading audit result...")
		return
	}
	if state.Err != nil {
		fmt.Fprintf(w, "Could not display audit result: %v\n", state.Err)
		return
	}
	if state.Audit == nil || len(state.Audit.Requirements) == 0 {
		fmt.Fprintln(w, "No audit result was provided.")
		return
	}
	if state.Demo {
		fmt.Fprintln(w, DemoNotice)
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "IntentLens Audit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, state.Audit.Summary)
	for _, requirement := range state.Audit.Requirements {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s  %s  %.0f%% confidence\n", requirement.ID, requirement.Status, requirement.Confidence*100)
		fmt.Fprintln(w, requirement.Requirement)
		fmt.Fprintln(w, "Evidence:")
		for _, evidence := range requirement.Evidence {
			fmt.Fprintf(w, "  - [%s] %s\n", evidence.Type, evidence.Explanation)
			var details []string
			for _, detail := range []struct{ label, value string }{
				{"file", evidence.File}, {"symbol", evidence.Symbol}, {"test", evidence.TestName},
				{"reference", evidence.Reference}, {"result", evidence.Result},
			} {
				if strings.TrimSpace(detail.value) != "" {
					details = append(details, detail.label+": "+detail.value)
				}
			}
			if len(details) > 0 {
				fmt.Fprintf(w, "    %s\n", strings.Join(details, " | "))
			}
		}
		if strings.TrimSpace(requirement.Recommendation) == "" {
			fmt.Fprintln(w, "Recommendation: none")
		} else {
			fmt.Fprintf(w, "Recommendation: %s\n", requirement.Recommendation)
		}
	}
}
