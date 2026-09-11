package cli

// Command and verb names that more than one file spells. Cobra takes these as
// plain strings, so without a name here a rename means finding every `Use:`,
// alias and hard-coded command path by hand.
const (
	cmdAgent      = "agent"
	cmdCheckpoint = "checkpoint"
	cmdCreateName = "create <name>"
	cmdList       = "list"
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

// Column headers shared by the control-plane tables: org, project, repo, grant
// and the mirror subtree each print several of the same columns.
const (
	colHeaderCloneURL = "CLONE URL"
	colHeaderCluster  = "CLUSTER"
	colHeaderName     = "NAME"
	colHeaderRegion   = "REGION"
	colHeaderRole     = "ROLE"
	colHeaderStatus   = "STATUS"
)

// Display nouns selected by a count. Spelled here rather than inline because
// several commands phrase the same "N thing(s)" line.
const (
	nounCheckpoint  = "checkpoint"
	nounCheckpoints = "checkpoints"
	nounSession     = "session"
	nounSessions    = "sessions"
)
