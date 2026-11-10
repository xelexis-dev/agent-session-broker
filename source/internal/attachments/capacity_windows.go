package attachments

import (
	"golang.org/x/sys/windows"
	"os"
)

func diskCapacity(file *os.File) (Capacity, error) {
	name, err := windows.UTF16PtrFromString(file.Name())
	if err != nil {
		return Capacity{}, err
	}
	var capacity Capacity
	err = windows.GetDiskFreeSpaceEx(name, &capacity.Free, &capacity.Total, nil)
	return capacity, err
}
