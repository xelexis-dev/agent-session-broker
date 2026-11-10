//go:build !windows

package clientstate

var replaceBlocked = func(error) bool { return false }
