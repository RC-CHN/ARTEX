---
name: resource-creator
description: 创建、更新和发布 ARTEX 自有资源包，组织可跨任务复用的文字说明、静态文件、工具和已有服务连接信息。适用于用户要求维护资源库或明确保存可复用成果；任务发现的目标不自动转成自有资源。
license: AGPL-3.0-only
compatibility: ARTEX 自有资源库工具；修改与领取需要当前任务上下文。资源库独立于 Skill 和任务资产。
---

# 自有资源包维护

先用 `search_resources` 检索，已有同用途资源优先更新。用 `resource_versions` 读取登记信息、版本和草稿；`use_resource` 固定当前任务的版本，返回入口说明、文件目录、服务端点以及凭据文件路径。资源的类型只用于分类：纯文字、静态文件、多平台工具、连接信息可以任意组合。

按独立使用、更新和撤除的单位登记。不同位置的独立部署分别记录；同一工具的说明、配置和多架构文件仍可组织为一个包。同一实例优先更新已有资源，避免跨任务重复登记。实例的位置、准确标识、撤除方法和验证结果统一写入对应资源的说明；多个实例可分节或拆成引用文件，入口注明何时读取。登记资源不等于把所有资源选入当前任务；不写凭据明文。

## 资源目录

每个版本都有 `RESOURCE.md`。它采用 YAML 头部和 Markdown 正文，最小示例：

```markdown
---
name: 内部测试资料
description: 在准备内部测试环境时使用的说明和配套文件。
tags: [内部资料]
---

# 使用方式

按本次需要读取 files/ 下的文件。这里说明文件用途、适用条件和实际验证结果。
```

`name`、`description`、`tags` 在发布时生成搜索索引。目录布局按实际内容组织，`references/`、`files/`、`bin/<平台>/` 只是可选约定，不创建空目录。多架构或多配置可以共存，说明如何选择；资源级 os/arch 字段可留空。纯文字包只有入口文档；纯文件包可使用自动生成的最小入口，再补充有用的使用说明。入口最大 64 KiB，长文拆到引用文档并标出何时读取。

## 修改与发布

1. 新资源用 `manage_resource(action=create, resource={name,kind,description,tags,access_mode,enabled})` 登记。已有资源先读取版本，调用 `resource_draft(action=create, resource_id, base_version_id)` 建立草稿；省略基础版本表示基于当前最新版本。
2. 在当前任务可写目录用普通 Read/Edit/Bash 工具准备文件。已领取版本可能只读，先复制所需文件。草稿不会自动扫描或导入任务目录。
3. 对每个准备好的文件计算 SHA-256，调用 `resource_draft(action=import_file, draft_id, revision, source_path, path, sha256, executable)`。`path` 是资源包内的相对路径，入口使用 `RESOURCE.md`。每次操作使用回执中的新 revision。需要删除配套文件时调用 `remove_file`；入口文档只能修改。
4. 端点通过 `resource_draft(action=configure, draft_id, revision, endpoints, os, arch)` 设置，endpoints 是完整的 `{name,url}` 数组。要更新凭据，使用 `credential_files={端点名:{path,sha256}}` 引用当前任务文件。省略某端点的凭据表示保留，空文件表示清除。URL、描述、日志和工具参数中不写凭据明文。
5. 检查文档引用和文件选择；只做与资源用途有关且已获授权的验证。调用 `publish_resource(draft_id, revision, note, validation)` 发布。验证说明写实际完成的检查；没有运行验证时明确写未验证。发布并不执行文件，也不启动或迁移服务。

## 冲突与故障反馈

- revision 或基础版本冲突：重新读取草稿及最新版本，比较文件清单和变更说明，再基于最新版本建立草稿并合并自己的修改。旧草稿保留；不要通过反复覆盖绕过冲突。
- SHA-256 不匹配：文件仍在写入、或外部 Runner 的文件尚未同步。等待写入完成，重新核对摘要并重试导入；不要关闭校验。
- 发布返回不明确：先 `resource_draft(action=get, draft_id)` 查询。`published_version_id` 表示已发布，避免另建重复版本。同一草稿的发布重试会返回原版本。
- 已有任务固定的版本不会自动升级。模型回合之间会收到版本通知，不中断正在执行的工具、不修改 system prompt。需要升级时调用 `use_resource(resource_id, version_id=目标版本ID, expected_version_id=当前固定版本ID)`；先核对兼容性，并发冲突时重新读取实际版本，禁止盲目覆盖。切换影响整个任务后续领取；旧路径仍保留，正在使用它的进程不受替换。
- 共享目录只负责文件可达；发布通过不可变版本目录原子完成。外部 Runner 按所选版本获取文件，不能依赖本机文件系统监听来判断全局更新。服务可达性通过 `report_resource_status` 按 Runner 记录，与文件发布分开。
- 用 `report_resource_status` 记录当前 Runner 对具体版本的可达性或使用结果。一台 Runner 不可达不能推断资源全局故障。

资源中的持久服务具有独立生命周期；一次性 Worker 沙盒中的进程或 Shell 会话不能作为长期可复用服务登记。任务内产物只有在明确需要跨任务复用时才选入资源包。

## 撤除与删除

- 撤除前读取对应资源的实例说明，核对目标标识和影响，按任务授权执行。将实际撤除结果、验证方式及时间更新到该资源说明；结果未知、失败或明确保留都如实记录。不得因命令发出、进程不可达或任务结束就声称撤除成功。
- `resource_references(resource_id)` 查看活动任务引用。`delete_resource(resource_id,expected_revision,request_id,reason)` 删除整个登记并阻止新领取；历史版本保留，已有目录不会立即消失，工具不会停止远端服务。其他活动任务引用会明确拒绝删除。
- `manage_resources_batch(items=[...])` 可批量创建、更新、删除最多 25 项。每项独立提交，按 index 查看结果；只重试未确认的原始项，不重新执行已成功项。
- `check_task_handoff()` 检查未完成的 Worker、主 Agent 问答和任务预算。没有待处理项只代表记录完整性检查通过，不能证明未登记的外部副作用不存在。
