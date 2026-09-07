<div align="center">
<img src="./Logo.png" alt="logo" width="100"/>
<h2>DontCrack-Manager</h2>
<h3>Root Process Manager for DontCrack</h3>
</div>

### 1. Introduction

- A single DontCrack instance supervises exactly **one** child process. DontCrack-Manager
  supervises **many** DontCrack instances at once — a system-level multi-process root
  manager for environments without a process manager (embedded Linux, slim containers).

```
┌────────────────────────────────────────────────────────────┐
│                 DontCrack-Manager (root manager)            │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                  │
│  │ DontCrack │  │ DontCrack │  │ DontCrack │  ...            │
│  │ instance 1│  │ instance 2│  │ instance 3│                │
│  └─────┬────┘  └─────┬────┘  └─────┬────┘                 │
│        │             │             │                       │
│     ┌──▼──┐      ┌──▼──┐      ┌──▼──┐                     │
│     │child A│      │child B│      │child C│                    │
│     └─────┘      └─────┘      └─────┘                     │
└────────────────────────────────────────────────────────────┘
```

- **Multi-instance supervision**: one YAML file drives N services, each supervised by one DontCrack instance;
- **Real contract**: reuses DontCrack CLI flags and HTTP API (`/heartbeat`, `/healthz`) as-is, zero assumptions;
- **Dependency ordering**: `depends_on` gates start order; a service starts only after its
  dependencies are ready (process alive + optional probe healthy);
- **Backoff restarts**: if a DontCrack process itself dies, restart per
  `always | on-failure | never` with exponential backoff (`backoff_max` cap; reset after 30s uptime);
- **Graceful shutdown**: SIGINT/SIGTERM forwarded to every DontCrack (which stops its child
  first), SIGKILL fallback after the grace period;
- **Aggregated status**: optional root HTTP service — `/healthz` (aggregate health),
  `/status` (per-service JSON detail), `/shutdown` (remote graceful stop of all);
- **Fail-closed security**: dependency cycles / duplicate ports / bad policies refuse startup;
  external listeners require a password (constant-time compare, same scheme as DontCrack family).

### 2. Quick Start

```bash
# build
go build -o dontcrack-manager ./cmd/dontcrack-manager

# validate config only (no start)
./dontcrack-manager -config examples/manager.yaml -check

# run as the root manager
./dontcrack-manager -config /etc/dontcrack-manager.yaml

# query aggregated status
curl http://127.0.0.1:11884/healthz
curl http://127.0.0.1:11884/status
curl -X POST http://127.0.0.1:11884/shutdown   # remote graceful shutdown
```

### 3. Configuration Reference

```yaml
log_level: info                 # debug|info|warn|error
listen: 127.0.0.1:11884         # aggregated status HTTP (empty disables); external listen requires password
password: ""                    # constant-time auth password
dontcrack_binary: dontcrack     # DontCrack executable (overridable per service)
shutdown_grace: 10s             # shutdown grace period

services:
  - name: web                   # unique; [A-Za-z0-9._-], 1-64
    path: /opt/app/web          # required: child process path (passed as -path)
    args: "--addr :8080"        # passed as -args
    pre: "mkdir -p /run"        # pre-start command (passed as -pre)
    env: "TZ=Asia/Shanghai"     # child env vars (passed as -env)
    auto_restart: true          # auto-restart the child (passed as -auto-restart)
    max_retries: -1             # DontCrack retry limit (-1 unlimited)
    start_now: true             # start the child immediately
    probe_cmd: "curl -sf http://127.0.0.1:8080/healthz"
    probe_interval: 10
    probe_timeout: 3
    probe_failure_limit: 3
    listen_address: 127.0.0.1   # this DontCrack instance's HTTP listen
    port: 11883                 # must be unique per instance (conflict refuses startup)
    password: ""                # instance management password (required for external listen)
    file_log: false
    log_path: "./logs/dontcrack"
    log_life_day: 7
    dontcrack_binary: ""        # per-service override
    dontcrack_env: ["HTTP_PROXY=http://proxy:7890"]  # extra env for the DontCrack process (same-name overrides inherited)
    restart: always             # manager-level policy: always|on-failure|never
    backoff: 3s                 # restart backoff base (exponential)
    backoff_max: 60s            # backoff cap
    depends_on: [db]            # startup dependencies
    depend_timeout: 60s         # dependency-ready wait cap
    enabled: true
```

### 4. HTTP API

| Path | Method | Description |
| ---- | ---- | ---- |
| `/healthz` | GET | 200 when all enabled services are healthy; otherwise 503 |
| `/status` | GET | per-service JSON detail (DontCrack PID / child PID / heartbeat snapshot / restarts / last exit code) |
| `/shutdown` | POST | trigger graceful shutdown of everything |

Auth priority matches DontCrack: `Authorization: Bearer <pw>` > `X-DontCrack-Password` > `?password=`.

### 5. Shutdown Semantics

1. Receive SIGINT/SIGTERM (or `/shutdown`);
2. Cancel heartbeat refresh and dependency waits; send SIGTERM to every DontCrack instance
   (DontCrack stops its child first);
3. Wait for all to exit, capped by `shutdown_grace`;
4. SIGKILL fallback for survivors after the grace period (fail-closed, no orphans left).

### 6. Division of Labor with DontCrack

| Layer | Responsibility |
| -- | ---- |
| DontCrack instance | start / probe / auto-restart / logs of a single child process |
| DontCrack-Manager | multi-instance dependency orchestration, supervision & recovery of DontCrack processes, aggregated status |

### 7. Full End-to-End Integration Test

The `e2e/` directory provides a reproducible end-to-end test: it builds a **real DontCrack
binary** and supervises 3 services with the root manager (db persistent / web depends on db
with HTTP probe / worker crashing periodically), covering:

1. multi-instance startup and dependency order (db before web);
2. probe health (`healthz=up` + `/heartbeat` snapshot `state=running`);
3. DontCrack-level auto-restart (worker crash auto-restart, state-file counter increments);
4. manager-level restart (kill the DontCrack process → `restart=on-failure` recovers it);
5. no-orphan guarantee (after kill -9 of a DontCrack, its children are cleaned up by process group);
6. aggregated `/healthz` `/status` and graceful `/shutdown` (all processes gone, exit code 0).

```bash
docker run --rm \
  -v $(pwd):/src/dcm \
  -v /path/to/DontCrack4ManyLinux:/src/dc4m \
  -v /path/to/this/repo/e2e:/e2e -w /e2e \
  golang:1.25 bash /e2e/e2e_test.sh
```

> The integration run caught and fixed: ① DontCrack flag-swallowing (a `-args` value starting
> with `-` gets absorbed by the flag package, silently resetting later flags → all flags now
> use the `-flag=value` form + fail-closed guard in DontCrack); ② DontCrack auto-restart never
> firing (`CurrentProcess` not released, restart plan always judged stale); ③ the root
> manager's "no orphans" promise unfulfilled (children orphaned after DontCrack is killed →
> Setpgid process-group management).

### 8. Tests

```bash
go vet ./...
go test -race ./...
```

### 9. Security

See [SECURITY.md](SECURITY.md). Report vulnerabilities privately to the FasterEdge
organization (tyza66@outlook.com).

### 10. License

[Apache 2.0](LICENSE)
