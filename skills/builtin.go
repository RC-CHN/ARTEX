// Package skills installs bundled instructions into the configured native Skill
// directory, including when only the executable is upgraded or a PVC is reused.
package skills

import (
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"os"
)

//go:embed nuclei/SKILL.md vulnx/SKILL.md web-discovery/SKILL.md authenticated-verify/SKILL.md portable-tools/SKILL.md targeted-verify/SKILL.md protocol-check/SKILL.md ip-lookup/SKILL.md python-environment/SKILL.md resource-creator/SKILL.md
var bundled embed.FS

var builtinNames = []string{"nuclei", "vulnx", "web-discovery", "authenticated-verify", "portable-tools", "targeted-verify", "protocol-check", "ip-lookup", "python-environment", "resource-creator"}

// SeedDefaults publishes missing files atomically without replacing user edits.
// Visibility is seeded separately in PostgreSQL; a disabled skill stays disabled.
func SeedDefaults(dir string) error {
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
	if err := root.Mkdir(name, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()
	if _, err := sub.Lstat("SKILL.md"); err == nil {
		return nil
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
	if err = sub.Link(tmp, "SKILL.md"); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
