package gitrepo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParsePushReflog(t *testing.T) {
	t.Parallel()
	out := "aaa\trefs/remotes/origin/x@{200}\tupdate by push\n" +
		"bbb\trefs/remotes/origin/x@{150}\tfetch: fast-forward\n" +
		"ccc\trefs/remotes/origin/x@{120}\tupdate by push\n" +
		"ddd\trefs/remotes/origin/x@{100}\tupdate by push\n" +
		"garbage line\n"
	assert.Equal(t, []string{"aaa", "ccc"}, parsePushReflog(out, time.Unix(120, 0)))
	assert.Empty(t, parsePushReflog(out, time.Unix(201, 0)))
	assert.Empty(t, parsePushReflog("", time.Unix(0, 0)))
}
