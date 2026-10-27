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
	return publishBundled(sub, name, "SKILL.md")
}

// Supporting files are published before the entrypoint. Every file uses an
// atomic no-replace link, except exact bundled predecessors of SKILL.md.
// Existing operator edits survive a restart or upgrade.
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
	upgrade := false
	var predecessor []byte
	if info, err := root.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || path != "SKILL.md" || (name != "resource-creator" && name != "tack") {
			return nil
		}
		predecessor, err = root.ReadFile(path)
		if err != nil {
			return err
		}
		if !bundledPredecessor(name, path, predecessor) {
			return nil
		}
		upgrade = true
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
	if upgrade {
		current, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		if string(current) != string(predecessor) {
			return nil
		}
		return root.Rename(tmp, path)
	}
	if err := root.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// Only exact bundled predecessors may be upgraded; operator edits are retained.
func bundledPredecessor(name, path string, content []byte) bool {
	if path != "SKILL.md" {
		return false
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	switch name {
	case "resource-creator":
		return digest == "02143138496b678d4fe1f7c7299b9e9cceccf87b269296b80eb4482a6cb3b8fb" || digest == "27fa331d8d71becd7ad1e1590d75a94a469bb03b92135a0e0e621734303fb3c5"
	case "tack":
		return digest == "1aff3e28a3a1c5cd651de3911b83d08bd837ce6f1d3ac6b32e337151585645b9"
	}
	return false
}
