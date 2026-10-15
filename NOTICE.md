# 来源与维护

ARTEX 由 RC-CHN/ARTEX 独立维护。项目历史源自 Autumn-27/ARTEX，后经 mhtsec/ARTEX 保存源码；原作者记录、Git 历史和根目录 AGPL-3.0 许可证继续保留。

`norma/` 最初从 `github.com/Autumn-27/norma` 的 v0.4.3 模块源码恢复，现在以 `github.com/RC-CHN/ARTEX/norma` 在本仓库维护。构建和测试始终使用本地源码，不下载或调用原仓库。

前端模板的许可证保留在 `web/LICENSE`。其他第三方依赖继续遵循各自许可证。本说明记录来源，不改变现有许可证或作者署名。

容器镜像预装 ProjectDiscovery 的 vulnx、Nuclei 和 nuclei-templates，均按各自 MIT 许可证分发。CLI 许可证随镜像保存在 `/usr/local/share/licenses/vulnx/`、`/usr/local/share/licenses/nuclei/`，模板许可证在 `/opt/nuclei-templates/LICENSE.md`；固定版本与校验和见 `deploy/container-tools/security-tools.lock.json`。

新增工具的来源、版本与制品哈希见 `toolkit/catalog.lock.json`；许可证随缓存二进制及独立工具包保存，补充副本在 `toolkit/licenses/`。sqlmap、testssl 的完整固定源码（含各自许可证）分别保留在镜像 `/opt/sqlmap`、`/opt/testssl`。交叉编译工具包保留 `source.lock.json` 与 `licenses/`，构建脚本不改变上游许可证。Python 包的许可证随其安装元数据保留。

离线 IP 地理数据来自 [ip2region](https://github.com/lionsoul2014/ip2region)，保留其 Apache-2.0 / MIT 双许可证。ASN 数据为 [DB-IP ASN Lite](https://db-ip.com/db/download/ip-to-asn-lite)，按 CC BY 4.0 分发：IP Geolocation by [DB-IP.com](https://db-ip.com)。原始 XDB/MMDB 数据未修改，仅解压并打包；来源、日期和 SHA-256 固定在 `toolkit/ip-data.lock.json`，许可证随镜像内的数据快照保留。查询结果携带来源链接，展示这些结果的页面应保留对应署名。
