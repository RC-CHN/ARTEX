// Package recordexport builds immutable, checksummed copies without changing
// the original records. Missing/racing/unrecorded material is always explicit.
package recordexport

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RC-CHN/ARTEX/norma/runtimeobs"
)

var ErrCapacity = errors.New("record export exceeds file/byte capacity")

const MaxFiles = 200000

type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Omission struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type Manifest struct {
	Format      int        `json:"format"`
	TaskIDs     []int64    `json:"task_ids"`
	Cutoff      time.Time  `json:"database_cutoff"`
	CompletedAt time.Time  `json:"completed_at"`
	Redacted    bool       `json:"redacted"`
	Files       []Entry    `json:"files"`
	Omissions   []Omission `json:"omissions"`
	Notes       []string   `json:"notes"`
}
type Builder struct {
	Dir              string
	Manifest         Manifest
	Limit, Bytes     int64
	ctx              context.Context
	seen             map[string]bool
	scanned          int
	omissionOverflow bool
}

func New(ctx context.Context, dir string, ids []int64, redacted bool, limit int64) (*Builder, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Builder{Dir: dir, Limit: limit, ctx: ctx, seen: map[string]bool{}, Manifest: Manifest{Format: 1, TaskIDs: ids, Redacted: redacted, Files: []Entry{}, Omissions: []Omission{}, Notes: []string{"Database tables share one PostgreSQL repeatable-read snapshot. Transcript records are bounded by the same timestamp. Workspace files newer than the cutoff or changed during copy are omitted and listed.", "Only actually retained records are available. Disabled recording, earlier deletion and unavailable remote files cannot be reconstructed.", "Bound traffic evidence belongs to the selected tasks. Legacy host-only traffic has no reliable task attribution and is not included automatically.", "Source data and running task state are never deleted or paused by this export. Completed packages are immutable; retries produce a new snapshot.", "Resource credential vault ciphertext is not exported. Raw task files and dialogues may contain credentials; keep original exports internal."}}}, nil
}
func (b *Builder) Omit(p, reason string) {
	if len(b.Manifest.Omissions) >= MaxFiles {
		b.omissionOverflow = true
		return
	}
	b.Manifest.Omissions = append(b.Manifest.Omissions, Omission{p, reason})
}
func validName(p string) bool {
	return fs.ValidPath(p) && p != "." && !strings.ContainsAny(p, "\\\x00")
}

type entryWriter struct {
	b      *Builder
	f      *os.File
	hash   hash.Hash
	name   string
	n      int64
	closed bool
	err    error
}

func (b *Builder) Open(name string) (io.WriteCloser, error) {
	if !validName(name) || b.seen[name] {
		return nil, fmt.Errorf("invalid or duplicate export path")
	}
	if len(b.seen) >= MaxFiles {
		return nil, ErrCapacity
	}
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	full := filepath.Join(b.Dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	b.seen[name] = true
	return &entryWriter{b: b, f: f, hash: sha256.New(), name: name}, nil
}
func (w *entryWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.b.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	if int64(len(p)) > w.b.Limit-w.b.Bytes {
		w.err = ErrCapacity
		return 0, w.err
	}
	n, err := w.f.Write(p)
	w.b.Bytes += int64(n)
	w.n += int64(n)
	_, _ = w.hash.Write(p[:n])
	w.err = err
	return n, err
}
func (w *entryWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	w.err = errors.Join(w.err, w.f.Close())
	if w.err == nil {
		w.b.Manifest.Files = append(w.b.Manifest.Files, Entry{w.name, w.n, hex.EncodeToString(w.hash.Sum(nil))})
	}
	return w.err
}
func (b *Builder) Write(name string, data []byte) error {
	w, err := b.Open(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return errors.Join(err, w.Close())
}
func (b *Builder) JSON(name string, v any) error {
	w, err := b.Open(name)
	if err != nil {
		return err
	}
	err = json.NewEncoder(w).Encode(v)
	return errors.Join(err, w.Close())
}

// Tree never follows symlinks. OpenRoot prevents file substitution from escaping
// the selected tree; SameFile catches swaps of the selected root itself.
func (b *Builder) Tree(source, prefix string, cutoff time.Time) error {
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		b.Omit(prefix, "source_not_present")
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		b.Omit(prefix, "not_a_regular_directory")
		return nil
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("export root changed")
	}
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		b.scanned++
		if b.scanned > MaxFiles || b.omissionOverflow {
			return ErrCapacity
		}
		if err := b.ctx.Err(); err != nil {
			return err
		}
		dest := path.Join(prefix, name)
		if walkErr != nil {
			b.Omit(dest, "unreadable")
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			b.Omit(dest, "symlink_not_followed")
			return nil
		}
		if !validName(name) {
			b.Omit(dest, "unsupported_filename")
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			b.Omit(dest, "stat_failed")
			return nil
		}
		if !info.Mode().IsRegular() {
			b.Omit(dest, "special_file_not_copied")
			return nil
		}
		if !cutoff.IsZero() && info.ModTime().After(cutoff) {
			b.Omit(dest, "modified_after_snapshot")
			return nil
		}
		return b.copyFile(root, name, dest, info)
	})
}
func (b *Builder) copyFile(root *os.Root, source, dest string, before fs.FileInfo) error {
	file, err := root.Open(source)
	if err != nil {
		b.Omit(dest, "open_failed")
		return nil
	}
	defer file.Close()
	start, err := file.Stat()
	if err != nil || !os.SameFile(start, before) || start.Size() != before.Size() || !start.ModTime().Equal(before.ModTime()) {
		b.Omit(dest, "changed_before_copy")
		return nil
	}
	if b.Manifest.Redacted {
		if before.Size() > 8<<20 {
			b.Omit(dest, "redacted_copy_excludes_files_over_8MiB")
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(file, before.Size()+1))
		if err != nil {
			return err
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
			b.Omit(dest, "redacted_copy_excludes_binary")
			return nil
		}
		after, err := file.Stat()
		if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			b.Omit(dest, "changed_during_copy")
			return nil
		}
		return b.Write(dest, Redact(data))
	}
	w, err := b.Open(dest)
	if err != nil {
		return err
	}
	n, copyErr := io.CopyN(w, file, before.Size())
	closeErr := w.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	after, err := file.Stat()
	if err != nil || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		if err = os.Remove(filepath.Join(b.Dir, filepath.FromSlash(dest))); err != nil {
			return err
		}
		delete(b.seen, dest)
		b.Bytes -= n
		b.Manifest.Files = b.Manifest.Files[:len(b.Manifest.Files)-1]
		b.Omit(dest, "changed_during_copy")
	}
	return nil
}
func (b *Builder) Finish(zipPath string) (checksum string, size int64, err error) {
	start := time.Now()
	defer func() { runtimeobs.Record(runtimeobs.ExportPackage, start, err != nil) }()
	if b.omissionOverflow {
		return "", 0, ErrCapacity
	}
	b.Manifest.CompletedAt = time.Now().UTC()
	sort.Slice(b.Manifest.Files, func(i, j int) bool { return b.Manifest.Files[i].Path < b.Manifest.Files[j].Path })
	// Manifest excludes its own hash; the outer ZIP digest covers it.
	if err = b.JSON("manifest.json", b.Manifest); err != nil {
		return
	}
	f, e := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		err = e
		return
	}
	defer func() {
		if err != nil {
			_ = os.Remove(zipPath)
		}
	}()
	h := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(f, h))
	// The manifest is already the authoritative file list. Avoid walking the
	// staged directory again (especially expensive on shared storage).
	root, openErr := os.OpenRoot(b.Dir)
	if openErr != nil {
		err = openErr
	} else {
		for _, entry := range b.Manifest.Files {
			if err = b.ctx.Err(); err != nil {
				break
			}
			if !validName(entry.Path) {
				err = fmt.Errorf("invalid staged export entry")
				break
			}
			info, e := root.Lstat(entry.Path)
			if e != nil {
				err = e
				break
			}
			if !info.Mode().IsRegular() {
				err = fmt.Errorf("invalid staged export entry")
				break
			}
			file, e := root.Open(entry.Path)
			if e != nil {
				err = e
				break
			}
			head := &zip.FileHeader{Name: entry.Path, Method: zip.Deflate}
			head.SetMode(0600)
			head.SetModTime(b.Manifest.Cutoff)
			w, e := zw.CreateHeader(head)
			if e == nil {
				_, e = io.Copy(w, &contextReader{ctx: b.ctx, r: file})
			}
			err = errors.Join(e, file.Close())
			if err != nil {
				break
			}
		}
		err = errors.Join(err, root.Close())
	}
	err = errors.Join(err, zw.Close(), f.Sync(), f.Close())
	if err != nil {
		return
	}
	info, e := os.Stat(zipPath)
	if e != nil {
		err = e
		return
	}
	size = info.Size()
	checksum = hex.EncodeToString(h.Sum(nil))
	return
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
