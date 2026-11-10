//go:build !darwin && (!linux || android) && !windows

package attachmentclient

func fullDiskError() error { return ErrUnsupported }
