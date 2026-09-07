// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
package config

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, yamlStr string) *Manager {
	t.Helper()
	m, err := Parse([]byte(yamlStr))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	return m
}

func mustFail(t *testing.T, yamlStr, wantSub string) {
	t.Helper()
	m, err := Parse([]byte(yamlStr))
	if err == nil {
		t.Fatalf("期望解析失败(含 %q), 实际成功: %+v", wantSub, m)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("错误信息应包含 %q, 实际: %v", wantSub, err)
	}
}

const goodBase = `
log_level: info
listen: 127.0.0.1:11884
dontcrack_binary: dontcrack
services:
  - name: web
    path: /opt/app/web
    args: "--addr :8080"
    auto_restart: true
    max_retries: -1
    start_now: true
    probe_cmd: "curl -sf http://127.0.0.1:8080/healthz"
    port: 11883
    restart: always
    backoff: 1s
    backoff_max: 10s
`

func TestParseGoodConfig(t *testing.T) {
	svc := mustParse(t, goodBase).Services[0]
	if svc.Name != "web" || svc.Path != "/opt/app/web" {
		t.Fatalf("字段解析错误: %+v", svc)
	}
	if !svc.IsEnabled() {
		t.Fatal("enabled 缺省应为 true")
	}
	if svc.MaxRetries != -1 {
		t.Fatalf("max_retries = %d, 期望 -1", svc.MaxRetries)
	}
	if svc.Restart != RestartAlways {
		t.Fatalf("restart = %q, 期望 always", svc.Restart)
	}
}

func TestDefaultsApplied(t *testing.T) {
	svc := mustParse(t, `
services:
  - name: a
    path: /bin/true
`).Services[0]
	if svc.Port != DefaultPort {
		t.Fatalf("port 默认 = %d, 期望 %d", svc.Port, DefaultPort)
	}
	if svc.ListenAddress != "127.0.0.1" {
		t.Fatalf("listen_address 默认 = %q", svc.ListenAddress)
	}
	if svc.Backoff.Duration != DefaultBackoff || svc.BackoffMax.Duration != DefaultBackoffMax {
		t.Fatalf("backoff 默认错误: %v/%v", svc.Backoff, svc.BackoffMax)
	}
	if svc.DependTimeout.Duration != DefaultDependTimeout {
		t.Fatalf("depend_timeout 默认 = %v", svc.DependTimeout)
	}
	if svc.ProbeInterval != 30 || svc.ProbeTimeout != 5 || svc.ProbeFailureLimit != 3 {
		t.Fatalf("probe 默认错误: %d/%d/%d", svc.ProbeInterval, svc.ProbeTimeout, svc.ProbeFailureLimit)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	mustFail(t, strings.Replace(goodBase, "auto_restart: true", "auto_restartt: true", 1), "not found")
}

func TestDuplicateName(t *testing.T) {
	mustFail(t, `
services:
  - name: a
    path: /bin/true
    port: 11883
  - name: a
    path: /bin/false
    port: 11884
`, "重复")
}

func TestDuplicatePort(t *testing.T) {
	mustFail(t, `
services:
  - name: a
    path: /bin/true
    port: 11883
  - name: b
    path: /bin/false
    port: 11883
`, "端口 11883 冲突")
}

func TestDisabledServiceSkipsPort(t *testing.T) {
	m := mustParse(t, `
services:
  - name: a
    path: /bin/true
    port: 11883
  - name: b
    path: /bin/false
    port: 11883
    enabled: false
`)
	if len(m.Services) != 2 {
		t.Fatalf("服务数 = %d", len(m.Services))
	}
	if m.Services[1].IsEnabled() {
		t.Fatal("b 应禁用")
	}
}

func TestMissingPath(t *testing.T) {
	mustFail(t, `
services:
  - name: a
    port: 11883
`, "path 必填")
}

func TestBadRestartPolicy(t *testing.T) {
	mustFail(t, strings.Replace(goodBase, "restart: always", "restart: maybe", 1), "restart")
}

func TestBadBackoff(t *testing.T) {
	mustFail(t, strings.Replace(goodBase, "backoff: 1s", "backoff: -2s", 1), "backoff")
	mustFail(t, strings.Replace(goodBase, "backoff_max: 10s", "backoff_max: 500ms", 1), "backoff_max")
}

func TestBadDurationString(t *testing.T) {
	mustFail(t, strings.Replace(goodBase, "backoff: 1s", "backoff: one-second", 1), "duration")
}

func TestSelfDependency(t *testing.T) {
	mustFail(t, `
services:
  - name: web
    path: /opt/app/web
    port: 11883
    depends_on: [web]
`, "依赖自身")
}

func TestMissingDependency(t *testing.T) {
	mustFail(t, `
services:
  - name: web
    path: /opt/app/web
    port: 11883
    depends_on: [ghost]
`, "依赖不存在的服务")
}

func TestDependencyCycle(t *testing.T) {
	mustFail(t, `
services:
  - name: a
    path: /bin/true
    port: 11883
    depends_on: [b]
  - name: b
    path: /bin/false
    port: 11884
    depends_on: [a]
`, "依赖环")
}

func TestDisabledDependencyRejected(t *testing.T) {
	mustFail(t, `
services:
  - name: ghost
    path: /opt/ghost
    port: 11883
    enabled: false
  - name: waiter
    path: /opt/waiter
    port: 11884
    depends_on: [ghost]
`, "依赖已禁用")
}

func TestExternalListenRequiresPassword(t *testing.T) {
	mustFail(t, `
listen: 0.0.0.0:11884
services:
  - name: a
    path: /bin/true
`, "password")
}

func TestExternalServiceListenRequiresPassword(t *testing.T) {
	mustFail(t, `
services:
  - name: a
    path: /bin/true
    listen_address: 0.0.0.0
`, "password")
}

func TestExternalListenWithPasswordOK(t *testing.T) {
	m := mustParse(t, `
listen: 0.0.0.0:11884
password: s3cret
services:
  - name: a
    path: /bin/true
    listen_address: 0.0.0.0
    password: svcpw
`)
	if m.Password != "s3cret" {
		t.Fatalf("password = %q", m.Password)
	}
}

func TestEmptyServicesRejected(t *testing.T) {
	mustFail(t, "listen: 127.0.0.1:11884\nservices: []", "services 不能为空")
}

func TestDependencyOrderValid(t *testing.T) {
	m := mustParse(t, `
services:
  - name: db
    path: /opt/db
    port: 11883
  - name: web
    path: /opt/web
    port: 11884
    depends_on: [db]
`)
	if _, ok := m.Lookup("db"); !ok {
		t.Fatal("Lookup(db) 失败")
	}
	if _, ok := m.Lookup("ghost"); ok {
		t.Fatal("Lookup(ghost) 应失败")
	}
}

func TestServiceNameValidation(t *testing.T) {
	mustFail(t, strings.Replace(goodBase, "name: web", "name: 'bad name!'", 1), "非法")
	mustFail(t, strings.Replace(goodBase, "name: web", "name: ''", 1), "非法")
}

func TestPortRangeValidation(t *testing.T) {
	// 负数端口: 直接拒绝(不再透传给 DontCrack 导致监听失败)
	mustFail(t, strings.Replace(goodBase, "port: 11883", "port: -1", 1), "port")
	// 超范围端口: 直接拒绝
	mustFail(t, strings.Replace(goodBase, "port: 11883", "port: 65536", 1), "port")
	// 合法边界 1 与 65535 应通过
	mustParse(t, strings.Replace(goodBase, "port: 11883", "port: 1", 1))
	mustParse(t, strings.Replace(goodBase, "port: 11883", "port: 65535", 1))
}
