# DontCrack-Manager — Root Process Manager for DontCrack

> FasterEdge open-source project · [Github](https://github.com/FasterEdge) · [Gitee](https://gitee.com/FasterEdge)

A single DontCrack instance supervises exactly **one** child process. DontCrack-Manager
supervises **many** DontCrack instances at once — a system-level multi-process root
manager for environments without a process manager (embedded Linux, slim containers).

## Highlights

- **Real contract**: reuses DontCrack CLI flags and HTTP API (`/heartbeat`, `/healthz`) as-is;
- **Dependency ordering**: `depends_on` gates start order; a service starts only after its
  dependencies are ready (process alive + optional probe healthy);
- **Backoff restarts**: if a DontCrack process itself dies, restart per
  `always | on-failure | never` with exponential backoff (`backoff_max` cap; reset after 30s uptime);
- **Graceful shutdown**: SIGINT/SIGTERM forwarded to every DontCrack (which stops its child
  first), SIGKILL fallback after the grace period;
- **Aggregated status**: optional root HTTP service — `/healthz`, `/status`, `/shutdown`;
- **Fail-closed security**: dependency cycles / duplicate ports / bad policies refuse startup;
  external listeners require a password (constant-time compare, same scheme as DontCrack).

## Usage

```bash
go build -o dontcrack-manager ./cmd/dontcrack-manager
./dontcrack-manager -config examples/manager.yaml -check   # validate only
./dontcrack-manager -config /etc/dontcrack-manager.yaml    # run as root manager
curl http://127.0.0.1:11884/status
```

See [README.md](README.md) for the full configuration reference and API docs.

## Tests

```bash
go vet ./...
go test -race ./...
```

## License

[Apache 2.0](LICENSE)