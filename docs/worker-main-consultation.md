# Worker 异步请示主 Agent

Worker 调用 `ask_main_agent(question, context)` 后立即收到 `request_id` 与排队回执，继续可独立完成的工作。主 Agent 自动处理请求，可以读取记录、查证、向其他 Worker 协调，最终调用 `reply_to_worker(request_id, answer)`。回复经现有工具执行/停止边界注入 Worker，不要求 Worker 轮询或让出执行槽。

## 使用与界面

主会话顶部的「协作请示」状态条展示进行中的数量。侧边抽屉区分进行中和已处理，可以查看问题、证据、处理状态和答复，跳转原 Worker，也可以人工接答。主对话保留 Worker 提问、主 Agent 的工具过程和答复；人工回复与系统失败回执有独立来源标记。

状态为 `pending → processing → answered → delivered`；任务或意图终止时，尚未送达的请求转为 `cancelled`。`answered` 表示回复已保存；`delivered` 表示已在 Worker 的持久会话中确认该回复，**不表示 Worker 已完成答复里的建议**。确认发生在后续工具边界、执行结束或恢复执行时。并行批次中已经开始的工具不会被追溯撤销。

人工编辑过程中若主 Agent 先答复，草稿保留，发送按钮禁用。服务端通过事务决定第一个有效答复；后来提交的不同答复不能覆盖。失败的人工提交同样保留草稿。

## 持久性和并发边界

- 问题、所属意图、主会话分段与活动通知在同一 PostgreSQL 事务提交；同一工具调用编号重放返回原回执，编号复用于不同问题会失败。
- 每个意图最多一个尚未答复的请求；每个任务最多 32 个未完成请求（含已回复待送达）。问题、上下文、答复分别限制为 8,000 / 24,000 / 32,000 字节。
- 数据库写入遵循任务、意图、请求的锁顺序。没有任务记录的独立 exploration 使用其自身行锁限制队列容量。主 Agent 和人工聊天共享现有的每任务串行入口；全局最多同时处理 4 个任务的自动请示。
- 自动轮次有处理代数校验；旧轮次的答复、重试和失败回执不能改写新轮次。相同来源、相同内容的答复允许幂等重放。
- 回复在 Worker 的下一次工具调用或自然停止边界检查。并行工具之间用互斥锁去重；以持久会话的完整回复标记确认送达，不能先更新送达状态再保存消息。启动恢复检查已保存标记，覆盖「消息落盘、回执尚未提交」的崩溃窗口。无待确认答复时不扫描会话文件。
- 意图完成、取消、阻塞、耗尽或删除，以及任务完成/失败/删除，由数据库触发器在同一事务取消待处理或待送达请求；后续重开意图不会复活旧请求。暂停保留请求和答案，恢复后继续。
- 自动处理每轮最多 24 个模型轮次（配置更小时使用配置值）、10 分钟，失败退避 15 秒，最多 3 次模型处理。最后失败或用户主动停止会留下明确的系统回执。暂停/服务退出保留队列；重启恢复 processing，超过 11 分钟的孤立处理记录也可重新领取。已耗尽模型轮次的恢复只补失败回执。
- 不可用任务会被跳过，任务列表按游标轮转，避免队首长期不可用挡住后续任务。没有可用主 Agent 模型时，请求仍保留在队列，可人工答复。
- 问题/答复广播使用稳定活动编号去重；晚于其他活动广播的事务通知仍会发送，SSE 重连游标不倒退。队列也会周期刷新。
- 任务归档包括请求表；恢复时 processing 重新入队，保留问题、答案和送达状态。

本实现沿用 ARTEX 当前单控制器的任务调度和主会话互斥。数据库事务不等于多控制器选主；不能把多个独立调度器同时启动当作已支持的高可用模式。重试也不承诺外部工具副作用的 exactly-once：主 Agent 应复用已保存证据，执行前检查最新状态。

自动请示明确标记为主 Agent 行为，不伪装成人类消息或新增授权。自动轮次移除目标、约束和授权范围修改工具；仍遵守已有任务约束及执行拦截。需要人类决定时，向 Worker 说明尚未获得决定，不自动批准。

## API

- `GET /api/tasks/{id}/main-requests`：最多 100 条，未完成请求优先，再按编号倒序展示历史。
- `POST /api/tasks/{id}/main-requests/{request}/reply`：`{"answer":"…"}`，记录为人工回复；已失效/已答复请求返回冲突。
- 主 Agent 工具 `list_main_requests` 可按 `intent_id` 过滤；自动处理轮次只能通过 `reply_to_worker` 答复触发自己的请求，其他协作使用 `steer_work`。

## 验证

Go 测试位于 `agent/main_requests_test.go`、`db/main_requests_test.go`、`server/main_requests_test.go`，使用合成任务和独立 PostgreSQL 数据库，覆盖流式/非流式实际 Agent 循环、提交后继续工作、工具/停止边界回复、并发领取与回复、锁等待取消、队列上限、暂停/停止/关闭、重启去重、旧轮次拒绝、SSE 延迟通知、归档恢复与长历史中未完成请求可见性。

配置专用测试 PostgreSQL 的 `ARTEX_TEST_PG_DSN` 后运行：

```sh
go test ./... -count=1 -timeout=10m
go test -race ./agent ./db ./server -run 'TestMainRequest|TestMainReply' -count=10 -timeout=5m
```

浏览器回归脚本为 `web/tests/main-requests.browser.cjs`，其 fixture 页面是 `web/tests/main-requests-preview.tsx`，不会进入正式路由。验证时在独立工作树临时把 fixture 复制到 `web/src/app/consultation-preview/page.tsx`，运行本机 Next dev（默认端口 4318），然后用安装了 Playwright 和 Chromium 的环境运行脚本。验证后删除临时路由，再构建正式产物。

可用 `ARTEX_UI_TEST_URL` 指定本机预览地址，`ARTEX_PLAYWRIGHT_MODULE` 指定已安装的 Playwright 路径，`ARTEX_UI_TEST_OUTPUT` 指定截图与 JSON 报告目录。所有 API 由脚本合成拦截，不连接真实后端。检查桌面/手机布局、深色模式、上下文展开、并发答复保留草稿、HTTP 冲突、重复点击、历史来源、Worker 跳转、键盘关闭和浏览器异常。
