---
name: web-discovery
description: 合并 Web 服务探测、爬取、前端分析和定向目录发现，生成带来源的接口覆盖清单。适用于入口或参数尚不完整的 Web/API 测试；已有真实会话的权限验证使用 authenticated-verify。
license: AGPL-3.0-only
compatibility: ARTEX Runner 的 httpx、katana、ffuf 和 artex-kit；通过 Bash 调用。
---

# Web 接口发现

先运行 `command -v artex-kit`、`artex-kit inventory`。自定义 Runner 缺少工具时说明缺失能力；默认镜像已锁定工具版本，不在任务中全局安装或升级。

从任务范围和已有资产开始，复用已有流量与接口文档。使用 httpx 获取服务特征，Katana 爬取链接与 JavaScript 接口；需要目录或参数枚举时，再根据技术特征选择小型字典调用 ffuf。先读取所用工具的 `-h`，显式设置范围、低并发、速率、深度和本次工作时长，结果写入独立目录。各进程的速率是局部限制，不能声称这些 CLI 已受 artex-kit 协调器的全局 HTTP 预算约束。

ffuf 先建立随机不存在路径的对照，检查假 404、统一登录页和动态响应长度；HTTP 200、长度差异和产品指纹都只是线索。记录方法、规范化路径、参数位置、发现来源、身份、工具版本与原始结果路径，按方法和路径去重；需要保留对象 ID 的归属关系时，不因路径归一化丢掉它。

区分 `static`、`mock-runtime`、`authenticated-observation` 和 `verified`。api-recon 的 mock 运行结果只证明前端生成了请求，不能证明后端允许访问。对有价值的候选接口用真实会话验证，再通过当前 Agent 可用的资产工具保存；复用真实流量 ID，不能编造证据引用。

把未覆盖、受阻和已探测项目分开记录。交接时给出候选接口与下一步验证依据，避免把整个扫描日志塞进模型上下文。
