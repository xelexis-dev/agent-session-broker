//go:build darwin || (linux && !android)

package attachmentclient

import (
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceFIFO(t *testing.T) {
	root := realTemp(t)
	if e := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	code(t, root, "fifo", "", "attachment-not-file")
}

func TestSourceRootAcquisitionReplacement(t *testing.T) {
	root := realTemp(t)
	parent := filepath.Join(root, "send-parent")
	cwd := filepath.Join(parent, "project")
	state := filepath.Join(root, "state-parent", "project")
	put(t, filepath.Join(cwd, "session.json"), []byte("safe-synthetic"))
	put(t, filepath.Join(state, "session.json"), []byte("private-synthetic"))
	file, e := openSource(cwd, "session.json", state, nil, func() {
		if err := os.Rename(parent, parent+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(state), parent); err != nil {
			t.Fatal(err)
		}
	})
	uploaded := 0
	if file != nil {
		data, _ := io.ReadAll(file)
		uploaded = len(data)
		file.Close()
	}
	if e == nil || uploaded != 0 {
		t.Fatal("replacement root exposed excluded source bytes")
	}
}
