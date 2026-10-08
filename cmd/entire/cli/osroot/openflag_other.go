//go:build !unix

package osroot

// Windows and Plan 9 do not expose an os.OpenFile-compatible O_NOFOLLOW.
// OpenFileNoFollow still performs pre-open and post-open identity checks there.
const noFollowOpenFlag = 0
