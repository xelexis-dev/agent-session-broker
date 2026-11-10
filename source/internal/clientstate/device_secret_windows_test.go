//go:build windows

package clientstate

import (
	"golang.org/x/sys/windows"
	"path/filepath"
	"testing"
)

func TestDeviceWindowsCauseDACLAndLock(t *testing.T) {
	for _, target := range []string{"directory", "secret", "lock"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "device-secret")
			if _, _, err := loadDeviceSecretCause(t.Context(), path, true); err != nil {
				t.Fatal("private fixture")
			}
			broad, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
			if err != nil {
				t.Fatal("ACL fixture")
			}
			acl, _, err := broad.DACL()
			if err != nil {
				t.Fatal("ACL fixture")
			}
			name, want := path, "secret-insecure"
			if target == "directory" {
				name = filepath.Dir(path)
				want = "directory-insecure"
			}
			if target == "lock" {
				name = path + ".lock"
				want = "lock-failed"
			}
			if err := windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal("ACL fixture update")
			}
			_, cause, err := loadDeviceSecretCause(t.Context(), path, true)
			if err == nil || cause != want {
				t.Fatal("Windows cause classification", cause)
			}
		})
	}
}

func TestDeviceWindowsUserDACLAndFallback(t *testing.T) {
	sd, err := privateDeviceSD()
	if err != nil {
		t.Fatal("user ACL unavailable")
	}
	if !devicePrivateDACL(sd, sd) {
		t.Fatal("private ACL rejected")
	}
	broad, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	if err != nil {
		t.Fatal("fixture ACL")
	}
	if devicePrivateDACL(broad, sd) {
		t.Fatal("broad ACL accepted")
	}
	path := filepath.Join(t.TempDir(), "x-broker", "device-secret")
	key, err := loadDeviceSecret(t.Context(), path)
	if err != nil || len(key) != 32 {
		t.Fatal("native private secret unavailable")
	}
	t.Setenv("LOCALAPPDATA", "")
	if _, err := deviceSecretPath(); err == nil {
		t.Fatal("missing local app data silently used a different location")
	}
}
