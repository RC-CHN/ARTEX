# 容器依赖与构建缓存

Dockerfile 将工具安装放在独立的 `runtime-tools` 阶段，应用版本与提交号只在 Go 编译和最终镜像标签处声明。仅修改 `ARTEX_BUILD_VERSION` / `VCS_REF` 时，前端依赖、Go 模块、apt 工具、npm 工具和 Chromium 安装层均可复用。

四个官方基础镜像都固定到多架构 manifest digest；Node 运行时使用 Debian Bookworm 版本，与 Python 运行层一致。Node 为 22.23.3，随镜像提供 npm，不再在构建时执行 NodeSource 在线安装脚本。

`package.json` 和 `package-lock.json` 固定 MCP 0.0.83、CLI 0.1.22、Playwright 1.64.0，以及全部传递依赖和 npm 包 integrity。锁文件只引用公开 npm registry。版本取自升级前已运行的工具，更新时应同时修改 manifest 和 lockfile，并重新验证浏览器。

已有数据库配置仍可运行 `npx @playwright/mcp`。镜像的 `npx` 入口将三个预装工具的无版本名称直接转发给锁定的可执行文件，保留参数和当前目录，无需 npm 缓存或网络。其他包名、显式版本或 npx 选项仍由官方 npx 处理。直接运行 `playwright-mcp`、`playwright-cli`、`playwright` 同样可用。

MCP / CLI 内部的 Playwright 版本可能不同于根包。当前它们需要 Chromium revision 1247，而根包需要 1248；`install-browsers.cjs` 按实际 Playwright 版本去重安装。`verify.cjs` 分别通过三个工具所解析的 Playwright API 启动 Chromium、打开页面并验证标题，避免只验证命令存在。

运行层默认设置 `PLAYWRIGHT_MCP_BROWSER=chromium`，让已有 `--headless` 配置直接使用预装浏览器；用户显式传入的 `--browser` 仍优先。`verify-mcp.cjs` 还会通过原有 npx 命令启动真实 MCP stdio 服务，调用浏览器导航和页面求值，验证这条默认路径。

```bash
# 构建中已运行浏览器及离线 npx 验证；以下可独立复测，容器断网。
docker run --rm --network none --entrypoint node artex:local /opt/artex-tools/verify.cjs
docker run --rm --network none --entrypoint node artex:local /opt/artex-tools/verify-mcp.cjs
docker run --rm --network none --entrypoint sh artex:local -c \
  'npm_config_offline=true npx @playwright/mcp --version && playwright-cli --help'

# 连续构建两个发布版本，检查依赖层全部 CACHED，输出耗时与完整日志。
python3 deploy/check-container-cache.py
```

检查结果默认保存在被忽略的 `dist/container-cache-check/`，不会进入源码构建上下文。缓存检查会创建两个本地镜像 `artex-cache-check:before` 和 `artex-cache-check:after`；它不会推送或发布镜像。

2026-10-13 在 Linux amd64、Docker 29.1.3 上实际验证：基础镜像预先拉取，首次构建为 **168.51 秒**；只更改发布版本与提交标签后为 **16.08 秒**。检查的 6 个依赖步骤全部命中缓存，Go 源码复制层也命中缓存。首次构建可使用已有基础依赖缓存，因此这不是从空缓存开始的下载耗时比较。新镜像另以 `--network none` 运行三个工具的浏览器验证、MCP stdio 导航/求值及 npx 入口验证，全部通过。

固定镜像摘要和 npm lockfile 可控制主要工具版本，但 apt 软件源仍接收安全更新，未固定到历史 snapshot；清空缓存重建时，系统软件包和镜像字节可能变化。需要升级基础镜像时，应主动更新 digest、检查新版本并重跑浏览器和缓存验证。

机制说明见 [Docker ARG 缓存规则](https://docs.docker.com/reference/dockerfile/#impact-on-build-caching)、[npm ci](https://docs.npmjs.com/cli/v11/commands/npm-ci/) 和 [Playwright 浏览器版本要求](https://playwright.dev/docs/browsers)。
