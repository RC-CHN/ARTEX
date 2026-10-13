package runnerfiles

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
)

// openDirectory pins each component before traversing the next. Merely checking
// Lstat followed by an operation on the original multi-component path allows a
// swapped directory symlink to redirect a write into another writable prefix.
func openDirectory(parent *os.Root, name string, create bool) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err = parent.Mkdir(name, 0755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		before, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrPath
	}
	next, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := next.Stat(".")
	after, afterErr := parent.Lstat(name)
	if err != nil || afterErr != nil || !after.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		next.Close()
		return nil, ErrConflict
	}
	return next, nil
}

// openParent returns a pinned parent and a single basename. Subsequent opens and
// renames must use that basename, never traverse the original path again.
func openParent(m Mount, p string, create bool) (*os.Root, string, error) {
	if !validPath(p) {
		return nil, "", ErrPath
	}
	r, err := os.OpenRoot(m.Source)
	if err != nil {
		return nil, "", err
	}
	if dir := path.Dir(p); dir != "." {
		for _, segment := range strings.Split(dir, "/") {
			next, err := openDirectory(r, segment, create)
			r.Close()
			if err != nil {
				return nil, "", err
			}
			r = next
		}
	}
	return r, path.Base(p), nil
}

func openRegular(r *os.Root, p string) (*os.File, os.FileInfo, error) {
	before, err := r.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, ErrPath
	}
	f, err := r.Open(p)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	after, afterErr := r.Lstat(p)
	if err != nil || afterErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		f.Close()
		return nil, nil, ErrConflict
	}
	// A regular file can still alias data outside this mount through a hard link.
	// Root-based path traversal does not detect that boundary crossing.
	if !singleLink(f, opened) {
		f.Close()
		return nil, nil, ErrPath
	}
	return f, opened, nil
}

func unchanged(r *os.Root, p string, f *os.File, before os.FileInfo, n int64) bool {
	after, err := f.Stat()
	if err != nil {
		return false
	}
	current, err := r.Lstat(p)
	return err == nil && current.Mode().IsRegular() && singleLink(f, after) && os.SameFile(before, current) &&
		n == before.Size() && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime())
}

// Verify the logical parent still refers to the pinned directory after a long
// transfer. A concurrent directory replacement must not publish into stale data.
func sameParent(m Mount, p string, pinned *os.Root) error {
	current, _, err := openParent(m, p, false)
	if err != nil {
		return err
	}
	defer current.Close()
	a, err := pinned.Stat(".")
	if err != nil {
		return err
	}
	b, err := current.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return ErrConflict
	}
	return nil
}
