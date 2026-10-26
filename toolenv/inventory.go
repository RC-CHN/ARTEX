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
	Vulnx   *Tool            `json:"vulnx,omitempty"`
	Nuclei  *Tool            `json:"nuclei,omitempty"`
	Kit     *Tool            `json:"artex_kit,omitempty"`
	HTTPX   *Tool            `json:"httpx,omitempty"`
	Katana  *Tool            `json:"katana,omitempty"`
	FFUF    *Tool            `json:"ffuf,omitempty"`
	SQLMap  *Tool            `json:"sqlmap,omitempty"`
	TestSSL *Tool            `json:"testssl,omitempty"`
	Extras  map[string]*Tool `json:"extras,omitempty"`
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
	dirs := filepath.SplitList(searchPath)
	find := func(name string) *Tool {
		for _, dir := range dirs {
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
	i := Inventory{Vulnx: find("vulnx"), Nuclei: find("nuclei"), Kit: find("artex-kit"),
		HTTPX: find("httpx"), Katana: find("katana"), FFUF: find("ffuf"), SQLMap: find("sqlmap"), TestSSL: find("testssl")}
	for _, name := range []string{"subfinder", "dnsx", "naabu", "tlsx", "yq", "chisel", "grpcurl", "websocat", "tack", "tack-control", "tack-node", "ssh", "sshpass", "rsync", "socat", "tcpdump"} {
		if t := find(name); t != nil {
			if i.Extras == nil {
				i.Extras = make(map[string]*Tool)
			}
			i.Extras[name] = t
		}
	}
	return i
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
	for _, item := range []struct {
		name, description string
		tool              *Tool
	}{
		{"artex-kit", "多架构工具目录、分发和验证工作流；iplookup 离线查询 IP 地理归属/ASN，ipdb status 检查数据库；inventory 报告实际能力。", i.Kit},
		{"httpx", "ProjectDiscovery Web 服务探测 CLI（Python httpx 包另行检查）。", i.HTTPX},
		{"katana", "Web 与 JavaScript 接口爬取。", i.Katana},
		{"ffuf", "目录、参数和虚拟主机发现。", i.FFUF},
		{"sqlmap", "针对具体请求和参数的 SQL 注入验证。", i.SQLMap},
		{"testssl", "TLS 协议与配置检查。", i.TestSSL},
		{"grpcurl", "gRPC 调用与服务描述；支持反射或显式 proto/protoset。", i.Extras["grpcurl"]},
		{"tack", "指定远端节点的异步命令执行与状态查询；完整镜像内置全平台包，加载 tack Skill 了解幂等提交、部署及撤除。", i.Extras["tack"]},
		{"websocat", "WebSocket 文本/二进制消息交互，支持 ws/wss。", i.Extras["websocat"]},
	} {
		if item.tool != nil {
			lines = append(lines, "- "+item.name+"（"+version(item.tool.Version)+"）："+item.description)
		}
	}
	var extras []string
	for _, name := range []string{"subfinder", "dnsx", "naabu", "tlsx", "yq", "chisel", "ssh", "sshpass", "rsync", "socat", "tcpdump"} {
		if i.Extras[name] != nil {
			extras = append(extras, name)
		}
	}
	if len(extras) > 0 {
		lines = append(lines, "- 其他可用命令："+strings.Join(extras, "、")+"。是否具备原始套接字等权限需按当前执行环境核验。")
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n【当前执行环境可用工具】\n" + strings.Join(lines, "\n") + "\n可通过 Bash 调用。若 Skill 工具列出了同名技能，先加载该技能获取操作流程；Skill 的可见性以当前 Agent 配置为准。"
}
