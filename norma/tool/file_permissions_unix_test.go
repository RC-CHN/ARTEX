//go:build !windows

package tool

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAtomicWritePermissions(t *testing.T) {
	if os.Getenv("ARTEX_TEST_UMASK_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAtomicWritePermissions$")
		cmd.Env = append(os.Environ(), "ARTEX_TEST_UMASK_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permission subprocess: %v\n%s", err, output)
		}
		return
	}
	syscall.Umask(0077)
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write := func(name string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"file_path": name, "content": "contents"})
		r, err := NewWrite().Call(t.Context(), raw, nil)
		if err != nil || r.IsError {
			t.Fatal("write failed", err, r.Flatten())
		}
	}
	write(path)
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("new file ignored umask", info.Mode())
	}
	if err := os.Chmod(path, 0750); err != nil {
		t.Fatal(err)
	}
	write(path)
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0750 {
		t.Fatal("existing permissions changed", info.Mode())
	}
	link := filepath.Join(dir, "link")
	target := filepath.Join(dir, "created-through-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	write(link)
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replaced dangling symlink", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "contents" {
		t.Fatal("missing symlink target", err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(path, 0400); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]string{"file_path": path, "content": "changed"})
		r, err := NewWrite().Call(t.Context(), raw, nil)
		if err == nil && !r.IsError {
			t.Fatal("parent permission bypassed file permissions")
		}
	}
}
