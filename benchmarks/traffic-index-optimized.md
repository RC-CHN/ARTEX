# PostgreSQL 搜索优化 benchmark（2026-10-13）

本次保留相同 Go API 和基准代码，对比迁移版 `f937c63` 与搜索优化后的结果。[首次迁移报告](traffic-index.md)及其原始数据保留作为历史基线。

环境：Linux amd64，Intel Xeon CPU Max 9470C，Go 1.26.3，`-cpu=4`；SQLite `modernc.org/sqlite v1.52.0` 使用本地临时文件及 WAL；PostgreSQL 16.14 使用独立 Docker 卷，经 loopback TCP 连接，默认持久化设置启用。优化后测试在全项目回归结束后单独执行，未同时构建镜像。

语料为 10,000 条记录、100 个主机、4 KiB 正文、1% 命中搜索词。每项 2 秒 × 3 轮，取各轮平均延迟的中位数；P95 也取三轮 P95 的中位数。建库、语料生成、统计信息更新及清理不计时。前后是同机不同时间运行，因此读写等未改变路径的波动也列出，不能把所有差异都归因于代码。

| 操作 | 原 PG 平均/次 | 优化后 PG 平均/次 | 耗时变化 | 本轮 SQLite 平均/次 |
|---|---:|---:|---:|---:|
| 读取单条报文 | 0.103 ms | 0.105 ms | +1.7% | 0.037 ms |
| 主机分页（50 条） | 0.160 ms | 0.162 ms | +1.0% | 0.303 ms |
| 主机 + 正文搜索（10 条） | 5.714 ms | 1.832 ms | -67.9% | 2.581 ms |
| 正文 + 状态分页（含计数） | 15.623 ms | 6.929 ms | -55.6% | 5.206 ms |
| 4 并发读取（摊销耗时） | 0.030 ms | 0.030 ms | -1.0% | 0.021 ms |
| 录制 4 KiB 报文 | 1.745 ms | 1.917 ms | +9.9% | 3.882 ms |

| 操作 | 原 PG P95 | 优化后 PG P95 | 本轮 SQLite P95 |
|---|---:|---:|---:|
| 读取单条报文 | 0.117 ms | 0.122 ms | 0.045 ms |
| 主机分页（50 条） | 0.200 ms | 0.196 ms | 0.386 ms |
| 主机 + 正文搜索（10 条） | 6.350 ms | 2.200 ms | 3.173 ms |
| 正文 + 状态分页（含计数） | 17.174 ms | 8.373 ms | 6.366 ms |
| 录制 4 KiB 报文 | 1.570 ms | 1.745 ms | 12.112 ms |

初始表及索引：SQLite 183.45 MiB，PG 169.84 MiB。口径沿用基线：SQLite 索引文件及 WAL/SHM，PG schema 表及索引，不包括集群 WAL、服务端内存或原始 blob 文件。

优化针对 GIN trigram 命中候选后的正文匹配：数据库触发器仅将派生搜索文档转为小写，查询使用 `LIKE lower(参数)`，把长正文的大小写转换移到写入阶段。原始报文、ID 和 blob 不改动。元数据查询保留 `ILIKE`，中文及转义后的字面子串搜索不变。已有搜索文档一次性回填，与迁移标记原子提交；触发器同样覆盖归档恢复和回滚后的旧镜像写入。

此次优化没有增加重复的搜索表或索引。写入包含规范化触发器成本；上表同时给出写入和单条读取，避免仅展示有利查询。上述结果只代表此语料、缓存、CPU、网络和存储条件，不等于集群生产吞吐。并发读取是吞吐倒数；Go 的 B/op 不包括 PostgreSQL 服务端内存。

参考 PostgreSQL 官方文档：[pg_trgm 索引与 LIKE/ILIKE](https://www.postgresql.org/docs/16/pgtrgm.html)、[模式匹配和区域设置](https://www.postgresql.org/docs/16/functions-matching.html)。

复现：设置指向临时 PostgreSQL 的 `ARTEX_TRAFFIC_TEST_DSN`，运行：

```bash
go test ./traffic -run '^$' -bench '^BenchmarkTrafficIndex$' -benchmem -benchtime=2s -count=3 -cpu=4 -v
```

原始数据：[优化前](traffic-index-20261013.txt)、[优化后](traffic-index-optimized-20261013.txt)。

配套回归：全项目 `go test ./... -count=2` 通过（1,852 个通过结果，真实 LLM 用例按预期跳过两次）；关闭流程通过 server / traffic / enrich 的 race 检查。随后补充熔断写入顺序测试，并重新通过 server 全包两轮及 race。关联公司范围展开的原有失败用例已修复，集成测试按包使用独立数据库。
