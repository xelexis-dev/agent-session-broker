//go:build !linux && !darwin && !windows

package attachments

import "os"

func diskCapacity(*os.File) (Capacity, error) { return Capacity{}, problem(507, "server-disk-low") }
