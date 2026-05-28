package repov1

import "time"

const (
	SHAHexPattern = "^[0-9a-f]{40}$|^[0-9a-f]{64}$"
	RepoIDPattern = "^[0-9A-HJKMNP-TV-Z]{26}$"
)

type ResolveRequest struct {
	RepoPath        string `doc:"Prefixed repository path (et/project/repo, git/owner/repo, or gh/owner/repo); the surface prefix is required." json:"repoPath"                       minLength:"1"`
	IncludeReplicas bool   `default:"false"                                                                                                     doc:"Include hosting-node base URIs." json:"includeReplicas"`
}

type ResolveInput struct {
	RepoPath        string `doc:"Prefixed repository path (et/project/repo, git/owner/repo, or gh/owner/repo); the surface prefix is required." minLength:"1"                         query:"repoPath"        required:"true"`
	IncludeReplicas bool   `default:"false"                                                                                                     doc:"Include hosting-node base URIs." query:"includeReplicas"`
}

type ResolveResponse struct {
	RepoID   string   `doc:"Repository ULID."                           json:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Replicas []string `doc:"Hosting-node base URIs for the repository." json:"replicas"`
}

type ResolveOutput struct{ Body ResolveResponse }

type MergeBaseRequest struct {
	CommitA string `doc:"Branch name or commit SHA." json:"commitA" minLength:"1"`
	CommitB string `doc:"Branch name or commit SHA." json:"commitB" minLength:"1"`
}

type MergeBaseInput struct {
	RepoID  string `doc:"Repository ULID."           path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	CommitA string `doc:"Branch name or commit SHA." minLength:"1" query:"commitA"                    required:"true"`
	CommitB string `doc:"Branch name or commit SHA." minLength:"1" query:"commitB"                    required:"true"`
}

type MergeBaseResponse struct {
	MergeBaseSHA string   `doc:"Commit SHA of the merge base."             json:"mergeBaseSHA,omitempty" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ResolvedSHAs []string `doc:"Resolved input ref SHAs in request order." json:"resolvedSHAs"`
}

type MergeBaseOutput struct{ Body MergeBaseResponse }

type Signature struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `format:"date-time" json:"date"`
}

type Trailer struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ExtraHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Commit struct {
	SHA          string        `json:"sha"                                                             pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Parents      []string      `json:"parents"`
	Message      string        `json:"message"`
	Author       Signature     `json:"author"`
	Committer    Signature     `json:"committer"`
	Trailers     []Trailer     `doc:"Ordered git trailers; populated only when parseTrailers is true." json:"trailers,omitempty"`
	Signature    string        `json:"signature,omitempty"`
	MergeTags    []string      `json:"mergeTags,omitempty"`
	ExtraHeaders []ExtraHeader `json:"extraHeaders,omitempty"`
}

type ListCommitsRequest struct {
	Ref           string     `json:"ref"                 minLength:"1"`
	NotRef        string     `json:"notRef,omitempty"`
	Since         *time.Time `format:"date-time"         json:"since,omitempty"`
	Until         *time.Time `format:"date-time"         json:"until,omitempty"`
	Path          string     `json:"path,omitempty"`
	Author        string     `json:"author,omitempty"`
	FirstParent   bool       `default:"false"            json:"firstParent"`
	ParseTrailers bool       `default:"false"            json:"parseTrailers"`
	PageSize      int32      `json:"pageSize,omitempty"  maximum:"200"          minimum:"0"`
	PageToken     string     `json:"pageToken,omitempty"`
}

type ListCommitsInput struct {
	RepoID        string    `path:"repoId"      pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Ref           string    `minLength:"1"      query:"ref"                        required:"true"`
	NotRef        string    `query:"notRef"`
	Since         time.Time `format:"date-time" query:"since"`
	Until         time.Time `format:"date-time" query:"until"`
	Path          string    `query:"path"`
	Author        string    `query:"author"`
	FirstParent   bool      `default:"false"    query:"firstParent"`
	ParseTrailers bool      `default:"false"    query:"parseTrailers"`
	PageSize      int32     `maximum:"200"      minimum:"0"                        query:"pageSize"`
	PageToken     string    `query:"pageToken"`
}

type ListCommitsResponse struct {
	Commits       []Commit `json:"commits"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

type ListCommitsOutput struct{ Body ListCommitsResponse }

type Ref struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commitSHA" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
}

type ListBranchesRequest struct {
	Search    string `json:"search,omitempty"`
	Regex     string `json:"regex,omitempty"`
	PageSize  int32  `json:"pageSize,omitempty"  maximum:"500" minimum:"0"`
	PageToken string `json:"pageToken,omitempty"`
}

type ListBranchesInput struct {
	RepoID    string `path:"repoId"     pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Search    string `query:"search"`
	Regex     string `query:"regex"`
	PageSize  int32  `maximum:"500"     minimum:"0"                        query:"pageSize"`
	PageToken string `query:"pageToken"`
}

type ListBranchesResponse struct {
	Branches      []Ref  `json:"branches"`
	NextPageToken string `json:"nextPageToken,omitempty"`
}

type ListBranchesOutput struct{ Body ListBranchesResponse }

type ListTagsRequest struct {
	Search    string `json:"search,omitempty"`
	Regex     string `json:"regex,omitempty"`
	PageSize  int32  `json:"pageSize,omitempty"  maximum:"500" minimum:"0"`
	PageToken string `json:"pageToken,omitempty"`
}

type ListTagsInput struct {
	RepoID    string `path:"repoId"     pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Search    string `query:"search"`
	Regex     string `query:"regex"`
	PageSize  int32  `maximum:"500"     minimum:"0"                        query:"pageSize"`
	PageToken string `query:"pageToken"`
}

type ListTagsResponse struct {
	Tags          []Ref  `json:"tags"`
	NextPageToken string `json:"nextPageToken,omitempty"`
}

type ListTagsOutput struct{ Body ListTagsResponse }

type TreeEntryType string

const (
	TreeEntryTypeBlob   TreeEntryType = "blob"
	TreeEntryTypeTree   TreeEntryType = "tree"
	TreeEntryTypeCommit TreeEntryType = "commit"
)

type TreeEntry struct {
	Path string        `json:"path"`
	Mode string        `json:"mode"`
	Type TreeEntryType `enum:"blob,tree,commit" json:"type"`
	SHA  string        `json:"sha"              pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Size int64         `json:"size"             minimum:"0"`
}

type GetTreeRequest struct {
	Ref       string `json:"ref"            minLength:"1"`
	Path      string `json:"path,omitempty"`
	Recursive bool   `default:"false"       json:"recursive"`
}

type GetTreeInput struct {
	RepoID    string `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Ref       string `minLength:"1"   query:"ref"                        required:"true"`
	Path      string `query:"path"`
	Recursive bool   `default:"false" query:"recursive"`
}

type GetTreeResponse struct {
	SHA       string      `json:"sha"       pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Entries   []TreeEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

type GetTreeOutput struct{ Body GetTreeResponse }

type FileStatus string

const (
	FileStatusOK       FileStatus = "ok"
	FileStatusNotFound FileStatus = "notFound"
	FileStatusTooLarge FileStatus = "tooLarge"
)

type FileHeader struct {
	Path   string     `json:"path"`
	SHA    string     `json:"sha,omitempty"        pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Size   int64      `json:"size"                 minimum:"0"`
	Status FileStatus `enum:"ok,notFound,tooLarge" json:"status"`
}

type GetFilesRequest struct {
	Ref   string   `json:"ref"   minLength:"1"`
	Paths []string `json:"paths" maxItems:"100" minItems:"1"`
}

type GetFilesInput struct {
	RepoID string   `path:"repoId"  pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Ref    string   `minLength:"1"  query:"ref"                        required:"true"`
	Paths  []string `maxItems:"100" minItems:"1"                       query:"path,explode" required:"true"`
}

type RawFileRequest struct {
	Ref  string `json:"ref"  minLength:"1"`
	Path string `json:"path" minLength:"1"`
}

type RawFileInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Ref    string `minLength:"1" query:"ref"                        required:"true"`
	Path   string `minLength:"1" query:"path"                       required:"true"`
}

type RawFilePathInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Ref    string `minLength:"1" query:"ref"                        required:"true"`
	Path   string `minLength:"1" path:"path"`
}

type ChangedFileStatus string

const (
	ChangedFileStatusAdded    ChangedFileStatus = "added"
	ChangedFileStatusModified ChangedFileStatus = "modified"
	ChangedFileStatusRemoved  ChangedFileStatus = "removed"
	ChangedFileStatusRenamed  ChangedFileStatus = "renamed"
)

type ChangedFile struct {
	Path         string            `json:"path"`
	Status       ChangedFileStatus `enum:"added,modified,removed,renamed" json:"status"`
	PreviousPath string            `json:"previousPath,omitempty"`
	Additions    int64             `json:"additions"                      minimum:"0"`
	Deletions    int64             `json:"deletions"                      minimum:"0"`
}

type CompareRequest struct {
	Base string `json:"base"           minLength:"1"`
	Head string `json:"head"           minLength:"1"`
	Path string `json:"path,omitempty"`
}

type CompareInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Base   string `minLength:"1" query:"base"                       required:"true"`
	Head   string `minLength:"1" query:"head"                       required:"true"`
	Path   string `query:"path"`
}

type CompareResponse struct {
	Files []ChangedFile `json:"files"`
}

type CompareOutput struct{ Body CompareResponse }

type DiffRequest struct {
	Base string `json:"base"           minLength:"1"`
	Head string `json:"head"           minLength:"1"`
	Path string `json:"path,omitempty"`
}

type DiffInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Base   string `minLength:"1" query:"base"                       required:"true"`
	Head   string `minLength:"1" query:"head"                       required:"true"`
	Path   string `query:"path"`
}

type MergeStrategy string

const (
	MergeStrategyMerge           MergeStrategy = "merge"
	MergeStrategyNoFastForward   MergeStrategy = "noFastForward"
	MergeStrategyFastForwardOnly MergeStrategy = "fastForwardOnly"
	MergeStrategySquash          MergeStrategy = "squash"
)

type MergeOutcome string

const (
	MergeOutcomeMergeCommit     MergeOutcome = "mergeCommit"
	MergeOutcomeFastForward     MergeOutcome = "fastForward"
	MergeOutcomeSquash          MergeOutcome = "squash"
	MergeOutcomeAlreadyUpToDate MergeOutcome = "alreadyUpToDate"
	MergeOutcomeConflict        MergeOutcome = "conflict"
)

type RevertOutcome string

const (
	RevertOutcomeReverted        RevertOutcome = "reverted"
	RevertOutcomeAlreadyUpToDate RevertOutcome = "alreadyUpToDate"
	RevertOutcomeConflict        RevertOutcome = "conflict"
)

type ConflictReason string

const (
	ConflictReasonTreeType    ConflictReason = "treeType"
	ConflictReasonTreeMode    ConflictReason = "treeMode"
	ConflictReasonBlobOverlap ConflictReason = "blobOverlap"
	ConflictReasonBlobBinary  ConflictReason = "blobBinary"
)

type MergeRequest struct {
	BaseRef         string        `json:"baseRef"                                    minLength:"1"`
	HeadRef         string        `json:"headRef"                                    minLength:"1"`
	Strategy        MergeStrategy `enum:"merge,noFastForward,fastForwardOnly,squash" json:"strategy"`
	ExpectedBaseSHA string        `json:"expectedBaseSHA"                            pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	CommitMessage   string        `json:"commitMessage,omitempty"`
	ExpectedHeadSHA string        `json:"expectedHeadSHA,omitempty"                  pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
}

type MergeInput struct {
	RepoID string       `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   MergeRequest `required:"true"`
}

type MergeResponse struct {
	Outcome         MergeOutcome   `enum:"mergeCommit,fastForward,squash,alreadyUpToDate,conflict" json:"outcome"`
	MergeCommitSHA  string         `json:"mergeCommitSHA,omitempty"                                pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	NewBaseSHA      string         `json:"newBaseSHA,omitempty"                                    pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ConflictedPaths []string       `json:"conflictedPaths"`
	ConflictReason  ConflictReason `enum:"treeType,treeMode,blobOverlap,blobBinary"                json:"conflictReason,omitempty"`
}

type MergeOutput struct{ Body MergeResponse }

type DryRunMergeRequest struct {
	BaseRef  string        `json:"baseRef"                                    minLength:"1"`
	HeadRef  string        `json:"headRef"                                    minLength:"1"`
	Strategy MergeStrategy `enum:"merge,noFastForward,fastForwardOnly,squash" json:"strategy"`
}

type DryRunMergeInput struct {
	RepoID string             `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   DryRunMergeRequest `required:"true"`
}

type DryRunMergeResponse struct {
	Outcome         MergeOutcome   `enum:"mergeCommit,fastForward,squash,alreadyUpToDate,conflict" json:"outcome"`
	ResolvedBaseSHA string         `json:"resolvedBaseSHA,omitempty"                               pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ResolvedHeadSHA string         `json:"resolvedHeadSHA,omitempty"                               pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	MergeBaseSHA    string         `json:"mergeBaseSHA,omitempty"                                  pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ConflictedPaths []string       `json:"conflictedPaths"`
	ConflictReason  ConflictReason `enum:"treeType,treeMode,blobOverlap,blobBinary"                json:"conflictReason,omitempty"`
	CommitsAhead    uint32         `json:"commitsAhead"`
}

type DryRunMergeOutput struct{ Body DryRunMergeResponse }

type RebaseRequest struct {
	Branch            string `json:"branch"                      minLength:"1"`
	Onto              string `json:"onto"                        minLength:"1"`
	Upstream          string `json:"upstream,omitempty"`
	ExpectedBranchSHA string `json:"expectedBranchSHA,omitempty" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ExpectedOntoSHA   string `json:"expectedOntoSHA,omitempty"   pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
}

type RebaseInput struct {
	RepoID string        `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   RebaseRequest `required:"true"`
}

type RebaseEventType string

const (
	RebaseEventTypeStarted   RebaseEventType = "started"
	RebaseEventTypeApplied   RebaseEventType = "applied"
	RebaseEventTypeConflict  RebaseEventType = "conflict"
	RebaseEventTypeCompleted RebaseEventType = "completed"
)

type CommitApplied struct {
	OriginalSHA string `json:"originalSHA"      pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	NewSHA      string `json:"newSHA,omitempty" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Dropped     bool   `json:"dropped"`
}

type CommitMapping struct {
	OriginalSHA string `json:"originalSHA"      pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	NewSHA      string `json:"newSHA,omitempty" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Dropped     bool   `json:"dropped"`
}

type RebaseEvent struct {
	Type            RebaseEventType `enum:"started,applied,conflict,completed"       json:"type"`
	MergeBaseSHA    string          `json:"mergeBaseSHA,omitempty"                   pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	CommitsToApply  []string        `json:"commitsToApply,omitempty"`
	OntoTipSHA      string          `json:"ontoTipSHA,omitempty"                     pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	OriginalSHA     string          `json:"originalSHA,omitempty"                    pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	NewSHA          string          `json:"newSHA,omitempty"                         pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Dropped         bool            `json:"dropped,omitempty"`
	CommitSHA       string          `json:"commitSHA,omitempty"                      pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ConflictedPaths []string        `json:"conflictedPaths,omitempty"`
	ConflictReason  ConflictReason  `enum:"treeType,treeMode,blobOverlap,blobBinary" json:"conflictReason,omitempty"`
	NewBranchSHA    string          `json:"newBranchSHA,omitempty"                   pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Mapping         []CommitMapping `json:"mapping,omitempty"`
}

type DryRunRebaseRequest struct {
	Branch   string `json:"branch"             minLength:"1"`
	Onto     string `json:"onto"               minLength:"1"`
	Upstream string `json:"upstream,omitempty"`
}

type DryRunRebaseInput struct {
	RepoID string              `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   DryRunRebaseRequest `required:"true"`
}

type RebaseCommitPreview struct {
	OriginalSHA     string         `json:"originalSHA"                              pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	WouldConflict   bool           `json:"wouldConflict"`
	ConflictedPaths []string       `json:"conflictedPaths"`
	WouldBeDropped  bool           `json:"wouldBeDropped"`
	ConflictReason  ConflictReason `enum:"treeType,treeMode,blobOverlap,blobBinary" json:"conflictReason,omitempty"`
}

type DryRunRebaseResponse struct {
	ResolvedBranchSHA string                `json:"resolvedBranchSHA,omitempty" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ResolvedOntoSHA   string                `json:"resolvedOntoSHA,omitempty"   pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	MergeBaseSHA      string                `json:"mergeBaseSHA,omitempty"      pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	CommitsToApply    []string              `json:"commitsToApply"`
	Previews          []RebaseCommitPreview `json:"previews"`
	WouldSucceed      bool                  `json:"wouldSucceed"`
}

type DryRunRebaseOutput struct{ Body DryRunRebaseResponse }

type RevertRequest struct {
	CommitSHA         string `json:"commitSHA"                   pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	TargetRef         string `json:"targetRef"                   minLength:"1"`
	ExpectedTargetSHA string `json:"expectedTargetSHA,omitempty" pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Mainline          uint32 `json:"mainline,omitempty"          minimum:"0"`
}

type RevertInput struct {
	RepoID string        `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   RevertRequest `required:"true"`
}

type RevertResponse struct {
	Outcome         RevertOutcome  `enum:"reverted,alreadyUpToDate,conflict"        json:"outcome"`
	NewTipSHA       string         `json:"newTipSHA,omitempty"                      pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	RevertCommitSHA string         `json:"revertCommitSHA,omitempty"                pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	ConflictedPaths []string       `json:"conflictedPaths"`
	ConflictReason  ConflictReason `enum:"treeType,treeMode,blobOverlap,blobBinary" json:"conflictReason,omitempty"`
}

type RevertOutput struct{ Body RevertResponse }

type DryRunRevertRequest struct {
	CommitSHA string `json:"commitSHA"          pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	TargetRef string `json:"targetRef"          minLength:"1"`
	Mainline  uint32 `json:"mainline,omitempty" minimum:"0"`
}

type DryRunRevertInput struct {
	RepoID string              `path:"repoId"   pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   DryRunRevertRequest `required:"true"`
}

type DryRunRevertResponse struct {
	ResolvedTargetSHA string         `json:"resolvedTargetSHA,omitempty"              pattern:"^[0-9a-f]{40}$|^[0-9a-f]{64}$"`
	Outcome           RevertOutcome  `enum:"reverted,alreadyUpToDate,conflict"        json:"outcome"`
	ConflictedPaths   []string       `json:"conflictedPaths"`
	ConflictReason    ConflictReason `enum:"treeType,treeMode,blobOverlap,blobBinary" json:"conflictReason,omitempty"`
}

type DryRunRevertOutput struct{ Body DryRunRevertResponse }
