// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 与 DontCrack /heartbeat 对齐的状态报文。
package status

// HeartbeatInfo 与 DontCrack4ManyLinux core/heartbeat.go 的 JSON 形状保持一致。
type HeartbeatInfo struct {
	Version          string   `json:"version"`
	State            string   `json:"state"` // 当前进程状态(如 running/stopped)
	Info             string   `json:"info"`
	Timestamp        string   `json:"timestamp"`
	Logs             []string `json:"logs"`
	ProcessPID       int      `json:"process_pid"`
	ProcessPath      string   `json:"process_path"`
	RestartCount     int      `json:"restart_count"`
	FileType         string   `json:"file_type"`
	LastExitCode     int      `json:"last_exit_code,omitempty"`
	LastExitTime     string   `json:"last_exit_time,omitempty"`
	LastExitBySignal bool     `json:"last_exit_by_signal,omitempty"`
	LastExitError    string   `json:"last_exit_error,omitempty"`
	ProgramArgs      string   `json:"program_args,omitempty"`
	ExtraEnvRaw      string   `json:"extra_env_raw,omitempty"`
}

// ServiceStatus 是根管理器对单个服务的聚合状态视图。
type ServiceStatus struct {
	Name          string         `json:"name"`
	Desired       string         `json:"desired"`       // running|stopped
	State         string         `json:"state"`         // pending|starting|running|backoff|stopped|failed
	Pid           int            `json:"pid,omitempty"` // DontCrack 进程 PID
	Restarts      int            `json:"restarts"`      // Manager 层重启次数
	StartedAt     string         `json:"started_at,omitempty"`
	LastExitCode  int            `json:"last_exit_code,omitempty"`
	LastExitAt    string         `json:"last_exit_at,omitempty"`
	LastExitError string         `json:"last_exit_error,omitempty"`
	ChildPID      int            `json:"child_pid,omitempty"` // 子进程 PID(来自 heartbeat)
	ChildState    string         `json:"child_state,omitempty"`
	ChildRestarts int            `json:"child_restarts,omitempty"`
	Heartbeat     *HeartbeatInfo `json:"heartbeat,omitempty"` // 最近一次 DontCrack 心跳快照
	Healthz       string         `json:"healthz,omitempty"`   // up|down(仅配置了探针的服务)
}

// Provider 由 supervisor 实现, 供聚合 HTTP 服务只读调用。
type Provider interface {
	Status() []ServiceStatus
	Healthy() bool
}
