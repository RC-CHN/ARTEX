// Package skills installs bundled instructions into the configured native Skill
// directory, including when only the executable is upgraded or a PVC is reused.
package skills

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

//go:embed nuclei/SKILL.md vulnx/SKILL.md web-discovery/SKILL.md authenticated-verify/SKILL.md portable-tools/SKILL.md targeted-verify/SKILL.md protocol-check/SKILL.md ip-lookup/SKILL.md python-environment/SKILL.md resource-creator/SKILL.md tack
var bundled embed.FS

var builtinNames = []string{"nuclei", "vulnx", "web-discovery", "authenticated-verify", "portable-tools", "targeted-verify", "protocol-check", "ip-lookup", "python-environment", "resource-creator", "tack"}

// SeedDefaults publishes missing files atomically without replacing user edits.
// Visibility is seeded separately in PostgreSQL; a disabled skill stays disabled.
var seedMu sync.Mutex

func SeedDefaults(dir string) error {
	seedMu.Lock()
	defer seedMu.Unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range builtinNames {
		if err := seed(root, name); err != nil {
			return fmt.Errorf("seed skill %s: %w", name, err)
		}
	}
	return nil
}

func seed(root *os.Root, name string) error {
	if name == "tack" {
		return seedTree(root, name)
	}
	if err := root.Mkdir(name, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()
	upgrade := false
	if info, err := sub.Lstat("SKILL.md"); err == nil {
		// Only replace the exact, unedited bundled predecessor. Custom files
		// and symlinks remain operator-owned. Startup runs before editors open.
		if !info.Mode().IsRegular() || name != "resource-creator" {
			return nil
		}
		old, err := sub.ReadFile("SKILL.md")
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(old)) != "02143138496b678d4fe1f7c7299b9e9cceccf87b269296b80eb4482a6cb3b8fb" {
			return nil
		}
		upgrade = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := bundled.ReadFile(name + "/SKILL.md")
	if err != nil {
		return err
	}
	tmp := ".skill-seed-" + rand.Text()
	f, err := sub.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer sub.Remove(tmp)
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Link is an atomic no-replace publication, also across concurrent starters.
	if upgrade {
		old, err := sub.ReadFile("SKILL.md")
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(old)) != "02143138496b678d4fe1f7c7299b9e9cceccf87b269296b80eb4482a6cb3b8fb" {
			return nil
		}
		return sub.Rename(tmp, "SKILL.md")
	}
	if err = sub.Link(tmp, "SKILL.md"); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// Supporting files are published before the entrypoint. Every file uses an
// atomic no-replace link; existing operator edits survive a restart or upgrade.
func seedTree(root *os.Root, name string) error {
	if err := root.MkdirAll(name, 0755); err != nil {
		return err
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()
	var files []string
	err = fs.WalkDir(bundled, name, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && path != name+"/SKILL.md" {
			files = append(files, path[len(name)+1:])
		}
		return nil
	})
	if err != nil {
		return err
	}
	files = append(files, "SKILL.md")
	for _, path := range files {
		if err := publishBundled(sub, name, path); err != nil {
			return err
		}
	}
	return nil
}

func publishBundled(root *os.Root, name, path string) error {
	if _, err := root.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := root.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := bundled.ReadFile(name + "/" + path)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".skill-seed-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
