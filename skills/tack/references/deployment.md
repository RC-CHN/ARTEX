# 部署与生命周期

Tack 0.1.2 的 `tack-control` 同进程提供管理端和可选 Chisel 服务端，`tack-node` 同进程提供命令执行和可选 Chisel 客户端。两条连接分别认证；管理端默认配置不自动开启隧道。每个节点一套身份和独占状态目录。

先选择并记录控制端、目标节点、账号、实际程序/配置/状态/工作路径、存储是否持久、启动机制、端口和回收方式。长期服务使用任务指定的稳定环境；临时 Runner 生命周期结束会销毁其进程。根据系统现有服务管理器准备启动定义，先完成前台验证。

## 配置管理连接

示例路径按本次环境替换；provision 只写配置，不在远端创建目录。

```sh
umask 077
tack provision -config controller.json -node-config node-a.json \
  -node node-a -server https://control.example.test:8443 \
  -state-dir /chosen/state -work-dir /chosen/work
tack-control -config controller.json -state /chosen/controller/tasks.json \
  -listen 0.0.0.0:8443 -admin-listen 127.0.0.1:8081 \
  -tls-cert /chosen/fullchain.pem -tls-key /chosen/private-key.pem
tack-node -config /chosen/node-a.json
```

Node 的 state_dir/work_dir 必须预先创建为绝对可写目录。只转交该节点配置，不转交 controller.json。普通 Agent 使用 `--credential` 操作凭据文件，包含 token；管理员可用 `operator-set` 授予节点白名单、read/execute 权限，详见操作说明。不同机器上的 Runner 需使用既定私有接入方式访问回环管理员接口。

私有 CA 使用 provision 的 `-node-ca` 和 CLI 的 `--ca`；Node 保留证书验证。纯本机合成测试可以显式启用 allow_insecure_loopback，不能拿它替代远端 HTTPS。

## 可选隧道

用 `tack-control -tunnel-keygen /chosen/tunnel.key` 创建独占密钥文件。控制端配置增加：

```json
{
  "tunnel": {
    "listen": "0.0.0.0:8444",
    "key_file": "/chosen/tunnel.key",
    "auth_file": "/chosen/tunnel-users.json",
    "tls_cert": "/chosen/fullchain.pem",
    "tls_key": "/chosen/private-key.pem",
    "reverse": false,
    "socks5": false
  }
}
```

这只是待合入控制端配置的片段，保留原 admin_token/nodes。隧道账号文件为 `{"node-a:独立随机密码":["^127\\.0\\.0\\.1:9000$"]}`，按实际转发端点缩小规则；不要复用管理 token。正向、反向和 SOCKS 转发的规则语义不同，明确本次用途和网络边界。

用 `tack-control -config controller.json -tunnel-fingerprint` 获取公开 SSH 指纹。节点配置的 tunnel 对象例：

```json
{
  "server": "https://control.example.test:8444",
  "auth": "node-a:独立随机密码",
  "fingerprint": "已核验的公开指纹",
  "remotes": ["127.0.0.1:19000:127.0.0.1:9000"]
}
```

该规则将节点本地 19000 转发至控制端侧 9000。写入私密配置文件后部署；provision 的 `-tunnel-config FILE` 也可复制该对象。节点验证 HTTPS 和 SSH 指纹。配置变更通过更新文件并按运行任务策略重启节点生效。

## 验证、升级和撤除

- 前台确认版本、一个明确节点的无害命令、输出收齐；有隧道时再验证一次实际转发。通过后才配置本次平台的常驻服务。重启/重启机器验证按任务授权范围执行。
- 管理长轮询默认空闲 15–45 分钟，代理读超时需至少 2800 秒；隧道还需 WebSocket 升级。心跳长不等于派发要等半小时，命令会唤醒轮询。
- 默认常规诊断关闭。需要排障时用 `-log-stderr` 配合有界捕获，或 `-log-file PATH -log-max-bytes 4194304 -log-files 4`。诊断日志、任务结果、状态恢复文件分别管理。
- 更新先排空相关工作，备份完整控制端 state、.history、.uploads、.operators.json 以及配置/密钥；节点保留完整 state，包括 journal、results 和 replay。先控制端后节点，绝不通过删除状态解决锁或升级问题。
- 撤除分别停止/禁用具体服务、撤销节点管理凭据与隧道账号、按清单处理已建立隧道会话、删除本部署拥有的文件。operator-revoke 只撤销操作员的新请求，不会撤销节点或取消已接受的命令。隧道账号变更也不自动断开既有连接。
- 登记实际清理验证；未知用 unknown，保留用 retained。记录节点/控制端端点、程序哈希、进程/服务名、目录、凭据引用和剩余项，不记录秘密值。

来源：[统一控制端](https://github.com/RC-CHN/tack/blob/tack-v0.1.2/docs/unified-control.md)、[统一节点](https://github.com/RC-CHN/tack/blob/tack-v0.1.2/docs/unified-node.md)。
