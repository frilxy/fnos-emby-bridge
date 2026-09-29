# ---- 构建阶段 ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 多架构：Go 交叉编译，CGO 关闭保证静态二进制
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o /out/fnos-emby-bridge ./cmd/fnos-emby-bridge

# ---- 运行阶段 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && addgroup -S fnos && adduser -S fnos -G fnos
WORKDIR /opt/fnos-emby-bridge
COPY --from=build /out/fnos-emby-bridge /opt/fnos-emby-bridge/fnos-emby-bridge
USER fnos
EXPOSE 8096
ENTRYPOINT ["/opt/fnos-emby-bridge/fnos-emby-bridge"]
