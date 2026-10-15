---
name: ip-lookup
description: 离线查询 IPv4/IPv6 的国家、省市、ISP、ASN 和网络组织，批量补充资产背景并保留数据来源及版本。适用于已有 IP 地址的归属分析，不执行域名解析或目标探测。
license: AGPL-3.0-only
compatibility: ARTEX Runner 的 artex-kit 与预装 ip2region、DB-IP ASN Lite 数据；自定义 Runner 先检查 ipdb status。
---

# 离线 IP 归属查询

先用 `command -v artex-kit`、`artex-kit ipdb status` 检查当前 Runner。默认镜像已内置数据，无需注册账号、下载密钥或连接外部查询服务。

```bash
artex-kit iplookup 8.8.8.8 2001:4860:4860::8888
artex-kit iplookup --file ips.txt > ip-attribution.json
printf '%s\n' '8.8.8.8' '::1' | artex-kit iplookup --file -
```

每行一个 IP 字面量，单批最多 10,000 个。域名、URL、CIDR、带端口或 zone ID 的地址会标为无效，不自动 DNS 解析。IPv4 映射的 IPv6 按 IPv4 归一化。私网、回环、链路本地、组播、共享地址空间和保留地址单独分类，不填充公网归属。

输出中 `geo` 来自 ip2region，`asn` 来自 DB-IP ASN Lite。保留 `database.sources` 中各自的日期、哈希、署名链接及 `stale` 标记；超过 90 天标为旧数据，不代表结果必然错误。来源日期不同，不把镜像构建日期当成全部数据的更新时间。

`found` 表示至少一个来源命中；检查每个字段的 `status`。`not_found` 表示有效库没有匹配，`partial` 表示至少一个库缺失或读取失败，不能把它当成“没有归属”。`invalid` 和 `special` 分别表示无效输入和特殊地址。没有数据时报告缺失，不向第三方 API 发送目标 IP，也不在普通查询中下载更新。

ASN 组织表示网络归属，不直接证明网站经营者或业务资产所有权；地理位置为估计，同一 Anycast IP 可能对应多个地点。把结果作为资产背景，复用任务已有资产工具写入时保留来源和时间，不据此自动扩展测试范围。展示结果时保留 [ip2region](https://github.com/lionsoul2014/ip2region) 与 [DB-IP](https://db-ip.com) 署名。

可通过 `--data-dir /path/to/ip-data` 或 `ARTEX_IP_DATA` 使用只读共享数据目录。管理员更新见仓库《工具仓库与验证工作流》的离线 IP 数据章节；普通 Agent 查询只读当前快照，不安装数据、不启动更新任务。
