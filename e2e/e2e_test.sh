#!/bin/bash
# DontCrack-Manager 完整体端到端联调: 真实 DontCrack 二进制 + 3 服务场景。
# 场景: db(常驻) / web(依赖db+HTTP探针) / worker(周期性崩溃, DontCrack auto-restart)。
set -u
export GOPROXY=https://goproxy.cn,direct
cd /e2e
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $1"; }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

echo "=== 0. 工具自检 ==="
which wget >/dev/null 2>&1 && ok "wget 可用" || bad "wget 缺失(探针依赖)"
which python3 >/dev/null 2>&1 && ok "python3 可用" || bad "python3 缺失(JSON 解析依赖)"

echo "=== 1. 构建 ==="
cd /src/dc4m && CGO_ENABLED=0 go build -o /e2e/bin/dontcrack . && ok "DontCrack 构建" || bad "DontCrack 构建失败"
cd /src/dc4m/example/childproc && CGO_ENABLED=0 go build -o /e2e/bin/childproc . && ok "childproc 构建" || bad "childproc 构建失败"
cd /src/dcm && CGO_ENABLED=0 go build -o /e2e/bin/dcm ./cmd/dontcrack-manager && ok "DontCrack-Manager 构建" || bad "DontCrack-Manager 构建失败"
cd /e2e && CGO_ENABLED=0 go build -o /e2e/bin/tinyhttp tinyhttp.go && ok "tinyhttp 构建" || bad "tinyhttp 构建失败"

echo "=== 2. 启动根管理器 ==="
rm -f /tmp/worker.state /e2e/dcm.log
mkdir -p /e2e/logs
/e2e/bin/dcm -config /e2e/manager.yaml > /e2e/dcm.log 2>&1 &
DCM_PID=$!
sleep 3
kill -0 $DCM_PID 2>/dev/null && ok "根管理器进程存活(pid=$DCM_PID)" || bad "根管理器启动即退出: $(tail -5 /e2e/dcm.log)"

echo "=== 3. 聚合 API ==="
# /healthz 需要 db+web 全绿(worker 周期崩会短暂 503, 轮询等它恢复)
GOT_GREEN=""
code=""
for i in $(seq 1 30); do
  code=$(wget -q -O /dev/null --timeout=2 -S http://127.0.0.1:11899/healthz 2>&1 | grep -o 'HTTP/[0-9.]* [0-9]*' | tail -1 | awk '{print $2}')
  if [ "$code" = "200" ]; then GOT_GREEN=yes; break; fi
  sleep 1
done
[ "$GOT_GREEN" = "yes" ] && ok "/healthz 达到 200(依赖就绪+探针健康)" || bad "/healthz 未达 200(最后状态码=$code)"

status_json=$(wget -q -O - --timeout=2 http://127.0.0.1:11899/status 2>/dev/null)
echo "$status_json" > /e2e/status.json
python3 - <<'PYEOF' && ok "/status JSON 可解析且含 3 服务" || bad "/status JSON 异常"
import json
data=json.load(open("/e2e/status.json"))
names=[s["name"] for s in data]
assert set(names)=={"db","web","worker"}, names
print("  服务:", names)
PYEOF

echo "=== 4. 依赖顺序(db 先于 web) ==="
python3 - <<'PYEOF' && ok "依赖顺序正确(db.started_at <= web.started_at)" || bad "依赖顺序错误"
import json
data=json.load(open("/e2e/status.json"))
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
python3 - <<'PYEOF' && ok "web 探针健康(healthz=up + heartbeat.state=running)" || bad "web 探针/心跳异常"
import json
data=json.load(open("/e2e/status.json"))
web=[s for s in data if s["name"]=="web"][0]
print("  web.healthz =",web.get("healthz")," child_state =",web.get("child_state")," hb =",(web.get("heartbeat") or {}).get("state"))
assert web.get("healthz")=="up", web
assert (web.get("heartbeat") or {}).get("state")=="running", web
PYEOF
true

echo "=== 6. worker 崩溃由 DontCrack auto-restart 重启(state-file 计数) ==="
for i in $(seq 1 25); do
  if [ -f /tmp/worker.state ] && [ "$(cat /tmp/worker.state 2>/dev/null)" -ge 2 ] 2>/dev/null; then break; fi
  sleep 1
done
cnt=$(cat /tmp/worker.state 2>/dev/null || echo 0)
[ "$cnt" -ge 2 ] 2>/dev/null && ok "worker 已重启 $cnt 次(state-file 计数, 期望 >=2)" || bad "worker 重启计数不足(=$cnt)"

echo "=== 7. Manager 层重启: kill web 的 DontCrack 进程 ==="
old_pid=$(python3 -c "import json;d=json.load(open('/e2e/status.json'));print([s for s in d if s['name']=='web'][0]['pid'])")
kill -9 "$old_pid" 2>/dev/null
NEW_PID=""
for i in $(seq 1 15); do
  j=$(wget -q -O - --timeout=2 http://127.0.0.1:11899/status 2>/dev/null)
  echo "$j" > /e2e/status.json
  pid=$(python3 -c "import json;d=json.load(open('/e2e/status.json'));print([s for s in d if s['name']=='web'][0].get('pid',''))" 2>/dev/null)
  st=$(python3 -c "import json;d=json.load(open('/e2e/status.json'));print([s for s in d if s['name']=='web'][0]['state'])" 2>/dev/null)
  if [ -n "$pid" ] && [ "$pid" != "$old_pid" ] && [ "$st" = "running" ]; then NEW_PID="$pid"; break; fi
  sleep 1
done
[ -n "$NEW_PID" ] && ok "web 的 DontCrack 被 kill 后由 Manager 重启(pid $old_pid -> $NEW_PID, state=running)" || bad "web 未在宽限内重启(期望 restart=on-failure)"

echo "=== 8. 优雅停机: POST /shutdown ==="
wget -q -O - --timeout=5 --post-data='' http://127.0.0.1:11899/shutdown 2>/dev/null | head -1
for i in $(seq 1 20); do
  kill -0 $DCM_PID 2>/dev/null || break
  sleep 1
done
if kill -0 $DCM_PID 2>/dev/null; then
  bad "根管理器未在 20s 内退出"
  kill -9 $DCM_PID 2>/dev/null
else
  ok "根管理器已退出(优雅停机)"
fi

echo "=== 9. 无残留进程 ==="
LEFT=$(ps -eo pid,comm | grep -E 'dontcrack|childproc|tinyhttp' | grep -v grep | wc -l)
[ "$LEFT" = "0" ] && ok "无残留进程" || { bad "存在残留进程:"; ps -eo pid,comm | grep -E 'dontcrack|childproc|tinyhttp' | grep -v grep; killall -9 dontcrack childproc tinyhttp 2>/dev/null; }

echo ""
echo "==================== 联调结论: PASS=$PASS FAIL=$FAIL ===================="
[ "$FAIL" = "0" ] && exit 0 || exit 1