package tool

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Failure is machine-readable feedback carried across execution runtimes. The
// textual result also includes it so providers need no special schema support.
type Failure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Executed   bool   `json:"executed"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action"`
}

func Fail(code, message, next string) Result {
	f := &Failure{Code: code, Message: message, NextAction: next}
	b, _ := json.Marshal(f)
	r := Errorf(string(b))
	r.Failure = f
	return r
}

// This is a metadata revision, not a content hash. Reads stay bounded even for
// huge files. Include identity/change time where available, but never access time.
func fileVersion(info os.FileInfo) string {
	if info == nil {
		return "missing"
	}
	s := fmt.Sprintf("%d:%d:%d", info.Size(), info.ModTime().UnixNano(), info.Mode())
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "CreationTime"} {
			if f := v.FieldByName(name); f.IsValid() && f.CanInterface() {
				s += fmt.Sprintf(":%v", f.Interface())
			}
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func currentFileVersion(path string) (string, os.FileInfo, error) {
	i, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !i.Mode().IsRegular() {
		return "", nil, fmt.Errorf("not a regular file: %s", path)
	}
	return fileVersion(i), i, nil
}

// Stable sidecar locks serialize cooperating file tools across processes and
// shared mounts. Never unlink the lock inode: queued writers may hold it open.
func lockFile(ctx context.Context, path string) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	for links := 0; ; links++ {
		if links >= 40 {
			return "", nil, errors.New("too many symbolic links")
		}
		if resolved, e := filepath.EvalSymlinks(abs); e == nil {
			abs = resolved
			break
		} else if !errors.Is(e, os.ErrNotExist) {
			return "", nil, e
		}
		// EvalSymlinks rejects a dangling final target. Write still follows
		// that link when creating the target, just as os.WriteFile does.
		info, e := os.Lstat(abs)
		if e != nil || info.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, e := os.Readlink(abs)
		if e != nil {
			return "", nil, e
		}
		if filepath.IsAbs(target) {
			abs = target
		} else {
			abs = filepath.Join(filepath.Dir(abs), target)
		}
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", nil, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	key := sha256.Sum256([]byte(filepath.Base(abs)))
	f, err := os.OpenFile(filepath.Join(parent, fmt.Sprintf(".artex-lock-%x", key[:16])), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	// Contention has a bounded wait even if the calling task has no deadline.
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err = wait.Err(); err != nil {
			f.Close()
			return "", nil, err
		}
		locked, e := tryFileLock(f)
		if e != nil {
			f.Close()
			return "", nil, e
		}
		if locked {
			return abs, func() { unlockFile(f); f.Close() }, nil
		}
		t := time.NewTimer(10 * time.Millisecond)
		select {
		case <-wait.Done():
			t.Stop()
		case <-t.C:
		}
	}
}

var errFileChanged = errors.New("file changed before commit")

// The caller holds lockFile. An external writer which does not use the protocol
// is detected at the last revision check, but cannot be made transactional by us.
func commitFile(ctx context.Context, path, version string, data string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if version != "missing" {
		// A writable parent alone must not bypass an existing file's write
		// permissions when replacing it atomically.
		check, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		if err = check.Close(); err != nil {
			return err
		}
	}
	// OpenFile applies umask to newly created files. Existing files retain
	// their original mode, independent of the current process umask.
	f, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".artex-write-"+rand.Text()), os.O_CREATE|os.O_EXCL|os.O_RDWR, mode.Perm())
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if version != "missing" {
		if err = f.Chmod(mode.Perm()); err != nil {
			return err
		}
	}
	for start := 0; start < len(data); {
		if err = ctx.Err(); err != nil {
			return err
		}
		end := min(start+(64<<10), len(data))
		n, e := io.WriteString(f, data[start:end])
		if e != nil {
			return e
		}
		if n != end-start {
			return io.ErrShortWrite
		}
		start = end
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	current, _, err := currentFileVersion(path)
	if err != nil {
		return err
	}
	if current != version {
		return errFileChanged
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func fileFailure(ctx context.Context, err error) (Result, error) {
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if errors.Is(err, errFileChanged) {
		return Fail("file_conflict", err.Error(), "Read the file again, then rebuild the edit using its current version. Do not overwrite another writer's changes."), nil
	}
	return Fail("file_io_error", err.Error(), "Check the path, permissions and free space. Retry only after correcting the cause."), nil
}

const maxEditBytes = 64 << 20

func readEditFile(ctx context.Context, path string, info os.FileInfo) (string, error) {
	if info == nil {
		return "", os.ErrNotExist
	}
	if info.Size() > maxEditBytes {
		return "", fmt.Errorf("file exceeds the 64 MiB Edit limit; use a bounded streaming transformation")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var out strings.Builder
	out.Grow(int(info.Size()))
	buf := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		n, e := f.Read(buf)
		if out.Len()+n > maxEditBytes {
			return "", fmt.Errorf("file grew beyond Edit limit")
		}
		out.Write(buf[:n])
		if e == io.EOF {
			return out.String(), nil
		}
		if e != nil {
			return "", e
		}
	}
}
