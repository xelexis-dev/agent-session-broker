//go:build !darwin

package broker

import "os"

func openRegistrationRoot(root string) (string, func() error, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	return root, func() error { return nil }, nil
}
