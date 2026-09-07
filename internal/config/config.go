// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: DontCrack 多进程根管理器配置加载与校验。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RestartPolicy 描述 DontCrack 进程自身退出后的重启策略。
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"     // 无论退出码, 一律重启
	RestartOnFailure RestartPolicy = "on-failure" // 仅非零退出码(或异常)时重启
	RestartNever     RestartPolicy = "never"      // 退出后不再拉起
)

// 默认值 (与 DontCrack 家族默认行为对齐)。
const (
	DefaultPort          = 11883
	DefaultLogCapacity   = 200
	DefaultLogMaxLine    = 1048576
	DefaultLogLifeDay    = 7
	DefaultProbeInterval = 30
	DefaultProbeTimeout  = 5
	DefaultProbeLimit    = 3
	DefaultBackoff       = 3 * time.Second
	DefaultBackoffMax    = 60 * time.Second
	DefaultDependTimeout = 60 * time.Second
	DefaultShutdownGrace = 10 * time.Second
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Duration 支持 YAML 字符串形式的 time.Duration("3s"/"1m30s")。
type Duration struct {
	time.Duration
}

// UnmarshalYAML 把 YAML 字符串解析为 Duration。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration 必须是字符串, 如 \"3s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("非法 duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Manager 是根管理器全局配置。
type Manager struct {
	LogLevel        string    `yaml:"log_level"`        // debug|info|warn|error
	Listen          string    `yaml:"listen"`           // 聚合状态 HTTP 地址(host:port), 空禁用
	Password        string    `yaml:"password"`         // 聚合状态鉴权密码(对外监听必填)
	DontCrackBinary string    `yaml:"dontcrack_binary"` // DontCrack 可执行文件(默认 $PATH 中 dontcrack)
	ShutdownGrace   Duration  `yaml:"shutdown_grace"`   // 停止 DontCrack 实例的宽限时间
	Services        []Service `yaml:"services"`         // 服务列表

	index map[string]int // name -> Services 下标
}

// Service 描述一个由单个 DontCrack 实例监管的子进程。
// 前段字段 (Path/Args/Pre/...) 原样透传给 DontCrack CLI flags;
// 后段字段 (Restart/Backoff/DependsOn/...) 属于 Manager 层监管语义。
type Service struct {
	// --- 透传给 DontCrack 的字段 ---
	Name              string   `yaml:"name"`
	Path              string   `yaml:"path"` // 必填: 被监管的子进程路径
	Args              string   `yaml:"args"`
	Pre               string   `yaml:"pre"`
	Env               string   `yaml:"env"`
	AutoRestart       bool     `yaml:"auto_restart"` // 子进程退出后 DontCrack 自动重启
	MaxRetries        int      `yaml:"max_retries"`  // -1 = 无限
	StartNow          bool     `yaml:"start_now"`
	ProbeCmd          string   `yaml:"probe_cmd"` // 空禁用探针
	ProbeInterval     int      `yaml:"probe_interval"`
	ProbeTimeout      int      `yaml:"probe_timeout"`
	ProbeFailureLimit int      `yaml:"probe_failure_limit"`
	ListenAddress     string   `yaml:"listen_address"` // DontCrack HTTP 监听地址(仅 host)
	Port              int      `yaml:"port"`           // 每实例必须唯一
	Password          string   `yaml:"password"`       // DontCrack 实例管理密码
	FileLog           bool     `yaml:"file_log"`
	LogPath           string   `yaml:"log_path"`
	LogLifeDay        int      `yaml:"log_life_day"`
	DontCrackBinary   string   `yaml:"dontcrack_binary"` // 服务级覆盖
	DontCrackEnv      []string `yaml:"dontcrack_env"`    // 追加给 DontCrack 进程本身的环境变量("K=V")

	// --- Manager 层监管字段 ---
	Restart       RestartPolicy `yaml:"restart"`        // DontCrack 进程退出策略
	Backoff       Duration      `yaml:"backoff"`        // 重启退避基数
	BackoffMax    Duration      `yaml:"backoff_max"`    // 退避上限
	DependsOn     []string      `yaml:"depends_on"`     // 启动依赖(等依赖就绪后再启动)
	DependTimeout Duration      `yaml:"depend_timeout"` // 等待依赖就绪上限
	Enabled       *bool         `yaml:"enabled"`        // 缺省 true

	// 内部透传参数(不对外暴露 YAML 字段, 与 DontCrack CLI 默认一致)。
	logCapacity int
}

// IsEnabled 返回服务是否启用(缺省启用)。
func (s *Service) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// Binary 返回该服务使用的 DontCrack 可执行文件。
func (s *Service) Binary(global string) string {
	if s.DontCrackBinary != "" {
		return s.DontCrackBinary
	}
	return global
}

// Lookup 返回服务名到下标映射。
func (m *Manager) Lookup(name string) (*Service, bool) {
	if m == nil || m.index == nil {
		return nil, false
	}
	i, ok := m.index[name]
	if !ok || i < 0 || i >= len(m.Services) {
		return nil, false
	}
	return &m.Services[i], true
}

// applyDefaults 填充零值字段的默认值(注意: 只补 0, 不覆盖负值, 负值由 Validate 拒绝)。
func (m *Manager) applyDefaults() {
	if m.LogLevel == "" {
		m.LogLevel = "info"
	}
	if m.DontCrackBinary == "" {
		m.DontCrackBinary = "dontcrack"
	}
	if m.ShutdownGrace.Duration == 0 {
		m.ShutdownGrace.Duration = DefaultShutdownGrace
	}
	for i := range m.Services {
		svc := &m.Services[i]
		if svc.ListenAddress == "" {
			svc.ListenAddress = "127.0.0.1"
		}
		if svc.Port == 0 {
			svc.Port = DefaultPort
		}
		if svc.MaxRetries == 0 {
			svc.MaxRetries = 3
		}
		if svc.ProbeInterval == 0 {
			svc.ProbeInterval = DefaultProbeInterval
		}
		if svc.ProbeTimeout == 0 {
			svc.ProbeTimeout = DefaultProbeTimeout
		}
		if svc.ProbeFailureLimit == 0 {
			svc.ProbeFailureLimit = DefaultProbeLimit
		}
		if svc.LogCapacity() == 0 {
			svc.LogCapacitySet(DefaultLogCapacity)
		}
		if svc.LogLifeDay == 0 {
			svc.LogLifeDay = DefaultLogLifeDay
		}
		if svc.Restart == "" {
			svc.Restart = RestartAlways
		}
		if svc.Backoff.Duration == 0 {
			svc.Backoff.Duration = DefaultBackoff
		}
		if svc.BackoffMax.Duration == 0 {
			svc.BackoffMax.Duration = DefaultBackoffMax
		}
		if svc.DependTimeout.Duration == 0 {
			svc.DependTimeout.Duration = DefaultDependTimeout
		}
	}
}

// LogCapacity 返回日志容量默认值(与 DontCrack 默认一致)。
func (s *Service) LogCapacity() int { return s.logCapacity }

// LogCapacitySet 设置日志容量。
func (s *Service) LogCapacitySet(v int) { s.logCapacity = v }

// Load 读取并校验配置文件。
func Load(path string) (*Manager, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return m, nil
}

// Parse 解析配置字节流并校验。
func Parse(data []byte) (*Manager, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // 未知字段直接报错: 拼写错误 fail-closed
	var m Manager
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate 校验配置, 返回聚合的全部错误(fail-closed)。
func (m *Manager) Validate() error {
	var errs []string

	if len(m.Services) == 0 {
		errs = append(errs, "services 不能为空(至少一个服务)")
	}
	if m.LogLevel != "debug" && m.LogLevel != "info" && m.LogLevel != "warn" && m.LogLevel != "error" {
		errs = append(errs, fmt.Sprintf("log_level %q 非法(可选 debug|info|warn|error)", m.LogLevel))
	}
	if m.ShutdownGrace.Duration <= 0 {
		errs = append(errs, "shutdown_grace 必须为正时长")
	}
	if m.DontCrackBinary == "" {
		errs = append(errs, "dontcrack_binary 不能为空")
	}

	// 监听地址: 非回环必须配置密码(fail-closed, 与 DontCrack 家族一致)。
	if m.Listen != "" {
		if _, _, err := splitListen(m.Listen); err != nil {
			errs = append(errs, fmt.Sprintf("listen %q 非法: %v", m.Listen, err))
		} else if !isLoopbackHost(hostOf(m.Listen)) && m.Password == "" {
			errs = append(errs, "对外监听("+m.Listen+")必须配置 password, 否则管理接口暴露给整个网络")
		}
	}

	// 服务级校验。
	m.index = make(map[string]int, len(m.Services))
	portSeen := make(map[int]string)
	for i := range m.Services {
		svc := &m.Services[i]
		if _, dup := m.index[svc.Name]; dup {
			errs = append(errs, fmt.Sprintf("服务名 %q 重复", svc.Name))
		}
		m.index[svc.Name] = i

		if !nameRe.MatchString(svc.Name) {
			errs = append(errs, fmt.Sprintf("服务名 %q 非法(允许 [A-Za-z0-9._-], 1-64 字符)", svc.Name))
		}
		if svc.IsEnabled() {
			if svc.Path == "" {
				errs = append(errs, fmt.Sprintf("服务 %q: path 必填", svc.Name))
			}
			if prev, dup := portSeen[svc.Port]; dup {
				errs = append(errs, fmt.Sprintf("服务 %q 与 %q 端口 %d 冲突(每实例必须唯一)", svc.Name, prev, svc.Port))
			}
			portSeen[svc.Port] = svc.Name
		}
		if svc.Port < 1 || svc.Port > 65535 {
			errs = append(errs, fmt.Sprintf("服务 %q: port %d 非法(允许 1-65535)", svc.Name, svc.Port))
		}
		if svc.MaxRetries < -1 {
			errs = append(errs, fmt.Sprintf("服务 %q: max_retries 必须 >= -1", svc.Name))
		}
		if svc.ProbeInterval <= 0 || svc.ProbeTimeout <= 0 || svc.ProbeFailureLimit <= 0 {
			errs = append(errs, fmt.Sprintf("服务 %q: probe_interval/probe_timeout/probe_failure_limit 必须为正", svc.Name))
		}
		if svc.Restart != RestartAlways && svc.Restart != RestartOnFailure && svc.Restart != RestartNever {
			errs = append(errs, fmt.Sprintf("服务 %q: restart %q 非法(可选 always|on-failure|never)", svc.Name, svc.Restart))
		}
		if svc.Backoff.Duration <= 0 {
			errs = append(errs, fmt.Sprintf("服务 %q: backoff 必须为正时长", svc.Name))
		}
		if svc.BackoffMax.Duration < svc.Backoff.Duration {
			errs = append(errs, fmt.Sprintf("服务 %q: backoff_max 不能小于 backoff", svc.Name))
		}
		if svc.DependTimeout.Duration <= 0 {
			errs = append(errs, fmt.Sprintf("服务 %q: depend_timeout 必须为正时长", svc.Name))
		}
		if !isLoopbackHost(svc.ListenAddress) && svc.Password == "" {
			errs = append(errs, fmt.Sprintf("服务 %q: 对外监听 %s 必须配置 password", svc.Name, svc.ListenAddress))
		}
		for _, kv := range svc.DontCrackEnv {
			if !strings.Contains(kv, "=") {
				errs = append(errs, fmt.Sprintf("服务 %q: dontcrack_env 项 %q 必须为 K=V 形式", svc.Name, kv))
				break
			}
		}
	}

	// 依赖校验: 存在性、自依赖、环、禁用依赖。
	for i := range m.Services {
		svc := &m.Services[i]
		for _, dep := range svc.DependsOn {
			if dep == svc.Name {
				errs = append(errs, fmt.Sprintf("服务 %q 不能依赖自身", svc.Name))
				continue
			}
			j, ok := m.index[dep]
			if !ok {
				errs = append(errs, fmt.Sprintf("服务 %q 依赖不存在的服务 %q", svc.Name, dep))
				continue
			}
			if !m.Services[j].IsEnabled() {
				errs = append(errs, fmt.Sprintf("服务 %q 依赖已禁用的服务 %q", svc.Name, dep))
			}
		}
	}
	if cycle := m.findCycle(); cycle != "" {
		errs = append(errs, "检测到依赖环: "+cycle)
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// findCycle 返回依赖环路径(无环返回空串)。
func (m *Manager) findCycle() string {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make([]int, len(m.Services))
	var path []string
	var visit func(i int) string
	visit = func(i int) string {
		state[i] = visiting
		path = append(path, m.Services[i].Name)
		for _, dep := range m.Services[i].DependsOn {
			j, ok := m.index[dep]
			if !ok {
				continue
			}
			switch state[j] {
			case visiting:
				// 找到环: 从 dep 处截取
				start := 0
				for k, n := range path {
					if n == dep {
						start = k
						break
					}
				}
				cycle := append(append([]string{}, path[start:]...), dep)
				return strings.Join(cycle, " -> ")
			case unvisited:
				if c := visit(j); c != "" {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[i] = done
		return ""
	}
	for i := range m.Services {
		if state[i] == unvisited {
			if c := visit(i); c != "" {
				return c
			}
		}
	}
	return ""
}

// splitListen 拆分 host:port。
func splitListen(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	if port == "" {
		return "", "", errors.New("缺少端口")
	}
	return host, port, nil
}

func hostOf(addr string) string {
	if h, _, err := splitListen(addr); err == nil {
		return h
	}
	return addr
}

// isLoopbackHost 判断主机是否为回环地址("" 视为全部接口, 非回环)。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
