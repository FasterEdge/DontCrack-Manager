# DontCrack-Manager — DontCrack 多进程根管理器

> FasterEdge 开源项目 · [Github](https://github.com/FasterEdge) · [Gitee](https://gitee.com/FasterEdge)

**单体的 DontCrack 一次只能管理一个进程。** DontCrack-Manager 同时拉起并监管
**多个 DontCrack 实例**(每个实例监管一个子进程), 作为嵌入式 Linux / 精简容器等
**无进程管理器环境下的系统多进程根管理器**。

```
┌────────────────────────────────────────────────────────────┐
│                    DontCrack-Manager (根管理器)              │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                  │
│  │ DontCrack │  │ DontCrack │  │ DontCrack │  ...            │
│  │  实例 1   │  │  实例 2   │  │  实例 3   │                 │
│  └─────┬────┘  └─────┬────┘  └─────┬────┘                 │
│        │             │             │                       │
│     ┌──▼──┐      ┌──▼──┐      ┌──▼──┐                     │
│     │子进程A│      │子进程B│      │子进程C│                    │
│     └─────┘      └─────┘      └─────┘                     │
└────────────────────────────────────────────────────────────┘
```

## 特性

- **多实例监管**: 一份 YAML 配置承载 N 个服务, 每个服务由一个 DontCrack 实例监管;
- **真实契约**: 完整复用 DontCrack CLI flags 与 HTTP API(`/heartbeat` `/healthz`), 零假设;
- **依赖编排**: `depends_on` 声明启动顺序, 依赖服务就绪(进程存活 + 可选探针健康)后才启动;
- **退避重启**: DontCrack 进程自身崩溃后按 `always | on-failure | never` 策略指数退避恢复
  (上限 `backoff_max`; 稳定运行 30s 后退避归零);
- **优雅停机**: SIGINT/SIGTERM 依次转发给各 DontCrack(其内部会先停子进程), 宽限后 SIGKILL 兜底;
- **聚合状态**: 可选根管理 HTTP 服务 — `/healthz`(全服务健康聚合) `/status`(JSON 详情)
  `/shutdown`(远程停全部);
- **安全 fail-closed**: 依赖环/重复端口/非法策略一律拒绝启动; 对外监听必须配置密码
  (常数时间比较, 与 DontCrack 家族一致); 所有健康查询与停机宽限均有超时。

## 快速开始

```bash
# 构建
go build -o dontcrack-manager ./cmd/dontcrack-manager

# 校验配置(不启动)
./dontcrack-manager -config examples/manager.yaml -check

# 作为根管理器运行
./dontcrack-manager -config /etc/dontcrack-manager.yaml

# 查询聚合状态
curl http://127.0.0.1:11884/healthz
curl http://127.0.0.1:11884/status
curl -X POST http://127.0.0.1:11884/shutdown   # 远程优雅停机
```

## 配置参考

```yaml
log_level: info                 # debug|info|warn|error
listen: 127.0.0.1:11884         # 聚合状态 HTTP; 对外监听必须配 password
password: ""                    # 常数时间鉴权密码
dontcrack_binary: dontcrack     # DontCrack 可执行文件(可被服务级覆盖)
shutdown_grace: 10s             # 停机宽限

services:
  - name: web                   # 唯一; [A-Za-z0-9._-], 1-64
    path: /opt/app/web          # 必填: 子进程路径(透传 -path)
    args: "--addr :8080"        # 透传 -args
    pre: "mkdir -p /run"        # 启动前命令(透传 -pre)
    env: "TZ=Asia/Shanghai"     # 子进程环境变量(透传 -env)
    auto_restart: true          # 子进程自动重启(透传 -auto-restart)
    max_retries: -1             # DontCrack 重试上限(-1 无限)
    start_now: true             # 立即启动子进程
    probe_cmd: "curl -sf http://127.0.0.1:8080/healthz"
    probe_interval: 10
    probe_timeout: 3
    probe_failure_limit: 3
    listen_address: 127.0.0.1   # 该 DontCrack 实例的 HTTP 监听
    port: 11883                 # 每实例必须唯一(冲突拒绝启动)
    password: ""                # 实例管理密码(对外监听必填)
    file_log: false
    log_path: "./logs/dontcrack"
    log_life_day: 7
    dontcrack_binary: ""        # 服务级覆盖
    dontcrack_env: ["HTTP_PROXY=http://proxy:7890"]  # 追加给 DontCrack 进程的环境变量
    restart: always             # Manager 层策略: always|on-failure|never
    backoff: 3s                 # 重启退避基数(指数增长)
    backoff_max: 60s            # 退避上限
    depends_on: [db]            # 启动依赖
    depend_timeout: 60s         # 等待依赖就绪上限
    enabled: true
```

## HTTP API

| 路径 | 方法 | 说明 |
| ---- | ---- | ---- |
| `/healthz` | GET | 全部启用服务健康 → 200; 否则 503 |
| `/status` | GET | 每服务 JSON 详情(DontCrack PID/子进程 PID/心跳快照/重启次数/最后退出码) |
| `/shutdown` | POST | 触发全量优雅停机 |

鉴权优先级与 DontCrack 一致: `Authorization: Bearer <pw>` > `X-DontCrack-Password` > `?password=`。

## 停机语义

1. 收到 SIGINT/SIGTERM(或 `/shutdown`);
2. 取消心跳刷新与依赖等待, 向每个 DontCrack 实例发送 SIGTERM(DontCrack 会先停其子进程);
3. 等待全部退出, 上限 `shutdown_grace`;
4. 宽限后仍存活的进程 SIGKILL 兜底(fail-closed, 不留孤儿)。

## 与 DontCrack 的分工

| 层 | 职责 |
| -- | ---- |
| DontCrack 实例 | 单子进程的启动/探针/自动重启/日志 |
| DontCrack-Manager | 多实例依赖编排、DontCrack 进程自身的存活监管与恢复、聚合状态 |

## 测试

```bash
go vet ./...
go test -race ./...
```

## 安全

见 [SECURITY.md](SECURITY.md)。漏洞报告请私信 FasterEdge 组织(tyza66@outlook.com)。

## License

[Apache 2.0](LICENSE)