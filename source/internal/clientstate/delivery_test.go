package clientstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDeliveryWatermarkIsScopedMonotonicMetadata(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, n := range []uint64{2, 1, 4, 3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.SaveDeliveryWatermark("scope", "registration", "epoch", n); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	other, err := Open(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.LoadDeliveryWatermark("scope", "registration", "epoch"); err != nil || got != 4 {
		t.Fatal("watermark did not persist monotonically")
	}
	for _, key := range [][3]string{{"scope", "registration", "other-epoch"}, {"scope", "other-registration", "epoch"}, {"other-scope", "registration", "epoch"}} {
		if got, err := other.LoadDeliveryWatermark(key[0], key[1], key[2]); err != nil || got != 0 {
			t.Fatal("watermark crossed its namespace")
		}
	}
	raw, err := os.ReadFile(store.deliveryPath("scope", "registration", "epoch"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1024 || strings.Contains(string(raw), "body") || strings.Contains(string(raw), "secret") {
		t.Fatal("watermark persisted payload or credentials")
	}
	if err := os.WriteFile(store.deliveryPath("scope", "registration", "epoch"), []byte(`{"through":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadDeliveryWatermark("scope", "registration", "epoch"); err == nil {
		t.Fatal("mismatched watermark was trusted")
	}
}

func TestDeleteDeliveryWatermarkExactAndBusy(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range [][3]string{{"scope", "reg", "old"}, {"scope", "reg", "new"}, {"other", "reg", "old"}, {"scope", "other", "old"}} {
		if err := store.SaveDeliveryWatermark(key[0], key[1], key[2], 7); err != nil {
			t.Fatal(err)
		}
	}
	path := store.deliveryPath("scope", "reg", "old")
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDeliveryWatermark("scope", "reg", "old"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.LoadDeliveryWatermark("scope", "reg", "old"); err != nil || got != 7 {
		t.Fatal("busy floor removed")
	}
	unlock()
	if err := store.DeleteDeliveryWatermark("scope", "reg", "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete json remains")
	}
	if info, err := os.Lstat(path + ".lock"); err != nil || !info.Mode().IsRegular() {
		t.Fatal("lock removed")
	}
	for _, key := range [][3]string{{"scope", "reg", "new"}, {"other", "reg", "old"}, {"scope", "other", "old"}} {
		if got, err := store.LoadDeliveryWatermark(key[0], key[1], key[2]); err != nil || got != 7 {
			t.Fatal("unrelated floor removed")
		}
	}
	if err := store.DeleteDeliveryWatermark("scope", "reg", "old"); err != nil {
		t.Fatal("missing target not idempotent")
	}
}

func TestDeleteDeliveryWatermarkConcurrentSave(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		if err := store.SaveDeliveryWatermark("scope", "reg", "epoch", 1); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if err := store.SaveDeliveryWatermark("scope", "reg", "epoch", 9); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := store.DeleteDeliveryWatermark("scope", "reg", "epoch"); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		got, err := store.LoadDeliveryWatermark("scope", "reg", "epoch")
		if err != nil || (got != 0 && got != 9) {
			t.Fatal("concurrent save/delete corrupted floor")
		}
		if _, err := os.Lstat(store.deliveryPath("scope", "reg", "epoch") + ".lock"); err != nil {
			t.Fatal("lock inode removed")
		}
	}
}

func TestDeleteDeliveryWatermarkUnsafePathsSkipped(t *testing.T) {
	for _, kind := range []string{"target-symlink", "directory", "parent-symlink", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.WriteFile(outside, []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			path := store.deliveryPath("scope", "reg", "epoch")
			if kind == "parent-symlink" {
				if err := os.Symlink(filepath.Dir(outside), filepath.Dir(path)); err != nil {
					t.Skip("symlink unavailable")
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "target-symlink":
					if err := os.Symlink(outside, path); err != nil {
						t.Skip("symlink unavailable")
					}
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "lock-symlink":
					if err := store.SaveDeliveryWatermark("scope", "reg", "epoch", 7); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path + ".lock"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path+".lock"); err != nil {
						t.Skip("symlink unavailable")
					}
				}
			}
			if err := store.DeleteDeliveryWatermark("scope", "reg", "epoch"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(outside); err != nil {
				t.Fatal("outside target removed")
			}
			if kind != "parent-symlink" {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("unsafe target removed")
				}
			}
		})
	}
}
