// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: DontCrack 实例状态查询客户端。
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client 是 DontCrack /heartbeat 与 /healthz 的查询客户端。
type Client struct {
	addr     string // host:port
	password string
	httpc    *http.Client
}

// NewClient 创建客户端。
func NewClient(addr, password string) *Client {
	return &Client{
		addr:     addr,
		password: password,
		httpc:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Heartbeat 查询 /heartbeat, 返回 DontCrack 的进程状态快照。
func (c *Client) Heartbeat(ctx context.Context) (*HeartbeatInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.addr+"/heartbeat", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("heartbeat HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var hb HeartbeatInfo
	if err := json.Unmarshal(body, &hb); err != nil {
		return nil, fmt.Errorf("解析 heartbeat: %w", err)
	}
	return &hb, nil
}

// Healthy 查询 /healthz: DontCrack 管理器与子进程都健康才返回 true。
// 返回 (可达性, 健康) ; 网络错误时可达性为 false。
func (c *Client) Healthy(ctx context.Context) (reachable, healthy bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.addr+"/healthz", nil)
	if err != nil {
		return false, false, err
	}
	c.applyAuth(req)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return true, resp.StatusCode == http.StatusOK, nil
}

func (c *Client) applyAuth(req *http.Request) {
	if c.password != "" {
		req.Header.Set("Authorization", "Bearer "+c.password)
	}
}

// HostPort 拼接 host:port。
func HostPort(host string, port int) string {
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}
