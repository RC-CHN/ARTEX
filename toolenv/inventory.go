// Package toolenv describes tools actually available in an execution process.
package toolenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Tool struct {
	Version   string `json:"version,omitempty"`
	Templates string `json:"templates,omitempty"`
}

type Inventory struct {
	Vulnx  *Tool `json:"vulnx,omitempty"`
	Nuclei *Tool `json:"nuclei,omitempty"`
}

var versionPattern = regexp.MustCompile(`^[a-zA-Z0-9.+_-]{1,64}$`)

func version(value string) string {
	if versionPattern.MatchString(value) {
		return value
	}
	return "未知版本"
}

func Detect(searchPath string) Inventory {
	if searchPath == "" {
		searchPath = os.Getenv("PATH")
	}
	var manifest map[string]struct {
		Version string `json:"version"`
	}
	if b, err := os.ReadFile("/opt/artex-tools/security-tools.lock.json"); err == nil && len(b) <= 32<<10 {
		_ = json.Unmarshal(b, &manifest)
	}
	return detect(searchPath, manifest)
}

func detect(searchPath string, manifest map[string]struct {
	Version string `json:"version"`
}) Inventory {
	find := func(name string) *Tool {
		for _, dir := range filepath.SplitList(searchPath) {
			if !filepath.IsAbs(dir) {
				continue
			}
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				continue
			}
			t := &Tool{}
			if path == "/usr/local/bin/"+name {
				t.Version = manifest[name].Version
				if name == "nuclei" {
					t.Templates = manifest["nuclei-templates"].Version
				}
			}
			return t
		}
		return nil
	}
	return Inventory{Vulnx: find("vulnx"), Nuclei: find("nuclei")}
}

// Prompt uses fixed descriptions; a node cannot supply arbitrary instructions
// through tool names or version strings. Reports are scoped to one sandbox.
func (i Inventory) Prompt() string {
	var lines []string
	if i.Vulnx != nil {
		lines = append(lines, "- vulnx（"+version(i.Vulnx.Version)+"）：在线 CVE 情报查询工具。")
	}
	if i.Nuclei != nil {
		line := "- nuclei（" + version(i.Nuclei.Version) + "）：模板漏洞检测工具。"
		if i.Nuclei.Templates != "" {
			line += "内置模板版本 " + version(i.Nuclei.Templates) + "。"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n【当前执行环境可用工具】\n" + strings.Join(lines, "\n") + "\n可通过 Bash 调用。若 Skill 工具列出了同名技能，先加载该技能获取操作流程；Skill 的可见性以当前 Agent 配置为准。"
}
