# syntax=docker/dockerfile:1

# 构建阶段：需要 engine + adapter115 + gateway + store 四个模块（replace 指向本地目录），
# 因此构建上下文必须是仓库根目录。
ARG GO_IMAGE=golang:1.24-bookworm
FROM ${GO_IMAGE} AS builder
WORKDIR /src

COPY engine/ ./engine/
COPY adapter115/ ./adapter115/
COPY gateway/ ./gateway/
COPY store/ ./store/

# 中国大陆用户可将 GOPROXY 换成 goproxy.cn 加速依赖下载
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src/gateway
RUN go env -w GOFLAGS=-mod=mod \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nb-gateway ./cmd/nb-gateway

# 运行阶段：distroless 静态镜像（自带 CA 证书，供 HTTPS 回源使用）
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/nb-gateway /nb-gateway
EXPOSE 8091
ENTRYPOINT ["/nb-gateway"]
