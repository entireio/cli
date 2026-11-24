package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

// Project reads return ulid:<account> references, while the flags take GitHub
// logins. Compare account identities, not the two spellings. Resolve everything
// before the conditional PATCH so an unknown identity cannot drop assignments.
func mergeProjectTrailAssignees(ctx context.Context, current api.ProjectTrail, add, remove []string) ([]string, error) {
	cache := make(map[string]string)
	for i, ref := range current.Assignees {
		if i < len(current.AssigneeAccountIDs) && current.AssigneeAccountIDs[i] != nil {
			id := *current.AssigneeAccountIDs[i]
			if looksLikeULID(id) {
				cache[strings.ToLower(strings.TrimSpace(ref))] = "ulid:" + strings.ToUpper(id)
			}
		}
	}
	var client *coreapi.Client
	resolve := func(ref string) (string, error) {
		ref = strings.TrimSpace(ref)
		if canonical, ok := cache[strings.ToLower(ref)]; ok {
			return canonical, nil
		}
		var id string
		if value, ok := strings.CutPrefix(ref, "ulid:"); ok {
			id = strings.TrimSpace(value)
		} else {
			// @username is an Entire username, not a GitHub login. Do not
			// resolve it in the wrong identity namespace.
			if ref == "" || strings.HasPrefix(ref, "@") || strings.Contains(ref, ":") {
				return "", fmt.Errorf("assignee %q must be a GitHub login or ulid:<account id> for --add-assignee/--remove-assignee", ref)
			}
			var err error
			if client == nil {
				client, err = activeCoreClient(ctx)
				if err != nil {
					return "", fmt.Errorf("resolve assignee %q: %w", ref, err)
				}
			}
			id, err = resolveAccountRef(ctx, client, "github:"+ref)
			if err != nil {
				return "", fmt.Errorf("resolve assignee %q: %w", ref, err)
			}
		}
		if !looksLikeULID(id) {
			return "", fmt.Errorf("assignee %q has no valid account ID", ref)
		}
		canonical := "ulid:" + strings.ToUpper(id)
		cache[strings.ToLower(ref)] = canonical
		return canonical, nil
	}
	sets := [][]string{current.Assignees, add, remove}
	for i, refs := range sets {
		resolved := make([]string, 0, len(refs))
		for _, ref := range refs {
			id, err := resolve(ref)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, id)
		}
		sets[i] = resolved
	}
	return mergeStringSet(sets[0], sets[1], sets[2]), nil
}
