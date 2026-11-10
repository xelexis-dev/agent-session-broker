package clientstate

import (
	"golang.org/x/sys/windows"
	"runtime"
	"testing"
	"unsafe"
)

// Only the task-owned fixture's DACL changes; restore it before TempDir cleanup.
func fixtureDACL(t *testing.T, path, descriptor string) func() {
	t.Helper()
	old, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal("fixture DACL read")
	}
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		t.Fatal("fixture DACL parse")
	}
	set := func(descriptor *windows.SECURITY_DESCRIPTOR) error {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		// Avoid automatic ACE propagation into existing fixture children.
		r, _, callErr := windows.NewLazySystemDLL("advapi32.dll").NewProc("SetFileSecurityW").Call(uintptr(unsafe.Pointer(name)), uintptr(windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION), uintptr(unsafe.Pointer(descriptor)))
		runtime.KeepAlive(name)
		runtime.KeepAlive(descriptor)
		if r == 0 {
			return callErr
		}
		return nil
	}
	if err := set(sd); err != nil {
		t.Fatal("fixture DACL update")
	}
	restore := func() {
		if err := set(old); err != nil {
			t.Error("fixture DACL restore")
		}
	}
	t.Cleanup(restore)
	return restore
}

func readOnlyStateDirectory(t *testing.T, path string) func() {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("fixture owner")
	}
	sid := u.User.Sid.String()
	// Deny FILE_ADD_FILE while retaining read and ACL-restoration rights.
	return fixtureDACL(t, path, "D:P(D;;0x2;;;"+sid+")(A;;FA;;;"+sid+")")
}

func insecureDevicePath(t *testing.T, path string) func() {
	t.Helper()
	return fixtureDACL(t, path, "D:P(A;;FA;;;WD)")
}
