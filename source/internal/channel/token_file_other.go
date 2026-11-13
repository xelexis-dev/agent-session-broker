//go:build !darwin && !linux

package channel

import "os"

// openTokenFile checks only the file type here: Windows ACLs are not inspected (a documented limit).
func openTokenFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = tokenFileNotRegular
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
