# ARTEX container image.
#
# 多阶段构建：前端静态导出 → Go 单二进制（内嵌前端）→ 运行镜像。
# 原 Docker Hub 镜像源（autumn27/artex）已失效，本仓库自此支持从纯源码自助构建：
#   docker compose build        # 或 docker build -t artex:local .
# TARGETARCH 由 buildx 自动注入（amd64/arm64），Go 交叉编译无需 QEMU；
# 运行层只装工具，编译全部在构建阶段完成。
# 国内网络可覆盖模块源（默认已对国内友好）：
#   --build-arg GOPROXY=https://goproxy.cn,direct
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com

########## 阶段 1：前端静态导出 ##########
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2 AS web
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web
# --ignore-scripts：跳过 prepare(husky)——容器里没有 .git，husky 会失败；
# Next.js/Biome 的平台二进制走 optionalDependencies，不依赖生命周期脚本。
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --ignore-scripts
COPY web/ ./
RUN npm run build:static

########## 阶段 2：Go 后端（内嵌前端，静态编译）##########
FROM golang:1.26-alpine@sha256:f1ddd9fe14fffc091dd98cb4bfa999f32c5fc77d2f2305ea9f0e2595c5437c14 AS gobuild
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
# go.mod 将 norma 替换为本地模块；下载依赖前必须先提供它的模块声明。
COPY norma/go.mod norma/go.sum ./norma/
RUN go mod download
COPY . .
# 前端静态产物从 web 阶段拷入（.dockerignore 已排除宿主 dist，不会冲突）
COPY --from=web /web/out/ server/webui/dist/
# 版本参数只参与编译，不能使前面的模块下载层失效。
ARG TARGETARCH
ARG ARTEX_BUILD_VERSION=dev
RUN CGO_ENABLED=0 GOARCH="${TARGETARCH}" go build -tags embedui -trimpath \
    -ldflags "-s -w -buildid= -X main.version=${ARTEX_BUILD_VERSION}" \
    -o /out/artex ./cmd/artex

########## 阶段 3：固定 Node 运行时（与 Python 同为 Debian Bookworm）##########
FROM node:22.23.3-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392 AS node-runtime

########## 阶段 4：运行工具（独立于应用源码和发布版本缓存）##########
FROM python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258 AS runtime-tools
ARG NPM_REGISTRY=https://registry.npmmirror.com
# 工具包固定到 lockfile；镜像源只影响下载地址，不改变依赖版本与 integrity。
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_DOWNLOAD_HOST=https://cdn.npmmirror.com/binaries/playwright \
    PLAYWRIGHT_BROWSERS_PATH=/opt/playwright-browsers \
    PLAYWRIGHT_MCP_BROWSER=chromium \
    PATH="/opt/artex-tools/node_modules/.bin:${PATH}"
# 常用工具：ripgrep / curl / vim，加一批 recon 常备件（按需增删）。
# Node/npm 直接来自固定摘要的官方 Debian 镜像，不再执行在线安装脚本。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libatomic1 libstdc++6 ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
COPY --from=node-runtime /usr/local/bin/node /usr/local/bin/node
COPY --from=node-runtime /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm
# 保留已有数据库里的 npx @playwright/mcp 配置，避免 npx 先查 registry。
COPY --chmod=755 deploy/container-tools/npx.sh /usr/local/bin/npx
WORKDIR /opt/artex-tools
COPY deploy/container-tools/package.json deploy/container-tools/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
    && npm cache clean --force
# MCP/CLI 可依赖不同的 Playwright revision，逐个安装，按版本去重。
COPY deploy/container-tools/install-browsers.cjs deploy/container-tools/verify.cjs deploy/container-tools/verify-mcp.cjs ./
RUN node install-browsers.cjs \
    && node verify.cjs \
    && node verify-mcp.cjs \
    && cd /tmp \
    && npm_config_offline=true npx @playwright/mcp --version \
    && npm_config_offline=true npx @playwright/cli --help \
    && npm_config_offline=true npx playwright --version \
    && npm_config_offline=true npx --version \
    && rm -rf /var/lib/apt/lists/*

########## 阶段 5：应用（仅这一阶段和 Go 编译随版本变化）##########
FROM runtime-tools AS runtime
WORKDIR /app
# 构建阶段产出的单二进制（内嵌前端）
COPY --from=gobuild /out/artex /app/artex
# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 artex —— docker stop 只把信号发给 PID 1，
# 不转发的话 artex 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
ARG ARTEX_BUILD_VERSION=dev
ARG VCS_REF
LABEL org.opencontainers.image.title="ARTEX" \
      org.opencontainers.image.version="$ARTEX_BUILD_VERSION" \
      org.opencontainers.image.revision="$VCS_REF"
# data/（流量 blob、会话记录及旧索引备份）持久化点
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
