package runnernode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RC-CHN/ARTEX/runnerfiles"
)

// SharedMount is an administrator provisioned mapping, never a host path taken
// from a session. Path is the manager's mount of PVC/SubPath. Keep mappings
// narrow (tasks, skills, drafts), excluding controller databases and credentials.
type SharedMount struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	PVC      string `json:"pvc"`
	SubPath  string `json:"sub_path,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func cleanAbsolute(p string) bool {
	return filepath.IsAbs(p) && p != "/" && filepath.Clean(p) == p && !strings.ContainsAny(p, "\\\x00\n,")
}

func validateSharedMounts(mounts []SharedMount) error {
	if len(mounts) > 32 {
		return fmt.Errorf("shared_mounts exceeds 32 mappings")
	}
	for i, m := range mounts {
		if !cleanAbsolute(m.Source) || !cleanAbsolute(m.Path) || !kubeName.MatchString(m.PVC) || len(m.PVC) > 253 || (m.SubPath != "" && !safeSubPath(m.SubPath)) {
			return fmt.Errorf("invalid shared storage mapping")
		}
		for _, other := range mounts[:i] {
			if within(m.Source, other.Source) || within(other.Source, m.Source) || within(m.Path, other.Path) || within(other.Path, m.Path) {
				return fmt.Errorf("overlapping shared storage mappings")
			}
		}
	}
	return nil
}

// Check only the declared path components; never enumerate task history. The
// kubelet independently pins and validates the subPath before mounting it.
func sharedDirectory(p string) error {
	if !cleanAbsolute(p) {
		return fmt.Errorf("invalid shared directory")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("shared storage directory unavailable: %w", err)
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("shared storage path must contain only real directories")
		}
	}
	return nil
}

func (k *KubernetesExecutor) sharedMount(m runnerfiles.Mount) (SharedMount, string, string, error) {
	if !cleanAbsolute(m.Source) {
		return SharedMount{}, "", "", fmt.Errorf("invalid shared source")
	}
	for _, mapping := range k.Config.SharedMounts {
		if !within(mapping.Source, m.Source) {
			continue
		}
		if mapping.ReadOnly && len(m.Writable) > 0 {
			return SharedMount{}, "", "", fmt.Errorf("shared mapping is read-only")
		}
		rel, err := filepath.Rel(mapping.Source, m.Source)
		if err != nil || (rel != "." && !safeSubPath(rel)) {
			return SharedMount{}, "", "", fmt.Errorf("invalid shared source subpath")
		}
		root := filepath.Join(mapping.Path, rel)
		if err := sharedDirectory(root); err != nil {
			return SharedMount{}, "", "", err
		}
		sub := filepath.Join(mapping.SubPath, rel)
		if sub == "." {
			sub = ""
		}
		return mapping, root, sub, nil
	}
	return SharedMount{}, "", "", fmt.Errorf("shared source has no provisioned mapping (snapshot fallback is disabled)")
}

func (k *KubernetesExecutor) sharedRoots(plan runnerfiles.Plan) (map[string]string, error) {
	roots := map[string]string{}
	for _, m := range plan.Mounts {
		_, root, _, err := k.sharedMount(m)
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Name, err)
		}
		roots[m.Name] = root
	}
	if err := k.checkSharedCapacity(roots); err != nil {
		return nil, err
	}
	return roots, nil
}

func (k *KubernetesExecutor) checkSharedCapacity(roots map[string]string) error {
	seen := map[string]bool{}
	bytes, inodes := k.Config.SharedMinFreeBytes, k.Config.SharedMinFreeInodes
	if bytes == 0 {
		bytes = 1 << 30
	}
	if inodes == 0 {
		inodes = 10000
	}
	for _, root := range roots {
		if seen[root] {
			continue
		}
		seen[root] = true
		if err := sharedCapacity(root, bytes, inodes); err != nil {
			return err
		}
	}
	return nil
}

func sharedWritable(root, prefix string) error {
	if !safeSubPath(prefix) {
		return fmt.Errorf("invalid shared writable prefix")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	// os.Root prevents a concurrent path replacement from creating directories
	// outside this task. Refuse links even if they point inside the same root.
	if err := r.MkdirAll(prefix, 0770); err != nil {
		return err
	}
	return sharedDirectory(filepath.Join(root, prefix))
}
