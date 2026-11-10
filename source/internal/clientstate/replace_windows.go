package clientstate

import (
	"errors"
	"golang.org/x/sys/windows"
)

var replaceBlocked = func(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
