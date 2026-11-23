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
  first), then the **whole process tree** is force-killed after the grace period;
- **Process-tree containment**: a separate process group (Setpgid) on unix, one Job Object
  (`KILL_ON_JOB_CLOSE`) per instance on Windows — orphan children are swept before restart;
  Linux additionally enables a child subreaper to sweep children that DontCrack moved into
  their own process group and the kernel then reparented to this manager (see section 5);
- **Aggregated status**: optional root HTTP service — `/healthz` (liveness: the manager itself
  is alive), `/readyz` (readiness: every `depends_on` dependency is ready and probes pass),
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
curl http://127.0.0.1:11884/healthz   # liveness: manager process alive -> 200
curl http://127.0.0.1:11884/readyz    # readiness: all deps ready and probes pass -> 200 else 503
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
| `/healthz` | GET | **liveness**: 200 as long as the manager itself is alive — deliberately independent of service health (a business blip must not restart the instance) |
| `/readyz` | GET | **readiness**: 200 only when every enabled service's `depends_on` dependencies are ready and probes pass; otherwise 503 |
| `/status` | GET | per-service JSON detail (DontCrack PID / child PID / heartbeat snapshot / restarts / last exit code) |
| `/shutdown` | POST | trigger graceful shutdown of everything |

The two probes are **intentionally different**, mirroring Kubernetes `livenessProbe` vs
`readinessProbe`:

- `/healthz` only reports whether the manager process can still answer queries. It stays 200
  when a service crashes or a dependency never comes up — otherwise liveness failure makes the
  orchestrator restart the whole instance, turning a self-healing blip into an architectural outage;
- `/readyz` shares the **same** readiness predicate with the internal `waitDeps`, so "readyz is
  green" and "a dependent may start" are the same fact, with no divergence between the two.
  With no enabled services it fail-closed returns 503.

Auth priority matches DontCrack: `Authorization: Bearer <pw>` > `X-DontCrack-Password` > `?password=`.

### 5. Shutdown Semantics and Process-Tree Containment

Shutdown:

1. Receive SIGINT/SIGTERM (or `/shutdown`);
2. Cancel heartbeat refresh and dependency waits; request graceful stop from every DontCrack
   instance (SIGTERM / Windows equivalent);
3. Wait for all to exit, capped by `shutdown_grace`;
4. Force-terminate the **whole process tree** for survivors after the grace period
   (fail-closed, no orphans left).

The tree container is platform-specific; its capability boundaries are stated plainly:

| Platform | Container | Graceful stop | Force kill | Boundary |
| ---- | ------ | -------- | -------- | -------- |
| Linux | separate process group (`Setpgid`) + child subreaper | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)`, then sweep reparented orphans by `PPid` | a child that calls `setsid` again leaves nothing observable behind (see below) |
| other unix (macOS/BSD) | separate process group (`Setpgid`) | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)` | no subreaper equivalent, so a child DontCrack put in its own group cannot be swept when the group broadcast misses it |
| Windows | one Job Object per instance (`KILL_ON_JOB_CLOSE`) | `GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT)` (SIGTERM equivalent) | `TerminateJobObject` (whole tree) | with no console (service launch) the graceful channel may fail → force kill after grace; a child with `BREAKAWAY_OK` escapes the Job |

### Why Linux needs an extra subreaper

Group broadcast alone is **not enough**, and end-to-end testing exposed it: DontCrack also
calls `setpgid` on its own children (it converges grandchildren per group), so those children
land in **their own** process group, disjoint from this manager's `kill(-pgid)` range. After
DontCrack is `kill -9`ed the child becomes an orphan — the group broadcast cannot reach it,
the kernel reparents it to init, and the manager cannot even see a `PPid` relation any more.
The "no orphan children" promise silently broke (in production: the port stayed occupied by
the leftover process, and the restarted service could never bind).

The fix has two parts. At startup `prctl(PR_SET_CHILD_SUBREAPER)` (`orphan_linux.go`) makes
orphans that would have gone to init reparent to this manager instead, restoring an
observable `PPid` relation. Then every `killOrphans` sweeps a second time by `PPid == self`
in addition to the group broadcast, killing each hit via its own process group and reaping it
with `wait4` (an unreaped process is a zombie, which residual checks still flag).

Two boundaries the sweep must respect, both pinned by tests (`orphan_linux_test.go`):

- **never steal `cmd.Wait`'s exit status**: the DontCrack processes still reaped by this
  manager's `cmd.Wait` are excluded, otherwise `waitExit` blocks forever and the service stays
  `running` for good;
- **broadcast to a group only when the orphan is its own group leader**: after DontCrack's
  `setpgid` the leader *is* the child, so the broadcast also catches its descendants; when it
  is not the leader, `-pid` could hit an unrelated group, so the sweep degrades to killing that
  single process.

On non-Linux platforms it is a no-op (`orphan_other.go`): Windows does not need it, because a
process joining a Job passes membership to its children and `TerminateJobObject` still catches
grandchildren; macOS/BSD have no `prctl`, so this capability boundary is recorded as a
degradation.

> Windows previously degraded to `taskkill /T /F` (hard kill only, no graceful semantics). It
> now uses Job Object containment plus a CTRL_BREAK graceful request: the force-kill guarantee
> comes from the kernel instead of an external command, and closing the Job handle on abnormal
> manager exit triggers `KILL_ON_JOB_CLOSE` to sweep residual members — a stronger no-orphan
> guarantee than `taskkill`. If containment fails (e.g. this process is already inside a Job that
> forbids nesting) startup is not blocked; a warning is logged and force-kill degrades to
> "DontCrack process only".

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
5. no-orphan guarantee (after kill -9 of a DontCrack, its children are swept by process-tree containment);
6. probe semantics split and graceful `/shutdown` (all processes gone, exit code 0).

Script paths are parameterized, so they run both in the Docker layout and in a CI checkout:

```bash
# Docker layout (equivalent to the historical usage)
docker run --rm \
  -v $(pwd):/src/dcm \
  -v /path/to/DontCrack4ManyLinux:/src/dc4m \
  -v /path/to/this/repo/e2e:/e2e -w /e2e \
  golang:1.27 bash /e2e/e2e_test.sh

# CI checkout layout (no docker needed)
DC4M_DIR=/path/to/DontCrack4ManyLinux DCM_DIR=/path/to/DontCrack-Manager \
  bash e2e/e2e_test.sh

# probe-semantics run (dependency never ready → /readyz 503 while /healthz 200)
DC4M_DIR=/path/to/DontCrack4ManyLinux DCM_DIR=/path/to/DontCrack-Manager \
  bash e2e/readyz_test.sh
```

`.github/workflows/e2e.yml` runs both scripts on a Linux runner, so the README's
"reproducible end-to-end" claim now has CI evidence; the run logs and `/status` snapshot are
uploaded as artifacts.

> The integration run caught and fixed: ① DontCrack flag-swallowing (a `-args` value starting
> with `-` gets absorbed by the flag package, silently resetting later flags → all flags now
> use the `-flag=value` form + fail-closed guard in DontCrack); ② DontCrack auto-restart never
> firing (`CurrentProcess` not released, restart plan always judged stale); ③ the root
> manager's "no orphans" promise unfulfilled (children orphaned after DontCrack is killed →
> process-group / Job Object containment); ④ `/healthz` and `/readyz` were the same
> implementation with different wording → split into real liveness/readiness semantics.

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
