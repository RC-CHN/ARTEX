# 工具原语性能基准（2026-10-19）

Linux amd64，Intel Xeon CPU Max 9470C，本机合成数据，Go testing benchmark，每项三次，取中位数。测试的是单个原语，不是任务端到端耗时；机器上还运行了测试与编译，绝对时间会随负载变化。没有读取线上任务数据，也没有把本机结果当成 NFS 性能结论。

优化前基准为 `95424c3` 上的工具实现；新加入的 benchmark 用相同数据、参数运行。该基线已经包含有界 Read 与原子编辑，早期无界 Read 的实验不混入这张表。

| 合成操作 | 优化前 | 优化后 | 每次分配内存（前 → 后） |
| --- | ---: | ---: | ---: |
| Edit 读取 8 MiB 文件 | 13.31 ms | 1.56 ms | 48.23 MiB → 8.00 MiB |
| 24 KiB 已捕获输出再次校验 | 73.19 µs | 21.73 µs | 74,409 B → 128 B |
| PTY 满缓冲后追加 4 KiB | 4.31 µs | 0.278 µs | 18,660 B → 0 B |
| Go Grep 普通计数，10,000 行 | 0.943 ms | 0.801 ms | 430,795 B → 33,455 B |
| Go Grep 多行计数，10,000 行、5,000 匹配 | 46.65 ms | 2.67 ms | 约 1.06 MiB → 约 1.06 MiB |

32 MiB 同步命令输出在无落盘目录时使用流式头尾捕获，累计分配约 113 KiB。该项只测捕获器，不包含进程启动或磁盘写入；配置输出目录后的完整落盘另有内容一致性测试。

续租元数据编码在空历史下约 4.9 µs / 1,335 B，带 37 MiB 历史工具输出约 3.9 µs / 1,320 B，表明编码成本不再随历史输出体积增长。这里不包含 PostgreSQL 事务耗时，也不与完整检查点的全流程耗时直接计算倍数。

原始数据：[优化前](tool-primitives-before-20261019.txt)、[优化后](tool-primitives-after-20261019.txt)、[续租元数据](runner-metadata-20261019.txt)。

复现：

```sh
cd norma
go test ./tool -run '^$' -bench 'BenchmarkToolPrimitives|BenchmarkStreamingCommandCapture' -benchmem -count=3
cd ..
go test ./runner -run '^$' -bench BenchmarkLeaseMetadata -benchmem -count=3
```

`TestSearchFallbackCompatibility` 使用优化前实现作为兼容性参照；`TestLinePagerCompatibility` 验证分块边界、长行、空行、分页参数与准确剩余数；`TestPTYWindowCompatibility` 验证滚动及游标；`TestStreamingCaptureCompatibility` 比较原有截断文本，落盘测试校验完整输出。事务测试在独立测试数据库检查旧历史片段未被重写以及 revision/诊断状态的原子性。
