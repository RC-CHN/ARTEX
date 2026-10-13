# 流量索引 benchmark（2026-10-13）

此报告记录首次迁移版 `f937c63` 的历史结果；后续搜索优化及测试修复见[优化报告](traffic-index-optimized.md)。

环境：Linux amd64，Intel Xeon CPU Max 9470C；Go 1.26.3，`-cpu=4`；SQLite 驱动 `modernc.org/sqlite v1.52.0`，本地临时文件、WAL 与默认同步设置；PostgreSQL 16.14 独立 Docker 数据卷、经 loopback TCP 连接，`fsync` / `synchronous_commit` / `full_page_writes` 均为 `on`。数据库没有业务流量。

同一 Go API、同一确定性语料：10,000 条记录，100 个主机，4 KiB 正文（不同 SHA-256 文本块），1% 带搜索词。读取数据在写基准前保持不变；建库、语料生成、ANALYZE、采样数组分配及清理不计时。每项 2 秒 × 3 轮，表中取三轮中位数，低者更好。P95 列也是各轮 P95 的中位数，不是合并样本的 P95。

| 操作 | SQLite 平均/次 | PG 平均/次 | PG / SQLite | SQLite P95 | PG P95 |
|---|---:|---:|---:|---:|---:|
| 读取单条报文 | 0.036 ms | 0.103 ms | 2.85× | 0.044 ms | 0.117 ms |
| 主机分页（50 条） | 0.291 ms | 0.160 ms | 0.55× | 0.363 ms | 0.200 ms |
| 主机 + 正文搜索（10 条） | 2.446 ms | 5.714 ms | 2.34× | 2.772 ms | 6.350 ms |
| 正文 + 状态筛选分页（含计数） | 5.099 ms | 15.623 ms | 3.06× | 5.856 ms | 17.174 ms |
| 4 并发读取（摊销耗时） | 0.024 ms | 0.030 ms | 1.23× | — | — |
| 录制 4 KiB 报文 | 3.703 ms | 1.745 ms | 0.47× | 10.094 ms | 1.570 ms |

初始表及索引占用：SQLite 183.46 MiB，PG 170.24 MiB。SQLite 统计索引文件及 WAL/SHM，PG 统计该 schema 全部表的 `pg_total_relation_size`；PG 的集群 WAL、全局目录和服务端缓存不在此数字内。

这些结果用于说明此工作负载的取舍，不能推算生产集群吞吐。SQLite 是进程内本地文件，PG 包含网络及独立进程开销；存储路径也不同。并发读取报告吞吐倒数，不代表单次请求延迟。`B/op` 只统计 Go 客户端分配，不包含 PG 服务端内存。CPU、缓存、数据分布、正文长度及网络延迟变化都可能改变结果。

迁移首先消除运行时对共享卷 SQLite WAL 的依赖。简单读取、全文搜索及写入的收益与代价应分别看上表，不能把 PG 描述成所有操作都更快。PG 使用复合分页索引和 GIN trigram 搜索索引；一次录制通过一条 SQL 事务原子保存元数据、正文、引用和搜索文档。

复现：按 README 设置独立的 `ARTEX_TRAFFIC_TEST_DSN`，运行：

```bash
go test ./traffic -run '^$' -bench '^BenchmarkTrafficIndex$' -benchmem -benchtime=2s -count=3 -cpu=4 -v
```

原始数据见 [traffic-index-20261013.txt](traffic-index-20261013.txt)。

验证：流量模块全部测试及 `go test -race ./traffic` 通过，包括原始 ID/字节保留、WAL 恢复、迁移原子回滚和重试、重复启动、非空目标保护、并发写入、归档恢复、删除回滚及正文搜索。服务器包在独立干净数据库中通过。全项目串行运行仍有原有 `TestGraphOverviewExpandsAssociatedCompanyScope` 失败，已在修改前提交 `2b48d19` 的独立数据库中复现；包间共享数据库还会留下任务导致服务器临时目录清理竞争，服务器单独运行通过。
