package dispatch

import (
	"errors"
	"time"
)

var errDispatchMissingMarkdown = errors.New("dispatch generation returned no markdown")

type Dispatch struct {
	Window        Window
	CoveredRepos  []string
	Repos         []RepoGroup
	GeneratedText string
}

type Window struct {
	NormalizedSince   time.Time
	NormalizedUntil   time.Time
	FirstCheckpointAt time.Time
	LastCheckpointAt  time.Time
}

type RepoGroup struct {
	FullName string
	URL      string
	Sections []Section
}

type Section struct {
	Label   string
	Bullets []Bullet
}

type Bullet struct {
	CheckpointID string
	Text         string
	Source       string
	Branch       string
	CreatedAt    time.Time
	Labels       []string
}
