//go:build linux

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 孤儿子进程再托付(child subreaper)清扫测试。
//
// 复现的是线上真实形态: DontCrack 自己会给子进程 setpgid(它要按组收敛孙进程),
// 于是子进程不在本管理器的进程组内; DontCrack 被强杀后该子进程本应托付给 init,
// 本管理器的 kill(-pgid) 再也打不到它。开启 subreaper 后它改托付给本进程,
// 清扫才能按 PPid 找到它。
package supervisor

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/config"
	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
)

// spawnGroupedGrandchild 起一个"自带进程组的孙进程", 模拟 DontCrack 的子进程:
// 父脚本用 `set -m` 打开作业控制, 后台任务 thus 得到自己的进程组(等价 setpgid),
// 随后父脚本挂起等待; 杀掉父脚本即留下一个脱离了本进程组的孤儿。
func spawnGroupedGrandchild(t *testing.T) (parent *exec.Cmd, grandPID int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grand.pid")
	script := filepath.Join(dir, "spawner")
	body := "#!/bin/bash\nset -m\nsleep 3600 &\necho $! > \"$1\"\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写模拟 DontCrack 脚本: %v", err)
	}
	cmd := exec.Command(script, pidFile)
	cmd.SysProcAttr = newSysProcAttr() // 与生产一致: DontCrack 本体在独立进程组
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动模拟 DontCrack: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(pidFile)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			return cmd, pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待孙进程 PID 超时")
	return nil, 0
}

// TestReparentedOrphanIsSwept 钉住整条不变量链:
// 孙进程确实脱离了 DontCrack 的进程组(组广播打不到) → 开启 subreaper 后被托付给
// 本进程(PPid 关系重新可观测) → 清扫强杀并回收它(不留僵尸)。
func TestReparentedOrphanIsSwept(t *testing.T) {
	if err := enableOrphanReaping(); err != nil {
		t.Fatalf("开启 child subreaper 失败: %v", err)
	}
	parent, grand := spawnGroupedGrandchild(t)
	t.Cleanup(func() { reapIfStillOurs(grand) })

	// 前提确认: 孙进程真的跑在自己的进程组里(否则本用例没有覆盖到双层进程组)。
	if pgid, err := syscall.Getpgid(grand); err != nil || pgid == parent.Process.Pid {
		t.Fatalf("孙进程应位于自己的进程组: pgid=%d err=%v, DontCrack 组=%d",
			pgid, err, parent.Process.Pid)
	}

	// DontCrack 本体被强杀 → 孙进程成为孤儿。
	if err := syscall.Kill(parent.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("强杀模拟 DontCrack: %v", err)
	}
	_ = parent.Wait()

	// subreaper 生效的标志: 内核把它托付给本进程, 而不是 init。
	deadline := time.Now().Add(5 * time.Second)
	for {
		ppid, ok := ppidOf(grand)
		if ok && ppid == os.Getpid() {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("孤儿未被托付给本进程(subreaper 未生效): ppid=%d ok=%v, 期望 %d",
				ppid, ok, os.Getpid())
		}
		time.Sleep(20 * time.Millisecond)
	}

	swept := sweepReparentedOrphans(nil)
	if !containsPID(swept, grand) {
		t.Fatalf("清扫应命中被托付的孤儿 %d, 实际清理: %v", grand, swept)
	}
	// 已回收(而不只是"信号发得出去"): 僵尸仍在 /proc 且 kill(pid,0) 成功,
	// 只有真正 Wait4 收走后 kill 才会报 ESRCH。
	if err := syscall.Kill(grand, 0); err == nil {
		t.Fatalf("孤儿 %d 未被回收(仍可被寻址, 可能是僵尸)", grand)
	}
}

// TestSweepSkipsTrackedProcess 钉住排除语义: 仍由 cmd.Wait 回收的 DontCrack 本体
// 不得被清扫抢走退出状态(否则 waitExit 永久阻塞, 服务永远停在 running)。
func TestSweepSkipsTrackedProcess(t *testing.T) {
	if err := enableOrphanReaping(); err != nil {
		t.Fatalf("开启 child subreaper 失败: %v", err)
	}
	parent, grand := spawnGroupedGrandchild(t)
	t.Cleanup(func() { reapIfStillOurs(grand) })
	if err := syscall.Kill(parent.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("强杀模拟 DontCrack: %v", err)
	}
	_ = parent.Wait()

	// 把孤儿登记为"仍被跟踪" → 必须跳过, 不得强杀。
	if swept := sweepReparentedOrphans(map[int]bool{grand: true}); containsPID(swept, grand) {
		t.Fatalf("被跟踪的进程不得被清扫: %v", swept)
	}
	if err := syscall.Kill(grand, 0); err != nil {
		t.Fatalf("被跟踪的进程不应被杀掉: %v", err)
	}
}

// reapIfStillOurs 只在本进程仍是该 PID 的父进程时才强杀回收。
// 用途是测试收尾: 用例若已回收该进程, 那个 PID 可能已被系统复用, 盲杀会波及无关进程。
func reapIfStillOurs(pid int) {
	if ppid, ok := ppidOf(pid); !ok || ppid != os.Getpid() {
		return
	}
	killAndReap(pid)
}

func containsPID(pids []int, want int) bool {
	for _, p := range pids {
		if p == want {
			return true
		}
	}
	return false
}

// TestSupervisorSweepRespectsOwnAndGate 覆盖监管器层新增的两道闸:
//   - 补扫前提(subreaper)未立起来时**不扫**: 判据失效却照扫, 可能误杀本进程的
//     其它合法孩子;
//   - 已由本管理器启动(markOwn)的进程即使被内核托付也不扫: 它的退出状态归
//     cmd.Wait, 抢走会让 waitExit 永久阻塞。
func TestSupervisorSweepRespectsOwnAndGate(t *testing.T) {
	parent, grand := spawnGroupedGrandchild(t)
	t.Cleanup(func() { reapIfStillOurs(grand) })
	if err := syscall.Kill(parent.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("强杀模拟 DontCrack: %v", err)
	}
	_ = parent.Wait()

	sup := New(&config.Manager{}, logging.New(io.Discard, logging.LevelError))

	// 闸一: 未开启 subreaper → 不扫, 孤儿必须还活着。
	if swept := sup.sweepOrphans(); len(swept) != 0 {
		t.Fatalf("subreaper 未开启时不得补扫, 实际清理: %v", swept)
	}
	if err := syscall.Kill(grand, 0); err != nil {
		t.Fatalf("闸一失效: 孤儿不应在未开启 subreaper 时被杀: %v", err)
	}

	if err := enableOrphanReaping(); err != nil {
		t.Fatalf("开启 child subreaper 失败: %v", err)
	}
	sup.reapOrphans.Store(true)

	// 闸二: 登记为本管理器自有 → 仍不扫。
	sup.markOwn(grand)
	if swept := sup.sweepOrphans(); containsPID(swept, grand) {
		t.Fatalf("自有进程不得被补扫: %v", swept)
	}
	if err := syscall.Kill(grand, 0); err != nil {
		t.Fatalf("闸二失效: 自有进程不应被杀: %v", err)
	}

	// 撤销登记后它才成为合法清扫目标(证明前两次是"闸"生效, 不是用例本身没覆盖到)。
	sup.ownMu.Lock()
	delete(sup.own, grand)
	sup.ownMu.Unlock()
	if swept := sup.sweepOrphans(); !containsPID(swept, grand) {
		t.Fatalf("未登记时应清扫该孤儿 %d, 实际清理: %v", grand, swept)
	}
}
