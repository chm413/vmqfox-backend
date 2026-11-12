# 当前只发布 linux/amd64 镜像（见 docker-publish.yml 的 platforms）。
#
# 两个构建阶段仍然用 --platform=$BUILDPLATFORM 固定跑在构建机本机架构，而不是
# 跟随目标平台。虽然单架构下两者通常是同一个平台，但保留这个固定有两个实际收益：
#   1. 将来恢复 arm64 时，前端不会被丢进 QEMU 重跑一遍——它的产物是平台无关的
#      静态 index.html + css + js，跨架构重建得到的文件逐字节相同；
#   2. 在 Apple Silicon 本机构建 amd64 镜像时走原生交叉编译而非 QEMU 模拟。
#
# 历史教训：此前两个阶段都跟随目标平台，构建 linux/amd64 + linux/arm64 时，
# arm64 那一路全程跑在 QEMU 下，pnpm install 的每条 registry 请求从 amd64 的
# 毫秒级退化到 10~22 秒，并最终触发 esbuild 原生 postinstall 的 SIGILL 崩溃，
# 单这一层就跑不完 28 分钟。Go 侧因为已设 CGO_ENABLED=0，用 GOOS/GOARCH 原生
# 交叉编译即可，从来不需要模拟。
#
# 运行时阶段（alpine）不指定 --platform，始终跟随目标平台——这是必须的。

# 阶段一：从 web 源码构建 React 静态资源。
# 构建产物直接落在 internal/http/static/out，供下一阶段的 go:embed 读取，
# 避免镜像依赖本地未跟踪的旧构建产物。
FROM --platform=$BUILDPLATFORM node:24-alpine AS web-builder

WORKDIR /web

# 使用仓库锁定的 pnpm 版本，保证 CI 与镜像安装同一套依赖树。
# corepack 缓存挂载：避免每次构建都重新下载 pnpm 本体。
RUN --mount=type=cache,target=/root/.cache/node/corepack \
    corepack enable && corepack prepare pnpm@11.22.0 --activate

COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./

# pnpm store 缓存挂载：命中的包不会再走网络。首次构建仍需完整下载，
# 之后增量构建只拉新增依赖。
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile

COPY web/ ./
RUN pnpm build

# 构建后校验 index.html 引用的每个本地资源都真实存在，
# 防止把缺少 JS/CSS 的空壳前端打进镜像。
RUN set -eu; \
    out=/internal/http/static/out; \
    test -f "$out/index.html" || { echo "构建产物缺少 index.html"; exit 1; }; \
    grep -oE '(src|href)="/assets/[^"]+"' "$out/index.html" \
      | sed -E 's#^[a-z]+="/assets/(.*)"$#\1#' \
      | sort -u > /tmp/referenced-assets.txt; \
    test -s /tmp/referenced-assets.txt || { echo "index.html 未引用任何 assets 资源"; exit 1; }; \
    while read -r asset; do \
        test -f "$out/assets/$asset" || { echo "index.html 引用的资源缺失: $asset"; exit 1; }; \
        echo "已校验前端资源: $asset"; \
    done < /tmp/referenced-assets.txt

# 阶段二：Go 1.26.11 builder；仅复制 cmd/internal，历史 PHP 源码不会进入运行镜像。
# GOARCH 由 TARGETARCH 决定，因此单个阶段即可产出任意目标架构的产物；
# 当前只构建 linux/amd64，恢复多架构时无需改动本阶段。
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
# 允许国内环境覆盖：docker build --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct

ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOPROXY=${GOPROXY}

WORKDIR /src
RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal

# 用本次构建的前端资源覆盖仓库中的占位目录，确保 embed 的一定是当前源码产物
RUN rm -rf internal/http/static/out
COPY --from=web-builder /internal/http/static/out ./internal/http/static/out

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/vmqfox-api ./cmd/vmqfox-api

# 阶段三：精简运行时仍保留 CA、时区和 wget，分别用于 HTTPS 通知、时间处理和健康检查。
# 注意：本阶段不指定 --platform，跟随目标平台——目标为 arm64 时运行镜像就是 arm64。
FROM alpine:3.22 AS runtime

ARG TZ=Asia/Shanghai
ENV TZ=${TZ}

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S vmqfox \
    && adduser -S -G vmqfox vmqfox

COPY --from=builder /out/vmqfox-api /usr/local/bin/vmqfox-api
COPY entrypoint.sh /usr/local/bin/vmqfox-entrypoint
RUN chmod 0555 /usr/local/bin/vmqfox-entrypoint /usr/local/bin/vmqfox-api

USER vmqfox:vmqfox
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/vmqfox-entrypoint"]
CMD ["/usr/local/bin/vmqfox-api"]
