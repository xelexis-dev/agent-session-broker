//go:build !windows

package nativeinbox

import (
	"context"
	"net"
	"path/filepath"
)

func socketPath(value string) (string, error) {
	path, err := stripSocketScheme(value)
	if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", ErrUnavailable
	}
	return path, nil
}

func dialLocal(ctx context.Context, path string) (writeConnection, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
