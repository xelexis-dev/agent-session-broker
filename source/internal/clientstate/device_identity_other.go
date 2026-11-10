//go:build !darwin && !linux && !windows

package clientstate

import "context"

func loadDeviceSecretPlatform(context.Context, string, bool) ([]byte, string, error) {
	return nil, "path-unavailable", errDeviceIdentity
}
func receiverProcessIdentity(context.Context, int) (string, error) { return "", errDeviceIdentity }
