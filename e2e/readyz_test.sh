#!/bin/bash
# DontCrack-Manager 探针语义端到端: 证明 /healthz(liveness) 与 /readyz(readiness)
# 语义**真的不同** —— 依赖服务起不来时, readyz 必须 503 而 healthz 必须仍 200。
#
# 场景: gateway 依赖 backend, 而 backend 的 dontcrack_binary 故意指向不存在的路径,
#      因此 backend 永远无法就绪 → gateway 也永不被拉起。
#
# 路径参数化(与 e2e_test.sh 一致, 默认兼容 CI checkout 布局)。
set -u
export GOPROXY=https://goproxy.cn,direct

E2E_DIR="${E2E_DIR:-$(cd "$(dirname "$0")" && pwd)}"
DC4M_DIR="${DC4M_DIR:-/src/dc4m}"
DCM_DIR="${DCM_DIR:-/src/dcm}"
BIN="$E2E_DIR/bin"
# Windows 本地复跑时 go build 的产物带 .exe 后缀(CI 的 Linux runner 上为空)。
EXT=""
[ "$(go env GOOS)" = "windows" ] && EXT=".exe"
mkdir -p "$BIN" "$E2E_DIR/logs"
cd "$E2E_DIR"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $1"; }

http_code() {
  if command -v wget >/dev/null 2>&1; then
    wget -q -O /dev/null --timeout=2 -S "http://127.0.0.1:11898$1" 2>&1 \
      | grep -o 'HTTP/[0-9.]* [0-9]*' | tail -1 | awk '{print $2}'
  else
    # CI(ubuntu-latest)自带 wget; 本地 Windows/Git Bash 可能只有 curl。
    curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:11898$1"
  fi
}

echo "=== 0. 工具自检 ==="
# HTTP 请求优先用 wget(CI ubuntu-latest 自带), 缺失时回落 curl; 二者都缺才是真问题
# (否则所有状态码断言都会空转)。
if command -v wget >/dev/null 2>&1; then
  ok "wget 可用"
elif command -v curl >/dev/null 2>&1; then
  ok "wget 缺失, 回落 curl"
else
  bad "wget 与 curl 均缺失(HTTP 断言无法执行)"
fi
# Windows 的 python3 可能是 Microsoft Store 占位符(which 成功但一跑就报错),
# 因此必须真跑一次 import 才算可用。
PY=""
for cand in python3 python; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import json, urllib.request' >/dev/null 2>&1; then
    PY="$cand"; break
  fi
done
[ -n "$PY" ] && ok "Python 解释器可用: $PY(JSON 解析依赖)" || bad "无可用 Python 解释器(JSON 解析依赖, 需 json/urllib.request)"

echo "=== 1. 构建 ==="
cd "$DCM_DIR" && CGO_ENABLED=0 go build -o "$BIN/dcm$EXT" ./cmd/dontcrack-manager \
  && ok "DontCrack-Manager 构建" || bad "DontCrack-Manager 构建失败"

echo "=== 2. 启动根管理器(依赖永远起不来) ==="
# 进程存活判定: 优先 kill -0(对 bash 拉起的子进程可用); Git Bash 下 $! 与原生
# Windows PID 可能不一致, 因此再退一步用 tasklist 兜底。
pid_alive() {
  kill -0 "$1" 2>/dev/null && return 0
  if [ "$(go env GOOS)" = "windows" ]; then
    tasklist //FI "PID eq $1" //NH 2>/dev/null | grep -q "$1" && return 0
  fi
  return 1
}
rm -f "$E2E_DIR/probe.log"
"$BIN/dcm$EXT" -config "$E2E_DIR/manager-readyz.yaml" > "$E2E_DIR/probe.log" 2>&1 &
DCM_PID=$!
sleep 3
# 进程存活判定: 优先 kill -0(对 bash 拉起的子进程可用); Git Bash 下 $! 与原生
# Windows PID 可能不一致, 因此再退一步用 HTTP 端口探测兜底。
pid_alive() {
  kill -0 "$1" 2>/dev/null && return 0
  if [ "$(go env GOOS)" = "windows" ]; then
    tasklist //FI "PID eq $1" //NH 2>/dev/null | grep -q "$1" && return 0
  fi
  return 1
}
pid_alive "$DCM_PID" && ok "根管理器进程存活(pid=$DCM_PID)" || bad "根管理器启动即退出: $(tail -5 "$E2E_DIR/probe.log")"

echo "=== 3. 探针语义差异(核心断言) ==="
code=$(http_code /readyz)
[ "$code" = "503" ] && ok "/readyz = 503(依赖未就绪, 与 /healthz 语义分离)" || bad "/readyz = $code, 期望 503"

code=$(http_code /healthz)
[ "$code" = "200" ] && ok "/healthz = 200(liveness 只反映监管器自身, 不随业务失败)" || bad "/healthz = $code, 期望 200"

# /status 必须仍可读: 探针失败不应连带打挂诊断接口。
code=$(http_code /status)
[ "$code" = "200" ] && ok "/status = 200(诊断接口在依赖未就绪时仍可用)" || bad "/status = $code, 期望 200"

echo "=== 4. 依赖未就绪不得启动依赖方 ==="
"$PY" - <<'PYEOF' && ok "gateway 未启动(pending/backoff, 依赖未就绪)" || bad "gateway 状态异常"
import json, urllib.request
data = json.load(urllib.request.urlopen("http://127.0.0.1:11898/status", timeout=5))
names = {s["name"]: s for s in data}
assert set(names) == {"backend", "gateway"}, sorted(names)
gw = names["gateway"]
print("  gateway.state =", gw.get("state"), " pid =", gw.get("pid"))
# 依赖未就绪 → gateway 永远拿不到启动机会: 要么还在首次等待(pending),
# 要么已因依赖超时进入退避(backoff); 两者都不得有进程与重启记录。
assert gw.get("state") in ("pending", "backoff"), gw
assert not gw.get("pid"), gw
assert gw.get("restarts") == 0, gw
PYEOF

echo "=== 5. 优雅停机 ==="
# wget 优先(CI ubuntu-latest 自带); 本地 Windows/Git Bash 可能只有 curl。
if command -v wget >/dev/null 2>&1; then
  wget -q -O - --timeout=5 --post-data='' http://127.0.0.1:11898/shutdown 2>/dev/null | head -1
else
  curl -s -X POST --max-time 5 http://127.0.0.1:11898/shutdown 2>/dev/null
fi
for i in $(seq 1 20); do
  pid_alive "$DCM_PID" || break
  sleep 1
done
if pid_alive "$DCM_PID"; then
  bad "根管理器未在 20s 内退出"
  if [ "$(go env GOOS)" = "windows" ]; then
    taskkill //F //PID "$DCM_PID" >/dev/null 2>&1
  else
    kill -9 "$DCM_PID" 2>/dev/null
  fi
else
  ok "根管理器已退出(优雅停机)"
fi

echo ""
echo "==================== 探针语义结论: PASS=$PASS FAIL=$FAIL ===================="
[ "$FAIL" = "0" ] && exit 0 || exit 1
