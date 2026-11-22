// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 根管理器聚合状态 HTTP 服务。
package status

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
)

// ShutdownFunc 由 main 注入, POST /shutdown 时触发全量优雅停机。
type ShutdownFunc func() error

// Server 是根管理器的聚合状态 HTTP 服务。
type Server struct {
	prov     Provider
	password string
	shutdown ShutdownFunc
	log      *logging.Logger
	srv      *http.Server
}

// NewServer 创建聚合状态服务(addr 形如 "127.0.0.1:11884"; password 空则不鉴权)。
func NewServer(addr, password string, prov Provider, shutdown ShutdownFunc, log *logging.Logger) *Server {
	s := &Server{prov: prov, password: password, shutdown: shutdown, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/shutdown", s.handleShutdown)
	mux.HandleFunc("/", s.handleRoot)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler 返回 HTTP 处理器(供测试注入 httptest.NewServer)。
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Serve 阻塞启动 HTTP 服务。
func (s *Server) Serve() error {
	if s.log != nil {
		s.log.Infof("聚合状态服务监听 %s", s.srv.Addr)
	}
	err := s.srv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown 优雅关闭 HTTP 服务。
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// checkPassword 与 DontCrack 家族一致:
// 凭据来源优先级 Authorization: Bearer > X-DontCrack-Password > query 参数; 常数时间比较。
func (s *Server) checkPassword(r *http.Request) error {
	if s.password == "" {
		return nil
	}
	pw := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		pw = strings.TrimPrefix(auth, "Bearer ")
	} else if v := r.Header.Get("X-DontCrack-Password"); v != "" {
		pw = v
	} else {
		pw = r.URL.Query().Get("password")
	}
	if subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) == 1 {
		return nil
	}
	return errors.New("unauthorized")
}

// handleHealthz 是 liveness 探针: 只回答"根管理器自己还活着吗"。
// 刻意不因业务服务不健康而返回 503 —— liveness 失败会让 K8s 重启整个 Pod,
// 把可自愈的业务抖动放大成架构性故障; 业务就绪与否由 /readyz 表达。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.checkPassword(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.prov.Alive() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	http.Error(w, "manager not alive", http.StatusServiceUnavailable)
}

// handleReadyz 是 readiness 探针: 全部启用服务的 DependsOn 依赖就绪且探针通过
// 才返回 200 —— 供 K8s / 上游负载均衡判断是否把流量导给这套实例。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.checkPassword(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.prov.Ready() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if err := s.checkPassword(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s.prov.Status())
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if err := s.checkPassword(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.shutdown != nil {
		if err := s.shutdown(); err != nil {
			http.Error(w, "shutdown failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("DontCrack-Manager: 根管理器聚合状态服务\nGET /healthz(存活)  GET /readyz(依赖就绪)  GET /status  POST /shutdown\n"))
}
