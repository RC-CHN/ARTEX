---
name: tack
description: 使用预装 Tack 管理指定远端节点、异步执行命令与查询结果，选择多平台节点包，部署验证命令和隧道连接并记录撤除。适用于已有 Tack 控制端的操作，或任务要求建立和维护 Tack 部署。
license: AGPL-3.0-only; bundled Tack material MIT
compatibility: ARTEX Runner 的 Bash、Python 3、artex-kit；完整镜像内置 Tack 0.1.2 CLI 和全平台发布包，自定义 Runner 需检查实际能力。
---

# Tack

本 Skill 的参考文件与脚本位于 `${SKILL_DIR}`；以下相对路径以该目录为起点，不以当前工作目录为起点。

先执行 `command -v tack`、`tack version` 和 `artex-kit inventory`。默认完整镜像提供 `tack`、`tack-control`、`tack-node`；全平台原包、发布清单和校验值位于 `/opt/artex-kit/tack/`。制品存在不代表已配置或启动服务。

Tack 控制端/节点是独立进程，Agent 通过 Bash 调用 CLI。先明确控制端、操作凭据文件和目标 node ID；已有连接从用户配置或已领取的资源说明取得。操作凭据用 `--credential FILE`，不把 token 写进命令或输出。管理接口默认只接受控制端本机回环连接；不同 Runner 的 localhost 不是同一台机器，需要按已有部署使用 SSH 转发或在控制端执行 CLI。

## 选择本次工作

- **已有节点执行与排障**：读取 [references/operations.md](references/operations.md)，包含准确命令、幂等重试、等待、取消及输出解释。
- **选包和跨平台分发**：读取 [references/platforms.md](references/platforms.md)，运行附带选包脚本，再用 `artex-kit` 离线取包。
- **安装控制端、节点或隧道，升级与撤除**：读取 [references/deployment.md](references/deployment.md)。长期服务部署在任务指定的稳定主机，记录实际路径和生命周期。

## 操作语义

1. 每次新命令使用独立 JSON 请求文件。`tack submit --file FILE` 会先把 `request_id` 原子写回该文件，再发请求。保留文件和返回的任务 ID；回执丢失时重试同一文件。修改请求而沿用 ID 会冲突；`request_retired` 或 `unknown` 需要核实原结果，不能换 ID 自动重跑。
2. 提交只是入队，可以继续其他工作，稍后 `get` 或有限时 `wait`。`get` 退出 0 只表示查询成功；判断执行结果可用 `wait --check`。等待超时退出 4 不会取消任务。取消用 `cancel`，随后查到终态才能确认。
3. 同时读取 `state`、`cancel_requested`、`result_pending`、`result.exit_code`、错误和截断标志。`succeeded` 且 `result_pending=true` 仍需等待输出；清理故障看独立 `result.cleanup`，不会把已成功命令变成需要重跑的命令。Agent 推荐 `--format json`，保留原始字节用输出文件。
4. 节点 `online` 仅表示租约未过期；结合 `status.management`、`accepting_tasks`、积压原因和时间戳判断。隧道 `connected` 仅证明握手，通过一次实际端点传输验证用途。
5. 命令独立执行，一节点同时执行一个命令。显式指定解释器、必要工作目录和环境；上一条命令的 `cd` 或变量不会传到下一条。默认临时工作目录会清理，需保留的文件写到本次部署约定的位置。

连接入口和可复用材料可登记自有资源；每个实际部署分别用 `manage_deployments` 记录节点 ID、进程/服务、路径、凭据引用、撤除步骤和验证结果。删除登记不执行远端清理。任务输出和测试结论引用节点与 Tack 任务 ID，避免把另一 Agent 的命令当作自己的结果。
