#!/bin/bash
# DontCrack-Manager 完整体端到端联调: 真实 DontCrack 二进制 + 3 服务场景。
# 场景: db(常驻) / web(依赖db+HTTP探针) / worker(周期性崩溃, DontCrack auto-restart)。
#
# 路径参数化(默认与 docker 运行布局一致, 也兼容 CI checkout 布局):
#   E2E_DIR    本脚本与 manager.yaml 所在目录
#   DC4M_DIR   DontCrack4ManyLinux 仓库根(提供真实 DontCrack 与 childproc)
#   DCM_DIR    DontCrack-Manager 仓库根
set -u
export GOPROXY=https://goproxy.cn,direct

E2E_DIR="${E2E_DIR:-$(cd "$(dirname "$0")" && pwd)}"
DC4M_DIR="${DC4M_DIR:-/src/dc4m}"
DCM_DIR="${DCM_DIR:-/src/dcm}"
BIN="$E2E_DIR/bin"
# 目标平台可执行扩展名: 主场景在 Linux CI 上跑(EXT 为空), 但脚本也允许在
# Windows/Git Bash 上本地复跑时正确产出/调用 .exe。
EXT=""
[ "$(go env GOOS)" = "windows" ] && EXT=".exe"
# worker 的 state-file 由子进程自己写, 必须是它所在平台能解析的路径:
# Linux CI 用 /tmp, Windows 本地复跑要把 /tmp 换成真实的 Windows 临时目录
# (用 cygpath -m 转成 C:/... 形式: 反斜杠会被 YAML 双引号当转义符吃掉)。
WORKER_STATE="${TMPDIR:-/tmp}/dcm-e2e-worker.state"
if [ "$(go env GOOS)" = "windows" ] && command -v cygpath >/dev/null 2>&1; then
  WORKER_STATE="$(cygpath -m "$WORKER_STATE")"
fi
mkdir -p "$BIN" "$E2E_DIR/logs"
cd "$E2E_DIR"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $1"; }

# PY: 可用的 Python 解释器。Windows 的 python3 常是 Microsoft Store 占位符
# (which 成功但一运行就报错), 因此必须真跑一次 -c 才算可用。
PY=""
for cand in python3 python; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import sys, json, urllib.request' >/dev/null 2>&1; then
    PY="$cand"; break
  fi
done

# http_code <path>: 返回 HTTP 状态码(轮询由调用方控制)。
http_code() {
  if command -v wget >/dev/null 2>&1; then
    wget -q -O /dev/null --timeout=2 -S "http://127.0.0.1:11899$1" 2>&1 \
      | grep -o 'HTTP/[0-9.]* [0-9]*' | tail -1 | awk '{print $2}'
  else
    # CI(ubuntu-latest)自带 wget; 本地 Windows/Git Bash 可能只有 curl。
    curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:11899$1"
  fi
}

# http_get <path>: 取响应体(CI 用 wget, 本地 Git Bash 可回落 curl)。
http_get() {
  if command -v wget >/dev/null 2>&1; then
    wget -q -O - --timeout=2 "http://127.0.0.1:11899$1" 2>/dev/null
  else
    curl -s --max-time 2 "http://127.0.0.1:11899$1" 2>/dev/null
  fi
}

# post_empty <path>: POST 空体(CI 用 wget, 本地 Git Bash 可回落 curl)。
post_empty() {
  if command -v wget >/dev/null 2>&1; then
    wget -q -O - --timeout=5 --post-data='' "http://127.0.0.1:11899$1" 2>/dev/null
  else
    curl -s -X POST --max-time 5 "http://127.0.0.1:11899$1" 2>/dev/null
  fi
}

# hard_kill <pid>: 模拟"DontCrack 进程被强杀"。Git Bash 的 kill 只对 MSYS 进程
# 生效, 原生 Windows 进程要经 taskkill; Linux CI 上走 kill -9。
hard_kill() {
  if [ "$(go env GOOS)" = "windows" ]; then
    taskkill //F //PID "$1" >/dev/null 2>&1
  else
    kill -9 "$1" 2>/dev/null
  fi
}

# alive_pid <pid>: 判断进程是否仍存活。Git Bash 的 kill -0 对 bash 拉起的原生
# Windows 子进程可能失效, 因此再退一步用 tasklist 兜底; Linux CI 上只用 kill -0。
alive_pid() {
  kill -0 "$1" 2>/dev/null && return 0
  if [ "$(go env GOOS)" = "windows" ]; then
    tasklist //FI "PID eq $1" //NH 2>/dev/null | grep -q "$1" && return 0
  fi
  return 1
}

echo "=== 0. 工具自检 ==="
# 脚本的 HTTP 请求优先用 wget(CI ubuntu-latest 自带), 缺失时回落到 curl;
# 二者都缺才是真问题(否则所有状态码断言都会空转)。
if command -v wget >/dev/null 2>&1; then
  ok "wget 可用"
elif command -v curl >/dev/null 2>&1; then
  ok "wget 缺失, 回落 curl"
else
  bad "wget 与 curl 均缺失(HTTP 断言无法执行)"
fi
[ -n "$PY" ] && ok "Python 解释器可用: $PY(JSON 解析依赖)" || bad "无可用 Python 解释器(JSON 解析依赖, 需 json/urllib.request)"
[ -d "$DC4M_DIR" ] && ok "DontCrack 仓库存在: $DC4M_DIR" || bad "DontCrack 仓库缺失: $DC4M_DIR"

echo "=== 1. 构建 ==="
cd "$DC4M_DIR" && CGO_ENABLED=0 go build -o "$BIN/dontcrack$EXT" . && ok "DontCrack 构建" || bad "DontCrack 构建失败"
cd "$DC4M_DIR/example/childproc" && CGO_ENABLED=0 go build -o "$BIN/childproc$EXT" . && ok "childproc 构建" || bad "childproc 构建失败"
cd "$DCM_DIR" && CGO_ENABLED=0 go build -o "$BIN/dcm$EXT" ./cmd/dontcrack-manager && ok "DontCrack-Manager 构建" || bad "DontCrack-Manager 构建失败"
cd "$E2E_DIR" && CGO_ENABLED=0 go build -o "$BIN/tinyhttp$EXT" tinyhttp.go && ok "tinyhttp 构建" || bad "tinyhttp 构建失败"

echo "=== 2. 启动根管理器 ==="
rm -f "$WORKER_STATE" "$E2E_DIR/dcm.log"
# 渲染配置中的 ${E2E_BIN} 占位(manager.yaml 里的二进制路径), 使同一份配置
# 在 docker 布局(/e2e/bin) 与 CI checkout 布局下都能跑。
# envsubst 来自 gettext-base(ubuntu-latest 自带), 但为避免 runner 差异导致整轮
# 空跑, 缺失时用 sed 兜底只替换本脚本真正会用到的两个变量。
RENDERED_CFG="$E2E_DIR/manager.rendered.yaml"
if command -v envsubst >/dev/null 2>&1; then
  E2E_BIN="$BIN" EXT="$EXT" WORKER_STATE="$WORKER_STATE" \
    envsubst < "$E2E_DIR/manager.yaml" > "$RENDERED_CFG"
else
  sed -e "s|\${E2E_BIN}|$BIN|g" -e "s|\${EXT}|$EXT|g" \
      -e "s|\${WORKER_STATE}|$WORKER_STATE|g" \
    "$E2E_DIR/manager.yaml" > "$RENDERED_CFG"
fi
"$BIN/dcm$EXT" -config "$RENDERED_CFG" > "$E2E_DIR/dcm.log" 2>&1 &
DCM_PID=$!
sleep 3
alive_pid $DCM_PID && ok "根管理器进程存活(pid=$DCM_PID)" || bad "根管理器启动即退出: $(tail -5 "$E2E_DIR/dcm.log")"

echo "=== 3. 聚合 API: liveness / readiness 分离 ==="
# 两类探针语义必须不同: /healthz(liveness)只反映监管器自身存活;
# /readyz(readiness)反映全部 DependsOn 依赖就绪且探针通过。
GOT_GREEN=""
code=""
for i in $(seq 1 30); do
  code=$(http_code /readyz)
  if [ "$code" = "200" ]; then GOT_GREEN=yes; break; fi
  sleep 1
done
[ "$GOT_GREEN" = "yes" ] && ok "/readyz 达到 200(依赖就绪+探针健康)" || bad "/readyz 未达 200(最后状态码=$code)"

# liveness 必须恒为 200: 它不因业务抖动失败(与 readiness 的关键区分点)。
code=$(http_code /healthz)
[ "$code" = "200" ] && ok "/healthz 恒为 200(liveness 与业务就绪解耦)" || bad "/healthz = $code, 期望 200"

# /status 也允许 curl 回退(本地无 wget 环境)。
status_json=$(http_get /status)
echo "$status_json" > "$E2E_DIR/status.json"
"$PY" - <<'PYEOF' && ok "/status JSON 可解析且含 3 服务" || bad "/status JSON 异常"
import json
data=json.load(open("status.json", encoding="utf-8"))
names=[s["name"] for s in data]
assert set(names)=={"db","web","worker"}, names
print("  服务:", names)
PYEOF

echo "=== 4. 依赖顺序(db 先于 web) ==="
"$PY" - <<'PYEOF' && ok "依赖顺序正确(db.started_at <= web.started_at)" || bad "依赖顺序错误"
import json
data=json.load(open("status.json", encoding="utf-8"))
def get(n):
    for s in data:
        if s["name"]==n: return s
db,web=get("db"),get("web")
print("  db.started_at =",db.get("started_at"))
print("  web.started_at =",web.get("started_at"))
assert db.get("started_at") and web.get("started_at")
assert db["started_at"] <= web["started_at"]
PYEOF
true

echo "=== 5. 探针健康 + 心跳快照 ==="
"$PY" - <<'PYEOF' && ok "web 探针健康(healthz=up + heartbeat.state=running)" || bad "web 探针/心跳异常"
import json
data=json.load(open("status.json", encoding="utf-8"))
web=[s for s in data if s["name"]=="web"][0]
print("  web.healthz =",web.get("healthz")," child_state =",web.get("child_state")," hb =",(web.get("heartbeat") or {}).get("state"))
assert web.get("healthz")=="up", web
assert (web.get("heartbeat") or {}).get("state")=="running", web
PYEOF
true

echo "=== 6. worker 崩溃由 DontCrack auto-restart 重启(state-file 计数) ==="
for i in $(seq 1 25); do
  if [ -f "$WORKER_STATE" ] && [ "$(cat "$WORKER_STATE" 2>/dev/null)" -ge 2 ] 2>/dev/null; then break; fi
  sleep 1
done
cnt=$(cat "$WORKER_STATE" 2>/dev/null || echo 0)
[ "$cnt" -ge 2 ] 2>/dev/null && ok "worker 已重启 $cnt 次(state-file 计数, 期望 >=2)" || bad "worker 重启计数不足(=$cnt)"

echo "=== 7. Manager 层重启: kill web 的 DontCrack 进程 ==="
old_pid=$("$PY" -c "import json;d=json.load(open('status.json', encoding='utf-8'));print([s for s in d if s['name']=='web'][0]['pid'])")
hard_kill "$old_pid"
NEW_PID=""
for i in $(seq 1 15); do
  http_get /status > "$E2E_DIR/status.json"
  pid=$("$PY" -c "import json;d=json.load(open('status.json', encoding='utf-8'));print([s for s in d if s['name']=='web'][0].get('pid',''))" 2>/dev/null)
  st=$("$PY" -c "import json;d=json.load(open('status.json', encoding='utf-8'));print([s for s in d if s['name']=='web'][0]['state'])" 2>/dev/null)
  if [ -n "$pid" ] && [ "$pid" != "$old_pid" ] && [ "$st" = "running" ]; then NEW_PID="$pid"; break; fi
  sleep 1
done
[ -n "$NEW_PID" ] && ok "web 的 DontCrack 被 kill 后由 Manager 重启(pid $old_pid -> $NEW_PID, state=running)" || bad "web 未在宽限内重启(期望 restart=on-failure)"

echo "=== 8. 优雅停机: POST /shutdown ==="
post_empty /shutdown | head -1
for i in $(seq 1 20); do
  alive_pid $DCM_PID || break
  sleep 1
done
if alive_pid $DCM_PID; then
  bad "根管理器未在 20s 内退出"
  hard_kill $DCM_PID
else
  ok "根管理器已退出(优雅停机)"
fi

echo "=== 9. 无残留进程 ==="
# ps -eo pid,comm 是 GNU procps 语法(Windows Git Bash 的 ps 不支持), 因此分平台:
if ps -eo pid,comm >/dev/null 2>&1; then
  LEFT=$(ps -eo pid,comm | grep -E 'dontcrack|childproc|tinyhttp' | grep -v grep | wc -l)
  [ "$LEFT" = "0" ] && ok "无残留进程" || { bad "存在残留进程:"; ps -eo pid,comm | grep -E 'dontcrack|childproc|tinyhttp' | grep -v grep; killall -9 dontcrack childproc tinyhttp 2>/dev/null; }
else
  # Windows: 用 tasklist 查残留(联调脚本在 Linux CI 上跑主场景)。
  LEFT=$(tasklist //FI "IMAGENAME eq dontcrack.exe" 2>/dev/null | grep -c 'dontcrack.exe' || true)
  [ "$LEFT" = "0" ] && ok "无残留进程(Windows tasklist)" || bad "存在残留进程(dontcrack.exe x$LEFT)"
fi

echo ""
echo "==================== 联调结论: PASS=$PASS FAIL=$FAIL ===================="
[ "$FAIL" = "0" ] && exit 0 || exit 1
