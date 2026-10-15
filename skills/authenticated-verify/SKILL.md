---
name: authenticated-verify
description: 使用独立身份和可重放 HTTP 流程验证权限差异、对象归属及业务步骤，并保存基线和验证证据。适用于真实登录态探索、横向或纵向权限测试和复测；mock 前端发现使用 api-recon。
license: AGPL-3.0-only
compatibility: Runner 中的 artex-kit，以及配置好的 ARTEX_KIT_URL 和 ARTEX_KIT_TOKEN_FILE；协调器私有配置维护身份引用。
---

# 身份与流程验证

运行 `command -v artex-kit`、`artex-kit inventory` 确认 Runner 能力。协调器未配置时明确说明缺少什么，不声称身份、共享预算或回调已就绪。凭据留在协调器私有配置中，计划只写身份名称；不要把 Token 文件内容放进终端输出或共享产物。

将当前真实流量转换为一个有 `test`、`revision` 和 `steps` 的 JSON 计划。`revision` 表示测试条件/目标版本；相同计划默认复用已有执行记录。需要重新发请求时显式设置 `repeat: true`，先确认上次是否已经产生状态变更，避免因回执超时重复创建或删除对象。

```json
{
  "test": "object-ownership",
  "revision": "fixture-v1",
  "steps": [
    {"identity": "user-a", "role": "baseline", "request": {"url": "https://target.example/api/objects/fixture-a"}, "expect": {"status": 200, "json": {"/owner": "user-a"}}},
    {"identity": "user-b", "role": "proof", "request": {"url": "https://target.example/api/objects/fixture-a"}, "expect": {"status": 200, "json": {"/owner": "user-a"}}}
  ]
}
```

替换示例目标、对象和身份后执行 `artex-kit verify plan.json --output verification.json`。协调器先做语义登录态检查，必要时按配置刷新；检查、刷新和业务请求共同消耗目标预算。Cookie 在身份之间隔离，重定向不会自动携带身份继续访问。

`request` 支持 method、url、headers、json 或 body；`expect` 支持 status、contains 和 JSON pointer 值比较。`capture: {"object_id": "/id"}` 将响应字段传给后续步骤；URL 中用 `{{object_id|url}}` 编码。创建、修改、验证、回收可建成多步流程；中途失败后根据已保存步骤人工判断回收需要，不盲目重跑整个流程。

`observed` 仅表示计划中的断言满足；`not_observed` 表示观察与断言不符；`inconclusive` 表示条件不足。它们不自动等于漏洞成立、无漏洞或已修复。特别检查对照是否有效、WAF/登录页是否干扰，以及数据归属和实际状态变化；调用现有漏洞上报/复测工具前保留必要证据。协调器摘要包含响应哈希与脱敏片段，不能把它伪装成 ARTEX 流量 ID；需要流量绑定时使用已有采集接口产生的真实 ID。

使用 `artex-kit status RUN_ID` 查看持久化结果与迟到回调。需要 HTTP 带外验证时先为基线执行申请 `artex-kit callback RUN_ID --ttl 3600`，将返回 URL 纳入后续探测计划。收到回调只证明有请求到达，仍需关联探测并排除自身访问；DNS 等协议另需配置自托管 Interactsh。回调协调器的生命周期独立于 Runner。
