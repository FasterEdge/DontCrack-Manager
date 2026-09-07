// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/config"
	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
)

// stubScript 是"假 DontCrack"存根(必须用 bash: dash 的 wait 在收到信号时不保证立即执行 trap):
//   - 忽略所有 flag(行为由环境变量驱动);
//   - 启动时向 $DCM_TEST_LOG 写入 "started <args...> <ns时间戳>";
//   - 收到 TERM/INT 时写入 "signaled" 并退出 0;
//   - $DCM_TEST_RUNTIME=inf 则常驻, 否则 sleep 后以 $DCM_TEST_EXIT 退出。
const stubScript = `#!/bin/bash
logf="${DCM_TEST_LOG:-}"
[ -n "$logf" ] && echo "started $* $(date +%s%N)" >> "$logf"
trap '[ -n "$logf" ] && echo "signaled" >> "$logf"; exit 0' TERM INT
rt="${DCM_TEST_RUNTIME:-inf}"
code="${DCM_TEST_EXIT:-0}"
if [ "$rt" != "inf" ]; then
  sleep "$rt"
  exit "$code"
fi
while :; do sleep 3600 & wait; done
`

func writeStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dontcrack-stub")
	if err := os.WriteFile(path, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("写存根: %v", err)
	}
	return path
}

func waitMarker(t *testing.T, path string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("等待标记 %q 超时; 当前内容:\n%s", want, string(data))
}

func markerCount(t *testing.T, path string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Count(string(data), "started ") >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("等待 %d 次启动超时; 实际 %d 次:\n%s", want, strings.Count(string(data), "started "), string(data))
}

func newTestSupervisor(t *testing.T, cfgYAML string) *Supervisor {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("配置解析失败: %v", err)
	}
	return New(cfg, logging.New(os.Stderr, logging.LevelWarn))
}

func TestSpawnAndGracefulStop(t *testing.T) {
	stub := writeStub(t)
	logf := filepath.Join(t.TempDir(), "svc.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: web
    path: /opt/app/web
    port: 11883
    dontcrack_env: [DCM_TEST_LOG=`+logf+`]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)

	waitMarker(t, logf, "started", 8*time.Second)

	// 验证 flag 透传("=" 形式, 避免 Go flag 包对 "-" 开头值错位吞并)。
	data, _ := os.ReadFile(logf)
	first := strings.SplitN(string(data), "\n", 2)[0]
	for _, want := range []string{"-port=11883", "-path=/opt/app/web", "-auto-restart=false", "-start-now=false"} {
		if !strings.Contains(first, want) {
			t.Fatalf("透传 flag 缺少 %q: %s", want, first)
		}
	}

	// 优雅停机: 存根收到 SIGTERM 后写入 signaled。
	sup.StopAll(5 * time.Second)
	waitMarker(t, logf, "signaled", 3*time.Second)

	// Healthy 应为 false(全部停止)。
	if sup.Healthy() {
		t.Fatal("停止后 Healthy() 应为 false")
	}
}

func TestRestartAlways(t *testing.T) {
	stub := writeStub(t)
	logf := filepath.Join(t.TempDir(), "crash.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: crashy
    path: /bin/true
    port: 11883
    restart: always
    backoff: 200ms
    backoff_max: 1s
    dontcrack_env: [DCM_TEST_LOG=`+logf+`, DCM_TEST_RUNTIME=0.1, DCM_TEST_EXIT=1]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)

	// 崩溃循环: 至少拉起 3 次。
	markerCount(t, logf, 3, 10*time.Second)

	st := sup.Status()
	if len(st) != 1 {
		t.Fatalf("状态数 = %d", len(st))
	}
	if st[0].Restarts < 2 {
		t.Fatalf("重启次数 = %d, 期望 >= 2", st[0].Restarts)
	}
	sup.StopAll(5 * time.Second)
}

func TestRestartNever(t *testing.T) {
	stub := writeStub(t)
	logf := filepath.Join(t.TempDir(), "once.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: once
    path: /bin/true
    port: 11883
    restart: never
    backoff: 100ms
    dontcrack_env: [DCM_TEST_LOG=`+logf+`, DCM_TEST_RUNTIME=0.1, DCM_TEST_EXIT=1]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)

	waitMarker(t, logf, "started", 8*time.Second)
	// 等待重启周期过去, 确认只启动了一次。
	time.Sleep(1200 * time.Millisecond)
	data, _ := os.ReadFile(logf)
	if strings.Count(string(data), "started ") != 1 {
		t.Fatalf("restart=never 应只启动 1 次, 实际 %d:\n%s", strings.Count(string(data), "started "), string(data))
	}
	sup.StopAll(3 * time.Second)
}

func TestDependencyOrdering(t *testing.T) {
	stub := writeStub(t)
	dir := t.TempDir()
	dbLog := filepath.Join(dir, "db.log")
	webLog := filepath.Join(dir, "web.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: db
    path: /opt/db
    port: 11883
    dontcrack_env: [DCM_TEST_LOG=`+dbLog+`]
  - name: web
    path: /opt/web
    port: 11884
    depends_on: [db]
    depend_timeout: 5s
    dontcrack_env: [DCM_TEST_LOG=`+webLog+`]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)

	waitMarker(t, webLog, "started", 8*time.Second)

	dbData, _ := os.ReadFile(dbLog)
	webData, _ := os.ReadFile(webLog)
	dbFirst := strings.SplitN(strings.TrimSpace(string(dbData)), "\n", 2)[0]
	webFirst := strings.SplitN(strings.TrimSpace(string(webData)), "\n", 2)[0]
	// 行格式: started <args...> <ns时间戳>
	ns := func(line string) int64 {
		fields := strings.Fields(line)
		var v int64
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &v); err != nil {
			t.Fatalf("解析时间戳失败 %q: %v", line, err)
		}
		return v
	}
	if ns(dbFirst) > ns(webFirst) {
		t.Fatalf("依赖顺序错误: db 启动晚于 web\ndb=%s\nweb=%s", dbFirst, webFirst)
	}
	sup.StopAll(5 * time.Second)
}

func TestDependencyTimeoutThenRetry(t *testing.T) {
	stub := writeStub(t)
	logf := filepath.Join(t.TempDir(), "wait.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: ghost
    path: /opt/ghost
    port: 11883
    dontcrack_binary: /nonexistent/dontcrack
    backoff: 100ms
  - name: waiter
    path: /bin/true
    port: 11884
    depends_on: [ghost]
    depend_timeout: 300ms
    backoff: 200ms
    dontcrack_env: [DCM_TEST_LOG=`+logf+`]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)

	// waiter 依赖的服务永远无法就绪: 应反复等待-退避, 但 DontCrack 进程不应被拉起。
	time.Sleep(1500 * time.Millisecond)
	data, _ := os.ReadFile(logf)
	if strings.Contains(string(data), "started") {
		t.Fatalf("依赖未就绪时不应启动 DontCrack:\n%s", string(data))
	}
	st := sup.Status()
	if len(st) != 2 {
		t.Fatalf("状态数 = %d, 期望 2", len(st))
	}
	sup.StopAll(3 * time.Second)
}

func TestSpawnFailureThenBackoff(t *testing.T) {
	// 不存在的 DontCrack 二进制 → 启动失败 → 退避重试(restart=always)。
	logf := filepath.Join(t.TempDir(), "nope.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: /nonexistent/dontcrack
services:
  - name: badbin
    path: /bin/true
    port: 11883
    restart: always
    backoff: 100ms
    dontcrack_env: [DCM_TEST_LOG=`+logf+`]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)

	// 等待状态进入 backoff/failed 之一(多次启动失败)。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := sup.Status()
		if len(st) == 1 && st[0].State != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := sup.Status()
	if len(st) != 1 {
		t.Fatalf("状态数 = %d", len(st))
	}
	if st[0].State != "backoff" && st[0].State != "failed" {
		t.Fatalf("状态 = %s, 期望 backoff/failed", st[0].State)
	}
	if st[0].LastExitError == "" && st[0].Heartbeat == nil {
		t.Fatalf("应有启动错误: %+v", st[0])
	}
	sup.StopAll(3 * time.Second)
}

func TestHealthyWithRunningService(t *testing.T) {
	stub := writeStub(t)
	logf := filepath.Join(t.TempDir(), "ok.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: srv
    path: /bin/true
    port: 11883
    dontcrack_env: [DCM_TEST_LOG=`+logf+`]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)
	waitMarker(t, logf, "started", 8*time.Second)
	if !sup.Healthy() {
		t.Fatal("运行中服务 Healthy() 应为 true")
	}
	sup.StopAll(3 * time.Second)
}

func TestDisabledServiceNotStarted(t *testing.T) {
	stub := writeStub(t)
	logf := filepath.Join(t.TempDir(), "off.log")
	sup := newTestSupervisor(t, `
dontcrack_binary: `+stub+`
services:
  - name: off
    path: /bin/true
    port: 11883
    enabled: false
    dontcrack_env: [DCM_TEST_LOG=`+logf+`]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Run(ctx)
	time.Sleep(800 * time.Millisecond)
	if data, _ := os.ReadFile(logf); strings.Contains(string(data), "started") {
		t.Fatalf("禁用服务不应启动:\n%s", string(data))
	}
	if sup.Healthy() {
		t.Fatal("无启用服务时 Healthy() 应为 false")
	}
	sup.StopAll(3 * time.Second)
}

// TestBuildArgsEqualsForm: buildArgs 必须全量使用 "-flag=value" 形式
// (含探针参数), 任何"空格分隔"形式都会让 Go flag 包把以 '-' 开头的值错位吞并。
func TestBuildArgsEqualsForm(t *testing.T) {
	sup := &Supervisor{cfg: &config.Manager{}}
	svc := &config.Service{
		Name:              "web",
		Path:              "/opt/app/web",
		Args:              "-addr :9090", // 以 '-' 开头的值: 空格形式必炸
		Pre:               "mkdir -p /run",
		Env:               "TZ=Asia/Shanghai",
		AutoRestart:       true,
		MaxRetries:        3,
		StartNow:          true,
		Port:              11884,
		ListenAddress:     "127.0.0.1",
		Password:          "secret",
		LogLifeDay:        7,
		ProbeCmd:          "wget -q -O /dev/null http://127.0.0.1:9090/healthz",
		ProbeInterval:     3,
		ProbeTimeout:      2,
		ProbeFailureLimit: 2,
	}
	svc.LogCapacitySet(200)
	sv := &Service{cfg: svc, sup: sup}
	joined := strings.Join(sv.buildArgs(), " ")

	for _, want := range []string{
		"-path=/opt/app/web",
		"-args=-addr :9090",
		"-pre=mkdir -p /run",
		"-env=TZ=Asia/Shanghai",
		"-auto-restart=true",
		"-max-retries=3",
		"-start-now=true",
		"-port=11884",
		"-listen-address=127.0.0.1",
		"-password=secret",
		"-log-capacity=200",
		"-log-max-line-bytes=1048576",
		"-file-log=false",
		"-log-life-day=7",
		"-probe-cmd=wget -q -O /dev/null http://127.0.0.1:9090/healthz",
		"-probe-interval=3",
		"-probe-timeout=2",
		"-probe-failure-limit=2",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buildArgs 缺少 %q: %s", want, joined)
		}
	}
	// 不允许出现任何空格分隔的 "flag value" 形式(暗病回归防护)。
	for _, bad := range []string{"-args ", "-port ", "-probe-cmd ", "-auto-restart ", "-password "} {
		if strings.Contains(joined, bad) {
			t.Fatalf("buildArgs 混用空格分隔形式: %s", joined)
		}
	}
}
