package cli

// Command and verb names that more than one file spells. Cobra takes these as
// plain strings, so without a name here a rename means finding every `Use:`,
// alias and hard-coded command path by hand.
const (
	// cmdRoot is the binary's own name, used by the root command and by the
	// hints that print a command for the user to run.
	cmdRoot       = "entire"
	cmdAgent      = "agent"
	cmdCheckpoint = "checkpoint"
	cmdCreateName = "create <name>"
	cmdList       = "list"
	cmdListRepo   = "list <repo>"
	cmdGrant      = "grant"
	cmdOrg        = "org"
	cmdRepo       = "repo"
	cmdReview     = "review"
	cmdSession    = "session"
	cmdStatus     = "status"
	cmdTokens     = "tokens"
	cmdTrail      = "trail"

	// Plural aliases, kept beside the names they alias.
	cmdCheckpointsAlias = "checkpoints"
	cmdSessionsAlias    = "sessions"
)

// Column headers shared by the control-plane tables: org, project and repo,
// plus their grant subtrees and repo's mirror subtree, print several of the
// same columns.
const (
	colHeaderCloneURL = "CLONE URL"
	colHeaderCluster  = "CLUSTER"
	colHeaderGrantee  = "GRANTEE"
	colHeaderName     = "NAME"
	colHeaderRegion   = "REGION"
	colHeaderRepo     = "REPO"
	colHeaderRole     = "ROLE"
	colHeaderSource   = "SOURCE"
	colHeaderStatus   = "STATUS"
	colHeaderType     = "TYPE"
)

// Display nouns selected by a count. Spelled here rather than inline because
// several commands phrase the same "N thing(s)" line.
const (
	nounCheckpoint  = "checkpoint"
	nounCheckpoints = "checkpoints"
	nounSession     = "session"
	nounSessions    = "sessions"
)
