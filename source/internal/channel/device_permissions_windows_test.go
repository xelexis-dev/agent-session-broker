package channel

import (
	"golang.org/x/sys/windows"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"
)

func insecureSecretFixture(t *testing.T, path string) func() {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("fixture owner")
	}
	sid := user.User.Sid.String()
	private := "O:" + sid + "D:P(A;;FA;;;" + sid + ")"
	set := func(name, descriptor string) {
		sd, err := windows.SecurityDescriptorFromString(descriptor)
		if err != nil {
			t.Fatal("fixture DACL parse")
		}
		name16, err := windows.UTF16PtrFromString(name)
		if err != nil {
			t.Fatal("fixture path")
		}
		// Preserve the exact DACL control bits without propagating to children.
		r, _, callErr := windows.NewLazySystemDLL("advapi32.dll").NewProc("SetFileSecurityW").Call(uintptr(unsafe.Pointer(name16)), uintptr(windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION), uintptr(unsafe.Pointer(sd)))
		runtime.KeepAlive(name16)
		runtime.KeepAlive(sd)
		if r == 0 {
			t.Fatal("fixture DACL update", callErr)
		}
	}
	set(filepath.Dir(path), private)
	set(path, "D:P(A;;FA;;;WD)")
	restore := func() { set(path, private) }
	t.Cleanup(restore)
	return restore
}
