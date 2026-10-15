---
name: protocol-check
description: 使用 grpcurl 和 websocat 调用、调试与复测 gRPC/WebSocket 接口，处理 schema、认证、TLS 和消息流并保存真实证据。适用于已知服务或连接入口的协议验证。
license: AGPL-3.0-only
compatibility: ARTEX Runner 的 Bash、grpcurl、websocat、timeout；默认完整镜像已安装，其他 Runner 需检查实际能力。
---

# gRPC 与 WebSocket 验证

先执行 `command -v grpcurl`、`command -v websocat`，用 `grpcurl -version`、`websocat --version` 或 `artex-kit inventory` 核对当前 Runner。缺少工具时报告能力缺口；不要因 Skill 存在就假定已安装。具体参数以已安装版本的 `grpcurl -help`、`websocat --help=full` 为准。

复用任务已有的服务地址、接口文档、身份引用和正常请求。明确本次 RPC 或消息是否改变状态、流是否长期保持，以及预期响应和结束条件。为每次调用设置时限和消息数量；这些 CLI 不经过 artex-kit 协调器，不自动受其 HTTP 请求预算限制。写操作或结果未知的中断调用不自动重试。

## gRPC

- grpcurl 的地址是 `host:port`，不是 HTTP URL。默认使用 TLS；只有已确认的明文 HTTP/2 服务才加 `-plaintext`。私有 CA 用 `-cacert`，双向 TLS 用 `-cert`、`-key`；证书错误先检查 CA、主机名和 `-authority`，不把 `-insecure` 当默认修复。
- 已开启反射时，可用 `list`、`describe fully.qualified.Service` 获取接口。反射未开启不代表没有服务或存在漏洞；改用已有 `.proto`（`-import-path`、`-proto`）或 descriptor set（`-protoset`），没有 schema 则记录阻塞原因，不猜测字段编码。
- 调用使用 `package.Service/Method`。JSON 请求通过 `-d @` 从文件/stdin 读取。字段遵循 protobuf JSON 映射：bytes 使用 Base64；64 位整数优先用字符串保留精度。流式 RPC 根据 schema 提供多条 JSON 消息并显式结束输入。
- `-H` 发送 metadata。需要环境变量凭据时，使用 `-expand-headers -H 'Authorization: Bearer ${TOKEN}'`，避免把令牌展开到命令行。令牌来自已有私有身份配置，不写入结果摘要。

以下变量指向本次已确认的服务和 schema；先设 `umask 077`，在独立结果目录执行：

```bash
grpcurl -connect-timeout 5 -max-time 20 -cacert "$CA_FILE" \
  -import-path "$PROTO_DIR" -proto service.proto \
  -d @ "$GRPC_ADDRESS" package.Service/Method \
  < request.json > response.json 2> grpc.stderr
status=$?
printf '%s\n' "$status" > grpc.exit
```

记录 gRPC status 和业务响应。HTTP/2 连接成功或 HTTP 200 不代表 RPC 成功；`Unauthenticated`、`PermissionDenied`、`Unimplemented`、`DeadlineExceeded` 需要分别解释。权限验证复用同一请求，明确每次身份与对象归属，避免把两个不同条件的响应直接比较。

## WebSocket

- 使用完整 `ws://` 或 `wss://` URL，保留路径和查询参数。按实际握手要求设置 `--origin`、`--protocol` 和 Cookie/Authorization；子协议不是任意字符串。`-H='Name: value'` 使用等号，避免可变参数吞掉后面的 URL。
- `-t` 发送文本消息；`-b` 发送二进制消息，先确定应用编码及消息边界。命令行字节流与 WebSocket 帧不是同一概念；不要默认把一行文本当作任意二进制协议消息。
- 一次文本往返可用以下形式；服务需要多步登录、订阅或持续推送时改用有界会话，不能沿用 `-1`。`-n` 在 stdin EOF 后继续等待响应，外层 `timeout` 保证最终退出。

```bash
timeout --signal=TERM --kill-after=2s 20s \
  websocat -t -n -1 "$WS_URL" \
  < message.txt > ws.response 2> ws.stderr
status=$?
printf '%s\n' "$status" > ws.exit
```

`wss` 使用镜像信任库；私有 CA 可为本次进程设置 `SSL_CERT_FILE` 指向 PEM CA 文件。不要默认使用 `-k` 跳过验证。握手凭据可能出现在命令参数或诊断输出中，只在当前私有执行目录处理，分享前脱敏。

101 握手成功只证明连接升级成功，不证明消息被业务接受；连接关闭、应用错误消息、无响应和超时分别记录。`timeout` 返回 124 表示达到时限，不能据此认定目标没有问题。主动重连是否重发消息取决于业务幂等性，默认不重放状态未知的消息。

## 证据与交接

保存工具版本、服务/RPC 或 WebSocket 路径、schema 版本、身份引用、TLS/子协议设置、请求和响应文件、退出码及时间。对外摘要脱敏 Cookie、令牌和 URL 中的凭据；无需把完整流放进模型上下文。通过当前 Agent 可用的资产和漏洞工具关联证据；只有确实被流量系统捕获才引用真实流量 ID，否则使用文件证据。

将协议观察、工具失败与最终验证结论分开。复测时使用相同 schema、身份和消息序列，并核对正常对照仍然可用。清理本轮启动的客户端及临时服务，保留需要交接的结果文件。
