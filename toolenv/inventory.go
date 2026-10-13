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
		lines = append(lines, "- vulnx（"+version(i.Vulnx.Version)+"）：在线检索 CVE。可用 vulnx search '<产品关键词>' --json 或 vulnx id <CVE编号> --json；查询需要联网，无 API key 时可能限流。需要代理时显式传 --proxy，代理地址取自环境说明。")
	}
	if i.Nuclei != nil {
		line := "- nuclei（" + version(i.Nuclei.Version) + "）：按模板检测目标。可先 nuclei -tl -tags cve 查看模板，再对任务范围内的目标运行 nuclei -u <URL> -id <模板ID> -jsonl。"
		if i.Nuclei.Templates != "" {
			line += "内置模板版本 " + version(i.Nuclei.Templates) + "，默认不自动更新；可用 -t 指定工作目录中的其他模板。"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n【当前执行环境可用工具】\n" + strings.Join(lines, "\n") + "\n产品/版本匹配只是候选线索；依据实际检测证据判断是否存在漏洞。工具可通过 Bash 调用。"
}
