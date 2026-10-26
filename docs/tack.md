# Tack 预装包与 Agent 验证

完整工具镜像固定包含 Tack 0.1.2 的全部 33 个官方归档：CLI 7 个、统一控制端 3 个、现代节点 13 个、Legacy Linux 节点 10 个。原包、manifest.json 和 SHA256SUMS 共 107,272,902 字节（约 102.3 MiB），位于 `/opt/artex-kit/tack/`。锁文件是 `toolkit/tack.lock.json`，包含发布来源、成员路径和校验值；构建时并行下载并逐项校验，不运行外平台程序。

默认 PATH 提供本机架构的 `tack`、`tack-control`、`tack-node`。`artex-kit fetch/stage` 可断网取出其他平台程序；Legacy 是独立目录名，避免覆盖默认节点程序。仅安装二进制，不自动配置或启动控制端、节点及隧道。

内置 `tack` Skill 包含操作、平台选择、部署与撤除参考，以及离线选包脚本。Agent 按需加载，先确定指定控制端、凭据文件和节点。Skill 特别区分提交和执行完成、等待超时和取消、查询退出码和远端退出码、命令成功和结果收齐；完整说明见 [Skill](../skills/tack/SKILL.md)。启动时补齐整个包的缺失文件，保留用户修改和禁用状态。

## 自动验证

普通 CI 验证制品锁定、离线失败、校验失败、平台拒绝规则，以及 Skill 安装和可见性。镜像测试运行真实发布二进制，覆盖 12 路重复提交只执行一次、身份冲突、操作员节点范围、退出码、有限等待和显式取消。容器以非 root、只读根文件系统、禁用外部网络运行。

真实模型测试是显式运行项，避免普通 CI 消耗 API 配额。它使用原生 ARTEX Agent 会话及 Skill/Bash/Read/Write/Edit 工具；只有宿主的模型客户端能访问模型 API，工具在一次性的隔离容器中运行。API 配置从标准输入进入内存，不复制到容器或报告。测试不读取生产任务。

```sh
CGO_ENABLED=0 go build -o /tmp/artex-tack-ai-probe ./ci/tack-ai
docker build --target security-tools -t artex:tack-review .
# model-config-provider 代表自己的凭据管理器，向 stdout 输出 llm.Config JSON。
model-config-provider | python3 ci/tack-ai-eval.py \
  --image artex:tack-review --probe /tmp/artex-tack-ai-probe \
  --output /tmp/artex-tack-evaluation --repeats 2
```

配置字段采用 `norma/llm.Config`，至少提供 Format、Model、BaseURL、APIKey；BaseURL 应采用该协议的标准基址。测试仅支持 Docker 所在主机能运行的 Linux probe。每轮使用独立控制端、节点、凭据和文件系统，完成后删除容器。

三类任务分别验证幂等重试、等待/取消，以及 Legacy 选包/远端失败。评估器独立查询控制端记录并读取实际产物；本地透明代理只记录 API 方法、路径、状态码和任务状态，避免 Shell 的 PATH 或绝对路径调用影响观测。报告保存模型最终答复、工具名计数、断言和脱敏 API 状态，不保存模型推理、模型凭据或控制端 token。

所有通过项应同时满足模型完成、使用 Skill、无提供方错误以及任务事实断言。脚本非零退出表示至少一轮失败，不能只看模型自述。交叉平台包校验与选包成功不代表在每一种物理设备上验收；目标内核和 CPU 兼容性仍以实际设备为准。

## 2026-10-26 验收记录

真实调用 `deepseek-v4.1-flash`，三种合成任务各独立运行两轮，6/6 通过。测试使用实际内置 Skill 和原生工具；任务状态由评估器再次向真实 Tack 控制端查询，不以模型回答作为唯一依据。

| 场景 | 第一轮 | 第二轮 | 验收事实 |
| --- | --- | --- | --- |
| 幂等重试 | 96.1 秒 / 12 轮 | 124.7 秒 / 21 轮 | 原请求文件持久化身份，两次提交同一任务，marker 只写入一次，完整 stdout 正确 |
| 等待与取消 | 59.1 秒 / 11 轮 | 79.4 秒 / 13 轮 | 250ms 等待退出 4 后仍运行且未请求取消；显式 cancel 后查到 canceled |
| 平台与失败判断 | 113.9 秒 / 17 轮 | 174.7 秒 / 27 轮 | 为 Linux 2.6.32 选择并实际 staging Legacy 包，拒绝 Windows 7；远端 exit 7 正确判为 failed |

[逐轮断言和运行摘要](../benchmarks/tack-ai-20261026.json) 随源码保存。早期评估出现过 Shell 重置 PATH 导致调用记录缺失，那些运行未计为通过；最终验收使用控制端 API 观测并重新完整运行六轮。有限样本验证代表这些场景已跑通，不代表所有模型、目标系统及故障组合均已覆盖。
