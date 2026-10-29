# Multi-stage build: 跨平台自适应编译（支持 Apple Silicon Mac arm64 与 Intel/AMD x86_64）
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /app/feishu-topics .

# 运行镜像
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata mailcap

ENV TZ=Asia/Shanghai \
    FEISHU_IN_DOCKER=1 \
    FEISHU_DOWNLOAD_DIR=/downloads

WORKDIR /app

COPY --from=builder /app/feishu-topics /app/feishu-topics

EXPOSE 8088

VOLUME ["/app/data", "/downloads"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8088/api/status || exit 1

ENTRYPOINT ["/app/feishu-topics"]
CMD ["--config", "/app/config.json", "--data", "/app/data"]
