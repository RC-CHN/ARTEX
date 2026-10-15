---
name: portable-tools
description: 查询多架构工具包、校验并按需分发到已有 SSH/SFTP 会话对应的主机，维护跨主机工具状态与清理回执。适用于目标缺少诊断或验证工具、不同系统架构适配及跨主机测试准备。
license: AGPL-3.0-only
compatibility: Runner 中的 artex-kit；SFTP 分发需要预装 Paramiko、已有 SSH 身份和可信 known_hosts。
---

# 便携工具与跨主机上下文

先运行 `command -v artex-kit`、`artex-kit inventory`、`artex-kit catalog`。Runner 的架构不等于目标架构；从已有会话确认目标 OS、CPU、运行库和可写位置，显式选择 `--platform`。目录有包不代表它在每个内核/运行库上都已验证。官方目录提供 Linux amd64/arm64、Windows amd64 的固定工具包；独立交叉编译目录还提供 ffuf、yq、Chisel 的 ARMv5/v6/v7、MIPS 大小端/32/64 位、PPC64、RISC-V、s390x、LoongArch 等包。

默认完整镜像已在 `/opt/artex-kit/portable/` 内置 ffuf、yq、Chisel 的全部 60 个压缩包（20 个平台），默认目录已合并这些架构，可断网取包。先检查该目录的 `build-report.json`；编译成功与实际设备执行通过是不同状态。自定义镜像以实际清单为准，外部仓库可通过 `--catalog /挂载路径/catalog.lock.json` 指定。

默认镜像缓存本机架构的命令，其他内置架构按需解压到私有缓存。用 `artex-kit fetch ffuf --platform linux/mipsle --offline` 验证离线取包；原始压缩包保持只读，不把整库复制进任务目录。其余工具的平台以目录为准，未内置的官方制品需预热，或明确使用 `artex-kit --cache CACHE fetch TOOL --platform PLATFORM` 下载。

## 工具与平台选择

- `ffuf`：定向 HTTP 路径或参数发现；先建立不存在路径的对照，设置速率与本次工作量。
- `yq`：本地 YAML/JSON/XML 等结构化数据读取与转换；适合目标端缺少 Python/jq 的场景。
- `chisel`：任务确实需要的 TCP/UDP 转发；记录入口、出口、监听地址、认证引用、进程和回收步骤，先核对 `--help`。分发命令本身不会启动它。

镜像内这三种工具各提供以下平台。参数是目录中的精确键：

| OS | `--platform` 可选值 |
| --- | --- |
| Linux x86/ARM | `linux/386`、`linux/amd64`、`linux/arm64`、`linux/armv5`、`linux/armv6`、`linux/armv7` |
| Linux MIPS | `linux/mips`、`linux/mipsle`、`linux/mips64`、`linux/mips64le` |
| Linux 其他 | `linux/ppc64`、`linux/ppc64le`、`linux/riscv64`、`linux/s390x`、`linux/loong64` |
| Windows | `windows/386`、`windows/amd64`、`windows/arm64` |
| macOS | `darwin/amd64`、`darwin/arm64` |

常见 `x86_64` 对应 amd64，`aarch64` 对应 arm64；MIPS/PPC 必须确认位数和大小端，不能只看架构名称猜。ARMv5/v6/v7 与 MIPS 包使用软浮点。静态 Go 程序不需要目标动态 libc，但仍有最低内核、CPU 和内存要求；QEMU 通过不代表旧路由器固件已经适配。

确认平台后先做离线本地 staging，再转交：

```sh
artex-kit stage yq --platform linux/armv5 --directory ./yq-armv5
# 回执包含版本、平台、文件列表及 SHA-256；先核对，再在目标端尝试 --version。
```

本地交接用 `artex-kit --cache CACHE stage TOOL --platform PLATFORM --directory NEW_DIRECTORY`，它创建独占目录、工具和校验回执。使用现有文件传输工具转交时，核对目标端 SHA-256，并把传输、校验和实际执行状态分开记录。

已有 SSH 身份可直接用 SFTP 分发：

```sh
artex-kit --cache CACHE transfer TOOL --platform linux/amd64 \
  --host HOST --user USER --known-hosts KNOWN_HOSTS --key KEY_FILE \
  --base /tmp --receipt transfer.json
```

该命令只上传并校验，不执行工具、不建立额外远控或持久化。保持主机密钥验证；未知或改变的密钥应通过现有可信渠道核验。凭据用文件/SSH agent 引用，不写进共享回执。目标端只需要 SFTP 能力，不需要 Python；Windows SFTP 路径按服务端的绝对路径填写。

每次跨主机操作记录：主机/资产引用、会话引用、网络路径、目标架构、工具版本与哈希、远端路径、操作状态和输出位置。传输成功不等于执行成功；连接中断后先查回执和已创建路径，再决定重试。

结束时对本地包执行 `artex-kit clean NEW_DIRECTORY/receipt.json`；远端使用 `artex-kit remote-clean transfer.json` 并提供相同 SSH 参数。清理仅删除校验仍一致、且没有其他文件的自建目录；文件被修改或出现额外内容时会停止，保留现场供核对，不递归清空目标目录。
