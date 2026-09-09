# syntax=docker/dockerfile:1.7
# 多阶段构建：golang 编译 → alpine 精简运行镜像
FROM golang:1.27-alpine AS build
# 通过 goproxy.cn 拉取依赖, 避免在无外网代理环境(如国内网络/受限内网)构建超时
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/dontcrack-manager

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app
COPY --from=build /out/app /usr/local/bin/dontcrack-manager
# 最小权限: 与 DontCrack4ManyLinux 镜像一致, 运行期切换非 root 用户
# (此前缺 USER app, 容器以 root 运行聚合管理服务——可发信号/管理密码面过大)。
USER app
# 根管理器聚合状态 HTTP 服务(默认 127.0.0.1:11884; 镜像内自行挂载配置)
EXPOSE 11884
ENTRYPOINT ["dontcrack-manager"]