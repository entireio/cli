//go:build !windows

package jsonutil

func isRenameContention(error) bool { return false }
