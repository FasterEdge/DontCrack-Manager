<div align="center">
<img src="./Logo.png" alt="logo" width="100"/>
<h2>DontCrack-Manager</h2>
<h3>DontCrack 多进程根管理器</h3>
</div>

### 一、功能简介

- **单体的 DontCrack 一次只能管理一个进程。** DontCrack-Manager 同时拉起并监管
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

- **多实例监管**: 一份 YAML 配置承载 N 个服务, 每个服务由一个 DontCrack 实例监管;
- **真实契约**: 完整复用 DontCrack CLI flags 与 HTTP API(`/heartbeat` `/healthz`), 零假设;
- **依赖编排**: `depends_on` 声明启动顺序, 依赖服务就绪(进程存活 + 可选探针健康)后才启动;
- **退避重启**: DontCrack 进程自身崩溃后按 `always | on-failure | never` 策略指数退避恢复
  (上限 `backoff_max`; 稳定运行 30s 后退避归零);
- **优雅停机**: SIGINT/SIGTERM(或 `/shutdown`)依次转发给各 DontCrack(其内部会先停子进程), 宽限后强杀**整棵进程树**兜底;
- **进程树收容器**: unix 用独立进程组(Setpgid), Windows 用 Job Object(`KILL_ON_JOB_CLOSE`)—— DontCrack 崩溃后重启前按树清理, **不留孤儿子进程**;
- **聚合状态**: 可选根管理 HTTP 服务 — `/healthz`(liveness: 根管理器自身存活) `/readyz`(readiness: 全部 `depends_on` 依赖就绪且探针通过) `/status`(JSON 详情) `/shutdown`(远程停全部);
- **安全 fail-closed**: 依赖环/重复端口/非法策略一律拒绝启动; 对外监听必须配置密码
  (常数时间比较, 与 DontCrack 家族一致); 所有健康查询与停机宽限均有超时。

### 二、快速开始

```bash
# 构建
go build -o dontcrack-manager ./cmd/dontcrack-manager

# 校验配置(不启动)
./dontcrack-manager -config examples/manager.yaml -check

# 作为根管理器运行
./dontcrack-manager -config /etc/dontcrack-manager.yaml

# 查询聚合状态
curl http://127.0.0.1:11884/healthz   # liveness: 根管理器自身存活 → 200
curl http://127.0.0.1:11884/readyz    # readiness: 依赖全部就绪且探针通过 → 200, 否则 503
curl http://127.0.0.1:11884/status
curl -X POST http://127.0.0.1:11884/shutdown   # 远程优雅停机
```

### 三、配置参考

```yaml
log_level: info                 # debug|info|warn|error
listen: 127.0.0.1:11884         # 聚合状态 HTTP(留空禁用); 对外监听必须配 password
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
    dontcrack_env: ["HTTP_PROXY=http://proxy:7890"]  # 追加给 DontCrack 进程的环境变量(同名覆盖继承值)
    restart: always             # Manager 层策略: always|on-failure|never
    backoff: 3s                 # 重启退避基数(指数增长)
    backoff_max: 60s            # 退避上限
    depends_on: [db]            # 启动依赖
    depend_timeout: 60s         # 等待依赖就绪上限
    enabled: true
```

### 四、HTTP API

| 路径 | 方法 | 说明 |
| ---- | ---- | ---- |
| `/healthz` | GET | **liveness**: 根管理器自身存活即 200; 与业务服务健康**无关**(业务抖动不应触发重启) |
| `/readyz` | GET | **readiness**: 全部启用服务的 `depends_on` 依赖就绪且探针通过 → 200; 否则 503 |
| `/status` | GET | 每服务 JSON 详情(DontCrack PID/子进程 PID/心跳快照/重启次数/最后退出码) |
| `/shutdown` | POST | 触发全量优雅停机 |

两个探针语义**刻意不同**, 对应 K8s 的 `livenessProbe` 与 `readinessProbe`:

- `/healthz` 只反映监管器进程本身还能否响应查询。业务服务崩了、依赖没起来, 它依然是 200 ——
  否则 liveness 失败会让编排系统重启声明周期内的整个实例, 把可自愈的业务抖动放大成架构性故障;
- `/readyz` 与内部 `waitDeps` 共用**同一个**就绪判定函数, 因此"readyz 通过"与"依赖方可以启动"
  是同一个事实, 不会出现 readyz 说就绪而依赖方仍被拦下的割裂。无任何启用服务时 fail-closed 返回 503。

鉴权优先级与 DontCrack 一致: `Authorization: Bearer <pw>` > `X-DontCrack-Password` > `?password=`。

### 五、停机语义与进程树收容

停机:

1. 收到 SIGINT/SIGTERM(或 `/shutdown`);
2. 取消心跳刷新与依赖等待, 向每个 DontCrack 实例发送优雅停止请求(SIGTERM / Windows 等价物);
3. 等待全部退出, 上限 `shutdown_grace`;
4. 宽限后仍存活的进程**强制终止整棵进程树**兜底(fail-closed, 不留孤儿)。

进程树收容器按平台实现, 能力边界如实列明:

| 平台 | 收容器 | 优雅停止 | 强杀兜底 | 能力边界 |
| ---- | ------ | -------- | -------- | -------- |
| Linux/macOS | 独立进程组(`Setpgid`) | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)` | 子进程若自行 `setsid` 脱离进程组, 则不在强杀范围 |
| Windows | 每实例一个 Job Object(`KILL_ON_JOB_CLOSE`) | `GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT)`(等价 SIGTERM) | `TerminateJobObject`(整棵树) | 若 DontCrack 无控制台(服务化启动), 优雅通道可能无效 → 只能靠宽限期后强杀; 子进程若带 `BREAKAWAY_OK` 逃逸出 Job 则不在范围内 |

> Windows 上此前退化为 `taskkill /T /F`(纯硬杀, 无优雅语义)。现改为 Job Object 收容 +
> CTRL_BREAK 优雅请求: 强杀语义由内核保证(不再依赖外部命令), 且监管器自身异常退出时
> 句柄关闭会触发 `KILL_ON_JOB_CLOSE` 清掉树内残留 —— 比 `taskkill` 更强的不留孤儿保证。
> 收容器接管失败(例如本进程已被上层 Job 收容且禁止嵌套)时不会阻断启动, 会写告警并把
> 强杀能力降级为"仅终止 DontCrack 本体"。

### 六、与 DontCrack 的分工

| 层 | 职责 |
| -- | ---- |
| DontCrack 实例 | 单子进程的启动/探针/自动重启/日志 |
| DontCrack-Manager | 多实例依赖编排、DontCrack 进程自身的存活监管与恢复、聚合状态 |

### 七、完整体端到端联调

`e2e/` 目录提供可复现的端到端测试:构建**真实 DontCrack 二进制**,用根管理器同时监管
3 个服务(db 常驻 / web 依赖 db 带 HTTP 探针 / worker 周期性崩溃),覆盖:

1. 多实例启动、依赖顺序(db 先于 web);
2. 探针健康判定(`healthz=up` + `/heartbeat` 快照 `state=running`);
3. DontCrack 层 auto-restart(worker 崩溃自动重启,state-file 计数递增);
4. Manager 层重启(kill 掉 DontCrack 进程 → `restart=on-failure` 恢复);
5. 无孤儿保证(kill -9 DontCrack 后,其派生子进程按进程树清理);
6. 聚合 API 探针语义分离与优雅停机 `/shutdown`(进程全清、退出码 0)。

脚本路径可参数化, 因此既能跑 Docker 布局也能跑 CI checkout 布局:

```bash
# Docker 布局(与历史用法等价)
docker run --rm \
  -v $(pwd):/src/dcm \
  -v /path/to/DontCrack4ManyLinux:/src/dc4m \
  -v /path/to/this/repo/e2e:/e2e -w /e2e \
  golang:1.27 bash /e2e/e2e_test.sh

# CI checkout 布局(无需 docker)
DC4M_DIR=/path/to/DontCrack4ManyLinux DCM_DIR=/path/to/DontCrack-Manager \
  bash e2e/e2e_test.sh

# 探针语义联调(依赖起不来 → /readyz 503 而 /healthz 200)
DC4M_DIR=/path/to/DontCrack4ManyLinux DCM_DIR=/path/to/DontCrack-Manager \
  bash e2e/readyz_test.sh
```

`.github/workflows/e2e.yml` 在 Linux runner 上自动执行上述两个脚本, 因此 README 宣称的
"可复现端到端"有 CI 证据; 联调日志与 `/status` 快照作为工件上传。

> 该联调曾抓出并修复:① DontCrack flag 解析错位(`-args` 值以 `-` 开头被 flag 包吞并,
> 端口等后续配置落默认值 → 全量改用 `-flag=value` 形式 + DontCrack 侧 fail-closed 防御);
> ② DontCrack auto-restart 永不生效(`CurrentProcess` 未释放导致重启计划恒判过期);
> ③ 根管理器"不留孤儿"承诺未兑现(DontCrack 被强杀后其子进程成孤儿 → 进程组/Job Object 收容);
> ④ `/healthz` 与 `/readyz` 实现同构、探针差异化名不副实 → 拆为 liveness/readiness 两套语义。

### 八、测试

```bash
go vet ./...
go test -race ./...
```

### 九、安全

见 [SECURITY.md](SECURITY.md)。漏洞报告请私信 FasterEdge 组织(tyza66@outlook.com)。

### 十、License

[Apache 2.0](LICENSE)
