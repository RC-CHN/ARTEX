# 离线选包

完整镜像内置 Tack 0.1.2 的 33 个官方归档，版本和 SHA-256 固定。`/opt/artex-kit/tack/manifest.json` 保留各包的构建、运行时与验证信息；归档含 LICENSE 与 BUILD.json。

| 目录中的工具名 | 发布平台 |
| --- | --- |
| tack | Linux/macOS amd64、arm64；Windows 386、amd64、arm64 |
| tack-control | Linux amd64、arm64；Windows amd64 |
| tack-node | Linux 386、amd64、arm64、armv5/v6/v7、mips/mipsle/mips64/mips64le；Windows 386、amd64、arm64 |
| tack-node-legacy | 同上 10 个 Linux 平台，解包的程序名仍为 tack-node |

工具目录的平台键使用 linux/mipsle 等形式，原包名中的 softfloat 已映射到对应架构。所有包都可离线 fetch/stage；默认 PATH 只安装当前 Linux 架构的现代 CLI、控制端和节点程序。

从已有访问方式确认目标 OS、CPU、位数/端序、内核及 ARM 浮点能力，不用 Runner 架构推断目标。选包脚本在操作端运行，路径相对于本 Skill 根目录：

```sh
python3 scripts/select-node-artifact.py /opt/artex-kit/tack/manifest.json --os linux --arch x86_64 --kernel 2.6.32
python3 scripts/select-node-artifact.py /opt/artex-kit/tack/manifest.json --os windows --arch amd64 --windows-version 10.0
artex-kit fetch tack-node-legacy --platform linux/amd64 --offline
artex-kit stage tack-node --platform linux/mipsle --directory ./node-mipsle
```

脚本返回原包名、哈希、运行时、验证级别以及可用于 artex-kit 的 tool/platform。`stage` 创建新目录和校验回执；通过现有传输渠道交接后核对哈希，再实际执行 `-version`。`artex-kit clean DIRECTORY/receipt.json` 只清理由它拥有且未修改的本地目录。

Modern Linux 通常要求内核 ≥3.2；Legacy amd64/386 面向 ≥2.6.32，ARMv5/v6/v7 的 3.1 系列可选 Legacy。MIPS64le 选包采用 ≥4.8 的保守边界。Windows 要求 Windows 10/Server 2016 起；没有 Windows 7 或 PowerPC 包。ARMv5 为软浮点，v6/v7 需核对 VFP；MIPS 包为软浮点，仍需设备验收。

交叉编译、模拟器检查、原生运行和目标设备验收是不同级别。Legacy 的老内核验证不能推广到所有 NAS/路由器。Static Go 不依赖目标 glibc，仍受内核、CPU、挂载执行权限和文件系统能力限制。选择错误应报告能力缺口。

附带选包脚本来自固定的 [Tack 0.1.2](https://github.com/RC-CHN/tack/tree/tack-v0.1.2)，保留其平台选择规则，ARTEX 只增加目录工具名与平台键输出。Tack 使用 MIT 许可，见本 Skill 的 LICENSE.tack。
