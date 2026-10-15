---
name: targeted-verify
description: 根据已发现的请求、产品或配置线索选择专项工具验证，保存工具版本、对照和结论。适用于 sqlmap、TLS 检查、定向模板验证及带外验证的结果复核。
license: AGPL-3.0-only
compatibility: ARTEX Runner 预装 artex-kit、sqlmap、testssl 和 nuclei；带外 DNS 验证需要另行配置 Interactsh 服务。
---

# 专项验证与复测

通过 `command -v artex-kit`、`artex-kit inventory` 核对 Runner 工具。固定版本工具使用其 `--help`/`-h` 确认选项，任务中不全局更新依赖。

根据线索选择工具：SQL 注入使用 sqlmap 处理具体请求和参数；TLS 配置使用 testssl；产品/CVE 线索使用 nuclei Skill 选择模板；业务权限差异使用 authenticated-verify。为每次执行设置任务约束允许的请求速率、并发和工作量，保存独立结果目录。不要把所有工具都跑一遍作为默认流程。

先记录正常对照与登录条件，限制本轮验证范围。识别工具发现、环境错误、验证观察和最终结论之间的区别。工具报错或未命中不足以证明目标已修复；对照仍可用且原触发条件失效，才支持修复结论。

记录目标、身份引用、工具和模板版本、关键参数、开始/结束状态、原始产物、对照和证明请求。复用现有资产、流量证据、漏洞上报和复测工具；没有真实流量 ID 时使用文件证据，不编造绑定。需要回调时通过配置好的常驻服务分配关联标识，并在 Runner 退出后继续从持久化记录核对迟到事件。
