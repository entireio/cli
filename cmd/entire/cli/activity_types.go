package cli

// API response types for the /api/v1/me/* endpoints used by `entire activity`.

// activityAgentCounts maps the 11 canonical agent IDs to counts.
// The API always populates every key (zero for absent agents).
type activityAgentCounts map[string]int

// userActivityResponse is the API response for GET /api/v1/me/activity.
type userActivityResponse struct {
	Stats               activityStatsResponse `json:"stats"`
	HourlyContributions []hourlyPoint         `json:"hourly_contributions"`
	Repos               []repoContribution    `json:"repos"`
	// DailyContributions is returned but unused by the CLI.
}

type activityStatsResponse struct {
	Tasks                 int     `json:"tasks"`
	Orchestration         int     `json:"orchestration"` // 0-100, percentage
	Iteration             float64 `json:"iteration"`
	Throughput            float64 `json:"throughput"`
	ContinuityHours       float64 `json:"continuity_hours"`
	Streak                int     `json:"streak"`                  // scoped to timeframe
	CurrentStreak         int     `json:"current_streak"`          // scoped to timeframe
	LifetimeStreak        int     `json:"lifetime_streak"`         // last 365 days
	LifetimeCurrentStreak int     `json:"lifetime_current_streak"` // last 365 days
}

// userCommitCheckpoint is checkpoint info nested inside a commit.
type userCommitCheckpoint struct {
	CheckpointID string   `json:"checkpoint_id"`
	Prompt       *string  `json:"prompt"`
	Agent        string   `json:"agent"`
	Agents       []string `json:"agents"`
	SessionCount int      `json:"session_count"`
	TotalSteps   int      `json:"total_steps"`
}

// userCommit represents a single commit returned by the commits API.
type userCommit struct {
	CommitSHA              string                 `json:"commit_sha"`
	CommitMsg              *string                `json:"commit_message"`
	CommitAuthorUsername   *string                `json:"commit_author_username"`
	CommitDate             *string                `json:"commit_date"`
	Additions              int                    `json:"additions"`
	Deletions              int                    `json:"deletions"`
	FilesChanged           int                    `json:"files_changed"`
	Checkpoints            []userCommitCheckpoint `json:"checkpoints"`
	RepoFullName           string                 `json:"repo_full_name"`
	IsPrivate              bool                   `json:"is_private"`
	CheckpointRepoFullName *string                `json:"checkpoint_repo_full_name"`
}

// userCommitsResponse is the API response for GET /api/v1/me/commits.
type userCommitsResponse struct {
	Commits   []userCommit `json:"commits"`
	Timeframe string       `json:"timeframe"`
	UpdatedAt string       `json:"updated_at"`
}

// Computed types used for rendering.

type contributionStats struct {
	Tasks         int
	Throughput    float64 // avg tokens/checkpoint in thousands
	Iteration     float64 // avg session_count per checkpoint
	ContinuityH   float64 // peak session length in hours (max(steps)*2/60)
	Streak        int     // longest consecutive days (last 365)
	CurrentStreak int     // current streak ending today (last 365)
}

// repoContribution matches the API's `repos[]` shape. Agents is keyed by the
// canonical agent ID (claude, gemini, …, unknown) with all 11 keys populated.
type repoContribution struct {
	Repo   string              `json:"repo"`
	Total  int                 `json:"total"`
	Agents activityAgentCounts `json:"agents"`
}

// hourlyPoint matches the API's `hourly_contributions[]` shape. AgentID is a
// canonical ID (no client-side normalization needed).
type hourlyPoint struct {
	Date    string `json:"date"` // "2006-01-02", in the caller's timezone
	Hour    int    `json:"hour"`
	AgentID string `json:"agent"`
	Value   int    `json:"value"`
}

// commitDay groups commits by date for display.
type commitDay struct {
	Date    string
	Commits []userCommit
}

// userSessionsResponse is the API response for GET /api/v1/me/sessions — the
// cross-repo recent-session feed the entire.io Overview page renders. The
// envelope is snake_case; session-item fields are camelCase (mirrors entire-api
// / entire.io exactly). Timeframe/UpdatedAt are unused by the CLI today.
type userSessionsResponse struct {
	Sessions  []userSession `json:"sessions"`
	Timeframe string        `json:"timeframe"`
	UpdatedAt string        `json:"updated_at"`
}

// userSession is one row of /me/sessions. Only the fields the Overview row
// renders are kept; the wire also carries token and attribution totals, plus
// prompt/stepCount/startedAt/customName/firstCommitAuthorUsername, which the
// web (and so the CLI) does not display. Agent/Model are pointers because the
// API sends null when unknown. DisplayName is already resolved server-side
// (custom name > AI-generated name > heuristic). repo_full_name / is_private
// are the two snake_case cross-repo fields.
type userSession struct {
	SessionID       string  `json:"sessionId"`
	DisplayName     string  `json:"displayName"`
	IsPublic        bool    `json:"isPublic"`
	Agent           *string `json:"agent"`
	Model           *string `json:"model"`
	LastActivityAt  string  `json:"lastActivityAt"`
	CheckpointCount int     `json:"checkpointCount"`
	RepoFullName    string  `json:"repo_full_name"`
}

// sessionDay groups sessions by the local day of their last activity.
type sessionDay struct {
	Date     string
	Sessions []userSession
}
