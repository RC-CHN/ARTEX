# 命令与结果

本文对应 Tack 0.1.2。以下变量是示例，由实际资源/任务填写，凭据文件只含 `{"token":"..."}`。`--allow-insecure-loopback` 仅供已确认的字面量本机 HTTP 地址；远端 HTTPS 使用证书验证，私有 CA 用 `--ca FILE`。

```sh
tack nodes --server "$CONTROL_URL" --credential "$CREDENTIAL_FILE"
tack tasks --server "$CONTROL_URL" --credential "$CREDENTIAL_FILE" --node "$NODE_ID" --caller-id "$CALLER_ID" --limit 20
```

列表支持 `--state`、`--owner`、`--since`、`--until` 和返回的 `--after` 游标。caller_id 是关联标识，owner 由认证身份产生；分页按任务 ID，不是时间。相同节点的操作员可能看到彼此命令，关联 ID 不构成权限隔离。

## 提交

任务文件位于当前 Agent 私有工作区，文件名对应一次执行。例：

```json
{
  "node_id": "chosen-node",
  "caller_id": "artex-task-worker-run",
  "command": {
    "mode": "exec",
    "program": "/usr/bin/printf",
    "args": ["%s\n", "connection verified"],
    "timeout_seconds": 15,
    "output_limit": 65536
  }
}
```

```sh
tack submit --server "$CONTROL_URL" --credential "$CREDENTIAL_FILE" --file request.json
tack get --server "$CONTROL_URL" --credential "$CREDENTIAL_FILE" --id "$TASK_ID" --format json
tack wait --server "$CONTROL_URL" --credential "$CREDENTIAL_FILE" --id "$TASK_ID" --timeout 30s --check --format json
tack cancel --server "$CONTROL_URL" --credential "$CREDENTIAL_FILE" --id "$TASK_ID" --format json
```

`exec` 使用 program/args；脚本使用 mode/script，可选 posix-sh、portable-sh、bash、zsh、powershell、cmd，先看节点 modes。`dir` 必须是节点上的目录，`env` 覆盖节点继承的环境。指定解释器缺失会明确失败，不会把失败脚本换解释器重跑。

可选 `start_before` 是 RFC3339 绝对时间，限制最晚开始时间；`timeout_seconds` 限制启动准备后的运行时间，取值 1–86400。离线后恢复的过期任务为 expired；旧协议节点不执行带截止时间的命令。两端需时钟一致。

CLI 为新文件生成并保存 ID；回执丢失保留原文件重试，不复制为一个新身份。相同 ID 的不同参数得到 request_conflict；被回收的 ID 得到 request_retired，不能自动创建新执行替代它。幂等提交与节点防重放不能证明外部副作用恰好发生一次。

## 反馈

| 观察 | 如何处理 |
| --- | --- |
| queued / dispatched / running | 已入队 / 已派发 / 已报告执行中；都未完成 |
| cancel_requested=true | 已请求取消，继续查询终态 |
| succeeded / failed | 检查 exit_code、error_code、phase 与输出 |
| timed_out / canceled | 检查部分输出及已发生的副作用 |
| expired | 未在期限内开始，不会启动命令 |
| unknown | 结果不确定，核实现场后才能决定新执行 |
| result_pending=true | 输出未收齐，继续查询原任务 |
| stdout_truncated / stderr_truncated | 仅保留头尾片段，中间丢失；不能视作完整记录 |
| result.cleanup.error_code | 执行与清理分开判断；清理失败不要求重跑命令 |
| transfer.error_code | 持久化/传输故障，结合更新时间；不是命令失败 |

`wait --check`：0 为执行成功且输出收齐；3 为其他终态；4 为等待预算耗尽、远端继续；1 为本地/网络/认证/协议错误。成功但清理失败仍退出 0，需检查 cleanup。`get` 不使用 --check。

`--format json` 将 Base64 转成文本，非 UTF-8 会标注 *_text_lossy；`json-raw` 保留协议字节字段。`--stdout-file FILE --stderr-file FILE` 保存原字节，只在结果存在且输出收齐时写文件。每流上限 1 MiB，截断后文件同样不完整；大文件使用已有传输路径，不通过打印 Base64 伪装成完整传输。

常用错误：program_missing/interpreter_missing（修正程序选择）、process_start_failed（启动失败）、command_exit_nonzero（命令非零退出）、workspace_io_failed（目录/存储问题）、result_write_failed（结果暂留内存）、result_upload_failed（本地结果保留待上传）。错误详情可能含路径，记录时保留必要上下文并避免凭据明文。

来源：[Tack 操作说明](https://github.com/RC-CHN/tack/blob/tack-v0.1.2/docs/task-operations.md)。
