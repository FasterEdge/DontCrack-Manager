// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
package status

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
)

// fakeDontCrack 模拟 DontCrack 的 /heartbeat 与 /healthz。
func fakeDontCrack(t *testing.T, password string, healthy bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !checkReqPassword(r, password) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(HeartbeatInfo{
			Version:      "1.0.20261123",
			State:        "running",
			Timestamp:    time.Now().Format(time.RFC3339),
			ProcessPID:   4242,
			ProcessPath:  "/opt/app/web",
			RestartCount: 2,
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !checkReqPassword(r, password) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if healthy {
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// checkReqPassword 镜像 DontCrack 的鉴权方案(Bearer > X-DontCrack-Password > query)。
func checkReqPassword(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}
	pw := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		pw = strings.TrimPrefix(auth, "Bearer ")
	} else if v := r.Header.Get("X-DontCrack-Password"); v != "" {
		pw = v
	} else {
		pw = r.URL.Query().Get("password")
	}
	return pw == expected
}

func TestFetchHeartbeat(t *testing.T) {
	srv := fakeDontCrack(t, "", true)
	cli := NewClient(strings.TrimPrefix(srv.URL, "http://"), "")
	hb, err := cli.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if hb.State != "running" || hb.ProcessPID != 4242 || hb.RestartCount != 2 {
		t.Fatalf("解析错误: %+v", hb)
	}
	if hb.ProcessPath != "/opt/app/web" {
		t.Fatalf("ProcessPath = %q", hb.ProcessPath)
	}
}

func TestFetchHeartbeatWithPassword(t *testing.T) {
	srv := fakeDontCrack(t, "s3cret", true)
	cli := NewClient(strings.TrimPrefix(srv.URL, "http://"), "s3cret")
	hb, err := cli.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("带密码 Heartbeat: %v", err)
	}
	if hb.State != "running" {
		t.Fatalf("State = %q", hb.State)
	}
}

func TestFetchHeartbeatWrongPassword(t *testing.T) {
	srv := fakeDontCrack(t, "s3cret", true)
	cli := NewClient(strings.TrimPrefix(srv.URL, "http://"), "wrong")
	if _, err := cli.Heartbeat(context.Background()); err == nil {
		t.Fatal("错误密码应失败")
	}
}

func TestClientHealthy(t *testing.T) {
	ok := fakeDontCrack(t, "", true)
	bad := fakeDontCrack(t, "", false)
	cok := NewClient(strings.TrimPrefix(ok.URL, "http://"), "")
	cbad := NewClient(strings.TrimPrefix(bad.URL, "http://"), "")
	reach, healthy, err := cok.Healthy(context.Background())
	if err != nil || !reach || !healthy {
		t.Fatalf("健康实例: reach=%v healthy=%v err=%v", reach, healthy, err)
	}
	reach, healthy, err = cbad.Healthy(context.Background())
	if err != nil || !reach || healthy {
		t.Fatalf("不健康实例: reach=%v healthy=%v err=%v", reach, healthy, err)
	}
}

// stubProvider 是 Provider 的测试替身。
type stubProvider struct {
	status []ServiceStatus
	alive  bool
	ready  bool
}

func (p *stubProvider) Status() []ServiceStatus { return p.status }
func (p *stubProvider) Alive() bool             { return p.alive }
func (p *stubProvider) Ready() bool             { return p.ready }

// doReq 执行一次带上下文的 HTTP 请求并返回状态码与响应体。
func doReq(t *testing.T, method, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestAggregatorEndpoints(t *testing.T) {
	var shutdownCalls atomic.Int32
	prov := &stubProvider{
		status: []ServiceStatus{
			{Name: "web", Desired: "running", State: "running", Pid: 111},
			{Name: "db", Desired: "running", State: "backoff"},
		},
		alive: true,
		ready: true,
	}
	srv := NewServer("127.0.0.1:0", "", prov, func() error { shutdownCalls.Add(1); return nil }, logging.NewDefault())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// /healthz(liveness)与 /readyz(readiness)在全绿时都应 200。
	code, body := doReq(t, http.MethodGet, ts.URL+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("healthz = %d, 期望 200", code)
	}
	if string(body) != "ok" {
		t.Fatalf("healthz 响应体 = %q, 期望 ok", body)
	}
	code, body = doReq(t, http.MethodGet, ts.URL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz = %d, 期望 200", code)
	}
	if string(body) != "ready" {
		t.Fatalf("readyz 响应体 = %q, 期望 ready", body)
	}

	// /status
	code, body = doReq(t, http.MethodGet, ts.URL+"/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d, 期望 200", code)
	}
	var got []ServiceStatus
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解码 status: %v", err)
	}
	if len(got) != 2 || got[0].Name != "web" || got[0].Pid != 111 {
		t.Fatalf("status 内容错误: %+v", got)
	}

	// /shutdown
	code, _ = doReq(t, http.MethodPost, ts.URL+"/shutdown")
	if code != http.StatusOK || shutdownCalls.Load() != 1 {
		t.Fatalf("shutdown: status=%d calls=%d", code, shutdownCalls.Load())
	}

	// 业务不就绪但监管器存活: readyz 必须 503, healthz 必须仍 200 ——
	// 这是两类探针语义分离的核心断言(liveness 不因业务抖动失败而要重启进程)。
	prov.ready = false
	code, body = doReq(t, http.MethodGet, ts.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("依赖未就绪 readyz = %d, 期望 503", code)
	}
	if !strings.Contains(string(body), "not ready") {
		t.Fatalf("readyz 响应体 = %q, 期望含 not ready", body)
	}
	code, _ = doReq(t, http.MethodGet, ts.URL+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("业务不就绪时 healthz = %d, 期望仍 200(liveness 与业务解耦)", code)
	}

	// 监管器自身失活: 两者都应 503。
	prov.alive = false
	code, _ = doReq(t, http.MethodGet, ts.URL+"/healthz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("监管器失活 healthz = %d, 期望 503", code)
	}
	code, _ = doReq(t, http.MethodGet, ts.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("监管器失活 readyz = %d, 期望 503", code)
	}
}

// TestReadyzNotReadyWhenDependencyNotReady 证明 /readyz 的依赖语义:
// 依赖服务永远起不来(二进制不存在)时, 依赖方即便进程活着也不 ready ——
// 这正是此前 /healthz 与 /readyz 实现同构时无法表达的区分。
//
// 真实 supervisor 的链路测试在 internal/supervisor 侧(TestReadyzBlockedWhenDependencyNotReady),
// 此处只验证 HTTP 层: provider 的 ready=false 必须映射为 503 且报文可读。
func TestReadyzNotReadyWhenDependencyNotReady(t *testing.T) {
	prov := &stubProvider{
		status: []ServiceStatus{
			{Name: "web", Desired: "running", State: "running", Pid: 222},
		},
		alive: true,
		ready: false,
	}
	srv := NewServer("127.0.0.1:0", "", prov, nil, logging.NewDefault())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 依赖未就绪: readyz 必须 503 且报文说明"未就绪"。
	code, body := doReq(t, http.MethodGet, ts.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("依赖未就绪 readyz = %d, 期望 503", code)
	}
	if !strings.Contains(string(body), "not ready") {
		t.Fatalf("readyz 报文 = %q, 期望含 not ready", body)
	}
	// liveness 与业务无关: 业务不就绪监管器仍存活(200)。
	if code, _ = doReq(t, http.MethodGet, ts.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("业务不就绪时 healthz = %d, 期望 200", code)
	}

	// /status 必须仍可读(503 的 readyz 不应连带打挂诊断接口)。
	code, _ = doReq(t, http.MethodGet, ts.URL+"/status")
	if code != http.StatusOK {
		t.Fatalf("依赖未就绪时 status = %d, 期望 200", code)
	}
}

func TestAggregatorAuth(t *testing.T) {
	prov := &stubProvider{alive: true, ready: true}
	srv := NewServer("127.0.0.1:0", "topsecret", prov, nil, logging.NewDefault())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	get := func(token string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("无凭据 = %d, 期望 401", code)
	}
	if code := get("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错误凭据 = %d, 期望 401", code)
	}
	if code := get("topsecret"); code != http.StatusOK {
		t.Fatalf("正确凭据 = %d, 期望 200", code)
	}

	// 常数时间比较路径也接受 header 与 query。
	code, _ := doReq(t, http.MethodGet, ts.URL+"/healthz?password=topsecret")
	if code != http.StatusOK {
		t.Fatalf("query 凭据 = %d, 期望 200", code)
	}
}
