---
name: vulnx
description: 使用预装 vulnx 在线检索最新 CVE、查询漏洞详情和筛选与资产产品/版本相关的候选漏洞。用于漏洞情报检索、核对影响范围和为 Nuclei 定向验证选择线索；不会直接扫描目标。
license: AGPL-3.0-only
compatibility: 需要执行环境中的 vulnx 和到 ProjectDiscovery 情报服务的网络连接；默认完整镜像已预装，通过 Bash 调用，无需 MCP。
---

# vulnx 在线漏洞情报

1. 确认资产产品、版本、组件与需要核对的问题。先看当前执行环境的可用工具说明，必要时运行 `command -v vulnx` 和 `vulnx version --disable-update-check`。自定义 Runner 缺少工具时明确报告，选择可用环境或已有在线搜索工具，不默认安装到宿主机。
2. 用实际产品关键词查询在线数据，控制结果数量；再按具体 CVE 查询详情。占位 CVE 必须替换为真实查询结果。

```sh
vulnx search '产品关键词' --limit 10 --json --silent --disable-update-check --timeout 30s
vulnx id CVE-YYYY-NNNN --json --silent --disable-update-check --timeout 30s
```

`vulnx search --help` 提供 `--product`、`--vendor`、`--severity`、`--kev`、`--template` 等筛选条件；只使用当前版本支持的参数，不猜测查询语法。部分命令初始化也需要网络。需要落盘时为每次查询创建独立目录，`--output` 对已存在文件会报错，避免重复使用同一输出名。

```sh
run_dir=$(mktemp -d "$PWD/vulnx-results.XXXXXX")
vulnx search '产品关键词' --limit 10 --output "$run_dir/candidates.json" \
  --silent --disable-update-check --timeout 30s
```

3. 核对返回信息中的厂商/产品、受影响版本、补丁、利用前提和参考链接；保留检索时间、CVE ID 与信息来源。优先查看厂商公告等原始证据。没有结果、服务报错或限流都不能推出“没有漏洞”。

在线查询优先，不要求维护本地 CVE 数据库。无 API key 可能限流；遇到 429 有限退避，必要时说明需通过正常配置提供凭据，不显示密钥或反复尝试认证。不要在日志或共享产物里存放认证信息。

默认直连。若额外环境提示词说明了可用代理且访问受限，按需给当前命令添加 `--proxy '<环境说明中的 HTTP 代理 URL>'`；该版本帮助说明的是 HTTP 代理，不把 SOCKS 地址当作 HTTP 地址。不修改全局代理环境或其他工具配置。

查询只产生候选线索，不自动证明资产存在漏洞。需要验证时加载 `nuclei` Skill，确认目标范围和模板再执行；Nuclei 的本地模板版本与在线 CVE 数据更新时间不同，可能没有对应模板。
