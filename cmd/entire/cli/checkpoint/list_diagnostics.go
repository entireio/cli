package checkpoint

import (
	"context"
	"sort"
	"sync"
)

// ListScopeIssueCode identifies a reason why a successful checkpoint List
// result is only a partial view of the persistent stores. List intentionally
// remains best-effort so one damaged local record or an unavailable remote does
// not hide healthy siblings; callers that promise complete enumeration can opt
// into ListScopeDiagnostics and surface that distinction to machines.
type ListScopeIssueCode string

const (
	// ListScopeIssueLocalCheckpointUnreadable means a checkpoint name was
	// present locally but its commit, tree, or metadata could not be read.
	ListScopeIssueLocalCheckpointUnreadable ListScopeIssueCode = "local_checkpoint_unreadable"
	// ListScopeIssueLocalStoreUnreadable means one persistent backend could not
	// be enumerated. Another backend may still have returned useful records.
	ListScopeIssueLocalStoreUnreadable ListScopeIssueCode = "local_checkpoint_store_unreadable"
	// ListScopeIssueRemoteEnumerationFailed means a configured checkpoint remote
	// could not be enumerated, so remote-only ref names may be absent.
	ListScopeIssueRemoteEnumerationFailed ListScopeIssueCode = "checkpoint_remote_enumeration_failed"
	// ListScopeIssueRemoteHydrationFailed means a remote-only ref name was found
	// but its metadata could not be hydrated for a session-scoped query.
	ListScopeIssueRemoteHydrationFailed ListScopeIssueCode = "remote_checkpoint_hydration_failed"
)

// ListScopeIssue is an aggregate diagnostic. It deliberately contains only a
// stable code and count: backend errors can contain credentials or user data,
// and complete-scope status never needs transcript content.
type ListScopeIssue struct {
	Code  ListScopeIssueCode `json:"code"`
	Count int                `json:"count"`
}

// ListScopeDiagnostics collects incomplete-scope facts for one List call. It
// is safe for nested stores and future concurrent enumerators to share.
type ListScopeDiagnostics struct {
	mu     sync.Mutex
	counts map[ListScopeIssueCode]int
}

type listScopeDiagnosticsContextKey struct{}

// WithListScopeDiagnostics returns a derived context and collector. Callers
// that do not install one preserve the historical best-effort List behavior.
func WithListScopeDiagnostics(ctx context.Context) (context.Context, *ListScopeDiagnostics) {
	diagnostics := &ListScopeDiagnostics{counts: make(map[ListScopeIssueCode]int)}
	return context.WithValue(ctx, listScopeDiagnosticsContextKey{}, diagnostics), diagnostics
}

// Issues returns a deterministic snapshot suitable for machine-readable
// output. An empty slice means no store reported an incomplete-scope event.
func (d *ListScopeDiagnostics) Issues() []ListScopeIssue {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	issues := make([]ListScopeIssue, 0, len(d.counts))
	for code, count := range d.counts {
		if count > 0 {
			issues = append(issues, ListScopeIssue{Code: code, Count: count})
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Code < issues[j].Code })
	return issues
}

// recordListScopeIssue records one occurrence and reports whether a collector
// was installed. Stores use the return value to retain legacy human warnings
// when no structured caller is listening.
func recordListScopeIssue(ctx context.Context, code ListScopeIssueCode) bool {
	diagnostics, ok := ctx.Value(listScopeDiagnosticsContextKey{}).(*ListScopeDiagnostics)
	if !ok || diagnostics == nil {
		return false
	}
	diagnostics.mu.Lock()
	if diagnostics.counts == nil {
		diagnostics.counts = make(map[ListScopeIssueCode]int)
	}
	diagnostics.counts[code]++
	diagnostics.mu.Unlock()
	return true
}
