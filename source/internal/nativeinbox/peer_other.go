//go:build !darwin && !linux && !windows

package nativeinbox

func socketPeerPID(uintptr) int { return 0 }
