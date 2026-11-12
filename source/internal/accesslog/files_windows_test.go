//go:build windows

package accesslog

import (
	"context"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccessLogWindowsReaderConflictAndRecovery(t *testing.T) {
	w, dir := testWriter(t)
	if !w.write([]byte("first\n"), time.Now()) {
		t.Fatal("write failed")
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(dir, w.base))
	if err != nil {
		t.Fatal("path fixture failed")
	}
	reader, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal("reader fixture failed")
	}
	now := time.Now()
	if w.rotate(now) == nil {
		windows.CloseHandle(reader)
		t.Fatal("rename ignored reader delete-sharing conflict")
	}
	w.counters.rotationErrors.Add(1)
	w.rotateAt = now.Add(30 * time.Second)
	if w.openActive(true) != nil {
		windows.CloseHandle(reader)
		t.Fatal("fallback append reopen failed")
	}
	windows.CloseHandle(reader)
	w.maintenance(now.Add(31 * time.Second))
	if !w.write([]byte("recovered\n"), now.Add(31*time.Second)) {
		t.Fatal("native rename retry did not recover")
	}
}
func TestAccessLogWindowsACLInheritance(t *testing.T) {
	root := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("token query failed")
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		t.Fatal("ACL fixture failed")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal("ACL fixture failed")
	}
	if windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil) != nil {
		t.Fatal("ACL fixture failed")
	}
	cfg := Defaults()
	cfg.File = "logs/access.log"
	l, err := Open(root, cfg)
	if err != nil {
		t.Fatal("private inherited log open failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l.Close(ctx)
	for _, path := range []string{filepath.Join(root, "logs"), filepath.Join(root, "logs", "access.log")} {
		got, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal("inherited ACL query failed")
		}
		text := got.String()
		if !strings.Contains(text, sid) || !strings.Contains(text, "ID") {
			t.Fatal("restricted data ACL was not inherited")
		}
		if _, err = os.Stat(path); err != nil {
			t.Fatal("inherited path missing")
		}
	}
}
