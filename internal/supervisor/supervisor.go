// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 多 DontCrack 实例监管核心。
//
// 设计: 单个 DontCrack 实例只监管一个子进程; 本包作为"根管理器"同时拉起并
// 监管多个 DontCrack 进程, 每个服务一个 goroutine 负责: 依赖等待 → 启动 →
// 退出分类 → 按策略退避重启。DontCrack 进程自身意外退出时由本层负责恢复。
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/config"
	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
	"github.com/FasterEdge/DontCrack-Manager/internal/status"
)

// State 是服务在 Manager 层的生命周期状态。
type State string

const (
	StatePending  State = "pending"  // 等待依赖/首次启动
	StateStarting State = "starting" // DontCrack 进程拉起中
	StateRunning  State = "running"  // DontCrack 进程存活
	StateBackoff  State = "backoff"  // 退避等待重启
	StateStopped  State = "stopped"  // 停止(Manager 主动或策略不再重启)
	StateFailed   State = "failed"   // 永久失败(restart=never 且启动失败)
)

// stableThreshold 进程存活超过该时长视为"稳定", 重置退避基数。
const stableThreshold = 30 * time.Second

// Service 是单个服务的监管状态。
type Service struct {
	cfg *config.Service
	sup *Supervisor
	log *logging.Logger

	mu         sync.Mutex
	state      State
	cmd        *exec.Cmd
	tree       procTree      // 进程树收容器(unix 进程组 / windows Job Object)
	exitCh     chan struct{} // cmd.Wait 返回后关闭(close-once)
	spawns     int           // DontCrack 启动总次数
	startedAt  time.Time
	stoppedAt  time.Time
	lastExit   int
	lastExitAt time.Time
	lastErr    error

	// 心跳缓存(每 5s 刷新, 供依赖就绪/状态聚合使用)。
	hb    *status.HeartbeatInfo
	hbErr error
	hbAt  time.Time

	// 退避状态。
	failures int // 连续失败次数

	desired atomic.Bool // 期望运行(StopAll 置 false)
}

// Supervisor 监管全部服务。
type Supervisor struct {
	cfg    *config.Manager
	log    *logging.Logger
	svcs   map[string]*Service
	mu     sync.Mutex
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	alive  atomic.Bool // 监管器自身存活(/healthz liveness), 与业务服务状态无关
}

// New 创建监管器(尚未启动服务)。
func New(cfg *config.Manager, log *logging.Logger) *Supervisor {
	s := &Supervisor{cfg: cfg, log: log, svcs: make(map[string]*Service, len(cfg.Services))}
	s.alive.Store(true)
	return s
}

// Run 启动全部启用服务的监管循环(非阻塞), 并启动心跳刷新。
func (s *Supervisor) Run(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	for i := range s.cfg.Services {
		svcCfg := &s.cfg.Services[i]
		if !svcCfg.IsEnabled() {
			s.log.Infof("服务 %q 已禁用, 跳过", svcCfg.Name)
			continue
		}
		sv := &Service{
			cfg: svcCfg,
			sup: s,
			log: s.log.Child("[svc:" + svcCfg.Name + "]"),
		}
		sv.desired.Store(true)
		sv.setState(StatePending)
		s.mu.Lock()
		s.svcs[svcCfg.Name] = sv
		s.mu.Unlock()
		s.wg.Add(1)
		go sv.run(s.ctx)
	}
	s.wg.Add(1)
	go s.heartbeatLoop(s.ctx)
}

// StopAll 停止全部服务并等待退出: SIGTERM(等价物) → 宽限 → 强制终止整棵树 兜底。
func (s *Supervisor) StopAll(grace time.Duration) {
	if s.cancel == nil {
		return
	}
	s.alive.Store(false)

	s.mu.Lock()
	svcs := make([]*Service, 0, len(s.svcs))
	for _, sv := range s.svcs {
		svcs = append(svcs, sv)
	}
	s.mu.Unlock()

	// 先置期望停止并请求优雅退出(DontCrack 收到后会优雅停掉其子进程)。
	for _, sv := range svcs {
		sv.desired.Store(false)
		sv.log.Infof("发送优雅停止请求(SIGTERM 等价物)停止 DontCrack 实例")
		sv.signal(syscall.SIGTERM)
		sv.treeSignal(func(t procTree) error { return t.graceful() })
	}

	// 进程生命周期由信号管理(见 spawn: 不绑定可取消 ctx, 避免取消时被 SIGKILL
	// 跳过优雅停机)。此处取消 ctx 仅停心跳刷新与依赖等待。
	s.cancel()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()

	select {
	case <-done:
		return
	case <-time.After(grace):
		// 宽限期后强制终止仍存活的进程(fail-closed: 不留孤儿)。
		// 按进程树强杀, 连带 DontCrack 遗留的子进程一起清理。
		for _, sv := range svcs {
			sv.log.Warnf("宽限期内未退出, 强制终止进程树(SIGKILL 等价物)")
			sv.signal(syscall.SIGKILL)
			sv.treeSignal(func(t procTree) error { return t.force() })
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.log.Errorf("部分 DontCrack 实例未能终止, 放弃等待")
		}
	}
}

// Status 返回全部服务的聚合状态(按名称排序, 输出确定)。
func (s *Supervisor) Status() []status.ServiceStatus {
	s.mu.Lock()
	svcs := make([]*Service, 0, len(s.svcs))
	for _, sv := range s.svcs {
		svcs = append(svcs, sv)
	}
	s.mu.Unlock()
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].cfg.Name < svcs[j].cfg.Name })

	out := make([]status.ServiceStatus, 0, len(svcs))
	for _, sv := range svcs {
		sv.mu.Lock()
		ss := status.ServiceStatus{
			Name:          sv.cfg.Name,
			Desired:       boolToStr(sv.desired.Load()),
			State:         string(sv.state),
			Restarts:      maxInt(0, sv.spawns-1), // 重启次数 = 启动总次数 - 1
			LastExitCode:  sv.lastExit,
			LastExitError: errString(sv.lastErr),
			Heartbeat:     sv.hb,
			Healthz:       healthzStr(sv.hbErr),
		}
		if !sv.startedAt.IsZero() {
			ss.StartedAt = sv.startedAt.Format(time.RFC3339)
		}
		if !sv.lastExitAt.IsZero() {
			ss.LastExitAt = sv.lastExitAt.Format(time.RFC3339)
		}
		if sv.cmd != nil && sv.cmd.Process != nil {
			ss.Pid = sv.cmd.Process.Pid
		}
		if sv.hb != nil {
			ss.ChildPID = sv.hb.ProcessPID
			ss.ChildState = sv.hb.State
			ss.ChildRestarts = sv.hb.RestartCount
		}
		sv.mu.Unlock()
		out = append(out, ss)
	}
	return out
}

// Alive 是 liveness 语义: 只反映**监管器自身**是否存活(能正常处理状态查询),
// 与被监管服务的健康状态无关 —— 供 K8s livenessProbe 判断"要不要重启我这个 Pod"。
//
// 这与旧实现有本质区别: 此前 /healthz 与 /readyz 实现完全相同, 只是文案不同,
// 一旦某个业务服务不健康 liveness 就会失败并把进程杀掉, 反而抹掉了自愈机会。
// 现在监管器只要还在响应查询就报 liveness=up(1), 与业务解耦。
func (s *Supervisor) Alive() bool {
	return s.alive.Load()
}

// Ready 是 readiness 语义: 全部启用服务的 DependsOn 依赖均已就绪, 且自身判定通过
// (进程存活 + 探针健康/心跳子进程 running) —— 供 K8s readinessProbe / 上游负载均衡
// 判断"要不要把流量导进来"。
//
// 判定口径(isReady)与依赖等待(waitDeps)共用同一函数, 保证"readyz 通过"与
// "依赖方可以启动"是同一个事实, 不会出现 readyz 说就绪而 waitDeps 仍拒绝的割裂。
// 无任何启用服务时 fail-closed 返回 false(无从证明就绪)。
func (s *Supervisor) Ready() bool {
	s.mu.Lock()
	svcs := make([]*Service, 0, len(s.svcs))
	for _, sv := range s.svcs {
		svcs = append(svcs, sv)
	}
	s.mu.Unlock()
	if len(svcs) == 0 {
		return false
	}
	for _, sv := range svcs {
		if !sv.isReady() {
			return false
		}
	}
	return true
}

// ---- 服务监管循环 ----

func (sv *Service) run(ctx context.Context) {
	defer sv.sup.wg.Done()
	// 服务生命周期结束时收尾进程树: 关闭收容器句柄并清理残留成员。
	// Windows 下这个 defer 尤其关键 —— 服务永久停止(restart=never / 0 退出)时
	// 若不关闭 Job 句柄, KILL_ON_JOB_CLOSE 的"不留孤儿"兜底也就不会触发,
	// DontCrack 遗留的子进程会一直活着。
	defer sv.killOrphans()
	first := true
	for {
		select {
		case <-ctx.Done():
			sv.setState(StateStopped)
			return
		default:
		}
		if !sv.desired.Load() {
			sv.setState(StateStopped)
			return
		}

		if !first {
			d := sv.nextBackoff()
			sv.setState(StateBackoff)
			sv.log.Infof("%v 后重试", d)
			if !sleepCtx(ctx, d) {
				sv.setState(StateStopped)
				return
			}
		}
		first = false

		if !sv.waitDeps(ctx) {
			if ctx.Err() != nil {
				sv.setState(StateStopped)
				return
			}
			sv.log.Warnf("依赖未就绪(等待 %v 超时), 进入退避", sv.cfg.DependTimeout.Duration)
			continue
		}

		if err := sv.spawn(ctx); err != nil {
			if ctx.Err() != nil {
				sv.setState(StateStopped)
				return
			}
			sv.mu.Lock()
			sv.lastErr = err
			sv.failures++
			sv.mu.Unlock()
			sv.log.Errorf("启动 DontCrack 失败: %v", err)
			if sv.cfg.Restart == config.RestartNever {
				sv.setState(StateFailed)
				return
			}
			continue
		}
		sv.failures = 0 // 成功启动, 清零失败计数(存活时长另行重置退避)

		// 阻塞等待 DontCrack 进程退出。
		sv.waitExit(ctx)

		if !sv.desired.Load() || ctx.Err() != nil {
			sv.setState(StateStopped)
			return
		}

		// 进程意外退出 → 按策略决策。
		sv.recordExit()
		switch sv.cfg.Restart {
		case config.RestartNever:
			sv.log.Warnf("restart=never, 不再重启")
			sv.setState(StateStopped)
			return
		case config.RestartOnFailure:
			sv.mu.Lock()
			code := sv.lastExit
			sv.mu.Unlock()
			if code == 0 {
				sv.log.Warnf("DontCrack 以 0 退出(on-failure), 不再重启")
				sv.setState(StateStopped)
				return
			}
		}
		sv.log.Warnf("DontCrack 进程退出, 按策略重启 (restart=%s)", sv.cfg.Restart)
	}
}

// waitDeps 依次等待依赖就绪; 单个依赖超时返回 false。
func (sv *Service) waitDeps(ctx context.Context) bool {
	for _, depName := range sv.cfg.DependsOn {
		dep := sv.sup.service(depName)
		if dep == nil {
			sv.log.Errorf("依赖 %q 不存在", depName)
			return false
		}
		deadline := time.Now().Add(sv.cfg.DependTimeout.Duration)
		for {
			if ctx.Err() != nil {
				return false
			}
			if dep.isReady() {
				sv.log.Infof("依赖 %q 就绪", depName)
				break
			}
			if time.Now().After(deadline) {
				sv.log.Warnf("依赖 %q 未就绪(超时)", depName)
				return false
			}
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return false
			}
		}
	}
	return true
}

// isReady 依赖就绪判定: DontCrack 进程存活, 且(有心跳快照时)其子进程也在运行。
// 注意: 此前仅在配置了探针时才检查子进程状态 —— 无探针服务的子进程崩溃死亡后
// 聚合 /healthz 仍返回 200, 且下游依赖会误判就绪(信息有却不用)。现只要有心跳
// 快照(hb != nil)一律要求 hb.State == running; 启动初期尚无快照时宽容放行
// (spawn 后立即刷新一次心跳)。
func (sv *Service) isReady() bool {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.state != StateRunning || sv.cmd == nil || sv.cmd.Process == nil {
		return false
	}
	hb := sv.hb
	if hb == nil {
		// 启动初期尚无心跳快照: 有探针服务的就绪判定必须依赖子进程状态 ——
		// 无快照不得放行(否则依赖/聚合会早于真实健康误判就绪, 见 e2e 第 3/5 节
		// 时序: 聚合 200 时 web.hb 尚为 nil → 抓快照即 FAIL);
		// 无探针服务宽容放行(无子进程状态依据, stub/裸运行无 HTTP 也须可用)。
		return sv.cfg == nil || sv.cfg.ProbeCmd == ""
	}
	return strings.EqualFold(hb.State, "running")
}

// spawn 构造并启动 DontCrack 进程, 绑定日志输出。
// 注意: 使用 exec.Command 而非 CommandContext —— 进程生命周期完全由信号管理
// (StopAll: SIGTERM → 宽限 → 强杀整棵树), 避免 ctx 取消时被 SIGKILL 跳过优雅停机。
// 进程纳入进程树收容器(见 newSysProcAttr): 若 DontCrack 被强杀/崩溃, 其子进程
// 会成为孤儿, 重启前按进程树清理, 保证"不留孤儿"(fail-closed)。
func (sv *Service) spawn(ctx context.Context) error {
	return sv.spawnWithTree(ctx, newProcTree())
}

// spawnWithTree 是 spawn 的可测形式: 收容器由调用方注入(测试用它模拟接管失败),
// 其余逻辑完全一致 —— 注入点只有 newProcTree(), 没有别的分支。
func (sv *Service) spawnWithTree(ctx context.Context, tree procTree) error {
	sv.killOrphans() // 上一次进程树若有残留(孤儿子进程), 先清理
	binary := sv.cfg.Binary(sv.sup.cfg.DontCrackBinary)
	args := sv.buildArgs()
	//nolint:noctx // 有意不用 CommandContext: 进程生命周期完全由信号管理(见上方注释)。
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = newSysProcAttr()
	// 自定义环境放最前: Linux getenv 取首个匹配, 这样 dontcrack_env 才能覆盖
	// 继承的同名变量(与 DontCrack 自身 buildChildEnv 的语义一致)。
	cmd.Env = mergeEnv(sv.cfg.DontCrackEnv, os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout 管道: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr 管道: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s: %w", binary, err)
	}

	sv.mu.Lock()
	sv.cmd = cmd
	sv.exitCh = make(chan struct{})
	sv.startedAt = time.Now()
	sv.state = StateRunning
	sv.spawns++
	sv.mu.Unlock()

	// 纳入进程树收容器: 失败不阻断启动(进程已在跑), 但要留下可诊断的告警 ——
	// 收容器缺失意味着强杀兜底只能退化为"只杀 DontCrack 本体", 有留孤儿风险。
	// 两类失败分开说, 因为处置方式不同:
	//   - ErrAlreadyContained: 进程已被上层收容器接管且禁止嵌套, 强杀能力**由上层继承**,
	//     本管理器重复收容既无意义也不可能;
	//   - 其它错误: 收容器真的没建起来, 强杀兜底退化为单进程终止。
	// 该告警是 README 第五节承诺的行为, 有测试钉住(见 attach_failure_test.go)。
	if err := tree.attach(cmd.Process); err != nil {
		if errors.Is(err, ErrAlreadyContained) {
			sv.log.Warnf("进程树收容器接管失败(进程已被上层收容器接管, 强杀兜底交由上层; "+
				"本管理器降级为仅终止 DontCrack 本体): %v", err)
		} else {
			sv.log.Warnf("进程树收容器接管失败(强杀兜底能力降级为仅终止 DontCrack 本体): %v", err)
		}
	}
	sv.mu.Lock()
	sv.tree = tree
	sv.mu.Unlock()

	go sv.scanLines(stdout, "out")
	go sv.scanLines(stderr, "err")
	sv.log.Infof("DontCrack 已启动 pid=%d binary=%s", cmd.Process.Pid, binary)

	// 启动后立即刷新一次心跳(加速依赖就绪判定)。
	// 先清空旧代快照: 上一代 DontCrack 的 hb 不得被新代沿用(否则立即刷新失败时
	// refreshHeartbeat 会保留旧值, 新进程被误判为携带旧状态就绪)。
	sv.mu.Lock()
	sv.hb = nil
	sv.hbErr = nil
	sv.mu.Unlock()
	sv.refreshHeartbeat(ctx)
	return nil
}

// waitExit 阻塞直到 cmd.Wait 返回并关闭 exitCh。
// 注意: 进程生命周期由信号管理(StopAll: SIGTERM → 宽限 → SIGKILL), 上下文取消
// 不会终止 DontCrack(见 spawn 中 exec.Command 而非 CommandContext 的注释)。
func (sv *Service) waitExit(ctx context.Context) {
	sv.mu.Lock()
	cmd := sv.cmd
	sv.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Wait() // 输出管道由 scanLines 消耗, Wait 等待进程结束
	sv.mu.Lock()
	if sv.exitCh != nil {
		close(sv.exitCh)
		sv.exitCh = nil
	}
	sv.mu.Unlock()
}

// mergeEnv 合并环境变量: 自定义项在前、继承项在后 —— 子进程 getenv 取首个匹配,
// 因此自定义项可覆盖继承的同名变量(与 DontCrack buildChildEnv 语义一致)。
func mergeEnv(extra, base []string) []string {
	env := make([]string, 0, len(extra)+len(base))
	env = append(env, extra...)
	env = append(env, base...)
	return env
}

// recordExit 记录退出码并更新退避计数(存活时长达到阈值则重置)。
func (sv *Service) recordExit() {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	code := -1
	if sv.cmd != nil && sv.cmd.ProcessState != nil {
		code = sv.cmd.ProcessState.ExitCode()
	}
	uptime := time.Since(sv.startedAt)
	sv.lastExit = code
	sv.lastExitAt = time.Now()
	if uptime >= stableThreshold {
		sv.failures = 0 // 稳定运行过, 退避归零
	} else {
		sv.failures++
	}
	sv.log.Infof("DontCrack 退出 code=%d uptime=%v", code, uptime.Round(time.Millisecond))
}

// nextBackoff 计算指数退避(min(base×2^(n-1), backoff_max)), 首次重试用 base。
func (sv *Service) nextBackoff() time.Duration {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	base := sv.cfg.Backoff.Duration
	cap := sv.cfg.BackoffMax.Duration
	n := sv.failures
	if n > 0 {
		shift := n - 1
		if shift > 20 {
			shift = 20
		}
		d := base << shift
		if d < base || d > cap { // 溢出保护
			return cap
		}
		return d
	}
	return base
}

// buildArgs 把服务配置翻译为 DontCrack CLI flags。
//
// 注意: 全部使用 "-flag=value" 形式。Go flag 包对 "-flag value" 会无条件把
// 下一个参数当作值; 若 -args 的值以 '-' 开头(常见于子进程参数如
// "-addr :9090"), 后续 flag 会被错位吞并, 甚至导致 flag 解析提前终止
// (bool flag 后紧跟的非 flag 参数会终止解析), 使端口/自动重启等落到默认值。
// "=" 形式对任何值(含空格与前导 '-')都无歧义。
func (sv *Service) buildArgs() []string {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	c := sv.cfg
	args := []string{
		"-path=" + c.Path,
		"-args=" + c.Args,
		"-pre=" + c.Pre,
		"-env=" + c.Env,
		"-auto-restart=" + strconv.FormatBool(c.AutoRestart),
		"-max-retries=" + strconv.Itoa(c.MaxRetries),
		"-start-now=" + strconv.FormatBool(c.StartNow),
		"-port=" + strconv.Itoa(c.Port),
		"-listen-address=" + c.ListenAddress,
		"-password=" + c.Password,
		"-log-capacity=" + strconv.Itoa(c.LogCapacity()),
		"-log-max-line-bytes=" + strconv.Itoa(config.DefaultLogMaxLine),
		"-file-log=" + strconv.FormatBool(c.FileLog),
		"-log-path=" + logPathVal(c.LogPath),
		"-log-life-day=" + strconv.Itoa(c.LogLifeDay),
	}
	if c.ProbeCmd != "" {
		// 探针参数同样用 "=" 形式(见上方注释: 空格形式对以 '-' 开头的值错位)。
		args = append(args,
			"-probe-cmd="+c.ProbeCmd,
			"-probe-interval="+strconv.Itoa(c.ProbeInterval),
			"-probe-timeout="+strconv.Itoa(c.ProbeTimeout),
			"-probe-failure-limit="+strconv.Itoa(c.ProbeFailureLimit),
		)
	}
	return args
}

// logPathVal 返回实际写入 -log-path 的值: 空配置回落 DontCrack 默认
// (与 config.applyDefaults 双保险——经 config.Parse 或测试直构两种途径
// 构造的 Service 都得到一致的 -log-path, 避免空值覆盖 DontCrack 侧默认)。
func logPathVal(p string) string {
	if p == "" {
		return config.DefaultLogPath
	}
	return p
}

// signal 向 DontCrack 进程发送信号。
func (sv *Service) signal(sig os.Signal) {
	sv.mu.Lock()
	cmd := sv.cmd
	sv.mu.Unlock()
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}

// treeSignal 对当前进程树执行动作(graceful/force)。收容器缺失或动作失败时忽略
// (强杀兜底已由 signal 单进程终止覆盖)。
func (sv *Service) treeSignal(fn func(procTree) error) {
	sv.mu.Lock()
	tree := sv.tree
	sv.mu.Unlock()
	if tree == nil {
		return
	}
	_ = fn(tree)
}

// killOrphans 清理上一次 DontCrack 死亡后遗留的进程树残留(孤儿子进程):
// 先请求优雅退出, 短宽限后强制终止整棵树并释放收容器。
// DontCrack 已死时树内只可能是孤儿, 强杀是唯一可靠手段。
func (sv *Service) killOrphans() {
	sv.mu.Lock()
	tree := sv.tree
	sv.tree = nil
	sv.mu.Unlock()
	sweepTree(tree)
}

// scanLines 逐行转发 DontCrack 输出到日志器。
func (sv *Service) scanLines(r io.ReadCloser, tag string) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		sv.log.Infof("[%s] %s", tag, line)
	}
}

// refreshHeartbeat 拉取一次 DontCrack /heartbeat 并缓存(3s 超时防挂死)。
func (sv *Service) refreshHeartbeat(ctx context.Context) {
	sv.mu.Lock()
	port := sv.cfg.Port
	addr := sv.cfg.ListenAddress
	password := sv.cfg.Password
	running := sv.state == StateRunning
	sv.mu.Unlock()
	if !running || port <= 0 {
		return
	}
	hbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cli := status.NewClient(status.HostPort(addr, port), password)
	hb, err := cli.Heartbeat(hbCtx)
	sv.mu.Lock()
	// 失败时保留旧快照(瞬时网络失败不应让健康状态闪烁/hb 变 nil);
	// healthz=down 由 hbErr 独立反映, 不依赖 hb 被清空。
	if err == nil {
		sv.hb = hb
	}
	sv.hbErr = err
	sv.hbAt = time.Now()
	sv.mu.Unlock()
}

// heartbeatLoop 周期刷新各服务心跳缓存。
func (s *Supervisor) heartbeatLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			svcs := make([]*Service, 0, len(s.svcs))
			for _, sv := range s.svcs {
				svcs = append(svcs, sv)
			}
			s.mu.Unlock()
			for _, sv := range svcs {
				sv.refreshHeartbeat(ctx)
			}
		}
	}
}

func (s *Supervisor) service(name string) *Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.svcs[name]
}

func (sv *Service) setState(st State) {
	sv.mu.Lock()
	sv.state = st
	if st == StateStopped || st == StateFailed {
		sv.stoppedAt = time.Now()
	}
	sv.mu.Unlock()
}

func boolToStr(b bool) string {
	if b {
		return "running"
	}
	return "stopped"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func healthzStr(err error) string {
	if err == nil {
		return "up"
	}
	return "down"
}

// sleepCtx 可被上下文取消的睡眠。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
