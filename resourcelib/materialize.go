package resourcelib

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func (s *Store) stage(ctx context.Context, r io.Reader) (string, File, error) {
	var out File
	if err := os.MkdirAll(filepath.Join(s.Root, "staging"), 0700); err != nil {
		return "", out, err
	}
	f, err := os.CreateTemp(filepath.Join(s.Root, "staging"), "upload-")
	if err != nil {
		return "", out, err
	}
	name := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	h := sha256.New()
	out.Size, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(&contextReader{ctx, r}, MaxFileBytes+1))
	if err != nil {
		return "", out, err
	}
	if out.Size > MaxFileBytes {
		return "", out, ErrLimit
	}
	if err = ctx.Err(); err != nil {
		return "", out, err
	}
	if err = f.Sync(); err != nil {
		return "", out, err
	}
	if err = f.Close(); err != nil {
		return "", out, err
	}
	out.Digest = hex.EncodeToString(h.Sum(nil))
	ok = true
	return name, out, nil
}
func blobName(f File) string {
	suffix := "-r"
	if f.Executable {
		suffix = "-x"
	}
	return filepath.Join("blobs", f.Digest+suffix)
}
func (s *Store) BlobPath(f File) string { return filepath.Join(s.Root, blobName(f)) }
func (s *Store) commitBlob(staged string, f File) error {
	if err := os.MkdirAll(filepath.Join(s.Root, "blobs"), 0755); err != nil {
		return err
	}
	mode := os.FileMode(0444)
	if f.Executable {
		mode = 0555
	}
	if err := os.Chmod(staged, mode); err != nil {
		return err
	}
	if err := os.Link(staged, s.BlobPath(f)); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDir(filepath.Join(s.Root, "blobs"))
}
func syncDir(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) VersionPath(v Version) string {
	return filepath.Join(s.Root, "versions", strconv.FormatInt(v.ID, 10))
}

// publishTree uses only the bounded manifest. It never scans the resource library.
// Hard links are private to the library; task working copies are separate inodes.
func (s *Store) prepareTree(ctx context.Context, v Version) (prepared string, err error) {
	parent := filepath.Join(s.Root, "versions")
	if err := os.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".publish-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	for _, f := range v.Files {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		dest := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return "", err
		}
		if err = os.Link(s.BlobPath(f), dest); err != nil {
			return "", err
		}
	}
	// Sync directories bottom-up before exposing the immutable tree.
	var dirs []string
	if err = filepath.WalkDir(tmp, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		return "", err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = syncDir(dirs[i]); err != nil {
			return "", err
		}
	}
	if err = os.Chmod(tmp, 0755); err != nil {
		return "", err
	}
	return tmp, nil
}

func (s *Store) commitTree(tmp string, v Version) error {
	if err := os.Rename(tmp, s.VersionPath(v)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.VersionPath(v)))
}

// Materialize supplies a complete disposable working copy, never a writable
// hard link to global data. An atomic directory rename hides partial copies.
func (s *Store) Materialize(ctx context.Context, v Version, taskRoot string) (string, error) {
	rel := filepath.Join("shared", "resources", fmt.Sprintf("r%d", v.ResourceID), fmt.Sprintf("v%d", v.ID))
	root, err := os.OpenRoot(taskRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if fi, e := root.Lstat(rel); e == nil {
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("resource destination is not a directory")
		}
		return filepath.Join(taskRoot, rel), nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	if err = root.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(filepath.Dir(rel), ".resource-"+hex.EncodeToString(nonce[:]))
	if err = root.Mkdir(tmp, 0755); err != nil {
		return "", err
	}
	defer root.RemoveAll(tmp)
	source, err := os.OpenRoot(s.Root)
	if err != nil {
		return "", err
	}
	defer source.Close()
	for _, f := range v.Files {
		if err = ValidatePath(f.Path); err != nil {
			return "", err
		}
		src, e := source.Open(blobName(f))
		if e != nil {
			return "", e
		}
		mode := os.FileMode(0644)
		if f.Executable {
			mode = 0755
		}
		h := sha256.New()
		_, e = writeTaskFile(ctx, taskRoot, filepath.Join(tmp, filepath.FromSlash(f.Path)), io.TeeReader(src, h), f.Size, mode, func() error {
			if hex.EncodeToString(h.Sum(nil)) != f.Digest {
				return errors.New("resource checksum mismatch")
			}
			return nil
		})
		src.Close()
		if e != nil {
			return "", e
		}
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = root.Rename(tmp, rel); err != nil {
		// Another identical materialization may have completed first.
		if fi, e := root.Lstat(rel); e != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", err
		}
	}
	return filepath.Join(taskRoot, rel), nil
}
func WriteCredential(ctx context.Context, taskRoot string, v Version, name string, data []byte) (string, error) {
	sum := sha256.Sum256([]byte(name))
	rel := filepath.Join("shared", "resource-credentials", fmt.Sprintf("v%d", v.ID), hex.EncodeToString(sum[:])+".txt")
	return writeTaskFile(ctx, taskRoot, rel, bytes.NewReader(data), int64(len(data)), 0600, nil)
}
func writeTaskFile(ctx context.Context, taskRoot, rel string, reader io.Reader, size int64, mode os.FileMode, verify func() error) (string, error) {
	root, err := os.OpenRoot(taskRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err = root.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(filepath.Dir(rel), ".resource-"+hex.EncodeToString(nonce[:]))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	defer root.Remove(tmp)
	n, err := io.Copy(f, io.LimitReader(&contextReader{ctx, reader}, size+1))
	if err == nil && n != size {
		err = errors.New("resource size mismatch")
	}
	if err == nil && verify != nil {
		err = verify()
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = root.Rename(tmp, rel); err != nil {
		return "", err
	}
	return filepath.Join(taskRoot, rel), nil
}

// OpenTaskFile rejects every symlink component and confines source reads even
// when another process renames components during the operation.
func OpenTaskFile(rootDir, p string) (*os.File, error) {
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(rootDir, p)
		if err != nil {
			return nil, err
		}
		p = filepath.ToSlash(rel)
	}
	if err := ValidatePath(p); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	prefix := ""
	for _, part := range strings.Split(p, "/") {
		prefix = filepath.Join(prefix, part)
		fi, e := root.Lstat(prefix)
		if e != nil {
			return nil, e
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlink sources are not accepted")
		}
	}
	f, err := root.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("source must be a regular file")
	}
	return f, nil
}
