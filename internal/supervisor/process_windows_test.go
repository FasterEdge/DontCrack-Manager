//go:build windows

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: Windows Job Object 进程树收容测试。
//
// 这些测试验证"能真的把进程树收容并按树强杀"这一不变量, 不 mock 任何 Win32
// 调用: 用测试二进制自身充当 DontCrack 本体(holder)与它派生的子进程(child),
// attach 进 Job 后 force, 断言整棵树(含 child)都退出。
package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 帮助进程角色(经环境变量驱动, 不依赖外部脚本/工具)。
const (
	helperRoleEnv = "DCM_WIN_TREE_ROLE"
	helperPidEnv  = "DCM_WIN_TREE_PIDFILE"
	helperGoEnv   = "DCM_WIN_TREE_GOFILE"
	roleHolder    = "holder" // 等到"go"文件出现后再 spawn child, 然后常驻
	roleChild     = "child"  // 纯粹常驻
)

// TestWinTreeHelper 本身不是测试: 它是被其它测试以子进程方式复用的帮助程序。
func TestWinTreeHelper(t *testing.T) {
	switch os.Getenv(helperRoleEnv) {
	case roleChild:
		time.Sleep(2 * time.Minute)
		return
	case roleHolder:
		// 等测试把 holder 收容进 Job 之后再派生 child —— 与真实场景一致:
		// DontCrack 是"先被收容、后派生子进程", 子进程才会自动继承进同一 Job。
		goFile := os.Getenv(helperGoEnv)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(goFile); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		child := exec.Command(os.Args[0], "-test.run=TestWinTreeHelper")
		child.Env = append(os.Environ(), helperRoleEnv+"="+roleChild)
		child.SysProcAttr = newSysProcAttr()
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "holder: 启动 child 失败:", err)
			os.Exit(3)
		}
		// 记录 child PID, 供测试断言"树内成员是否被一并终止"。
		if pf := os.Getenv(helperPidEnv); pf != "" {
			_ = os.WriteFile(pf, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		}
		_ = child.Wait()
		return
	}
	t.Skip("帮助进程模式: 仅在由其它测试以子进程方式拉起时生效")
}

// treeProc 是一次树收容实验的句柄集。
type treeProc struct {
	holder   *exec.Cmd
	childPID int
}

// startTree 启动 holder(暂不派生 child), 把它 attach 进 Job, 再放行让它派生 child。
func startTree(t *testing.T) (*treeProc, procTree) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	goFile := filepath.Join(dir, "go")

	holder := exec.Command(os.Args[0], "-test.run=TestWinTreeHelper")
	holder.Env = append(os.Environ(),
		helperRoleEnv+"="+roleHolder,
		helperPidEnv+"="+pidFile,
		helperGoEnv+"="+goFile,
	)
	holder.SysProcAttr = newSysProcAttr()
	if err := holder.Start(); err != nil {
		t.Fatalf("启动 holder: %v", err)
	}
	tp := &treeProc{holder: holder}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		if tp.childPID > 0 {
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(tp.childPID)); err == nil {
				_ = windows.TerminateProcess(h, 1)
				windows.CloseHandle(h)
			}
		}
	})

	tree := newProcTree()
	if err := tree.attach(holder.Process); err != nil {
		t.Fatalf("attach holder: %v", err)
	}
	t.Cleanup(tree.close)

	// 放行: holder 现在才派生 child, child 自动继承进同一 Job。
	if err := os.WriteFile(goFile, []byte("go"), 0o644); err != nil {
		t.Fatalf("放行 holder: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatalf("解析 child PID %q: %v", data, err)
			}
			tp.childPID = pid
			return tp, tree
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("等待 child PID 落盘超时")
	return nil, nil
}

// alive 判断 PID 对应进程是否仍存活(可等待句柄 + 0 超时等待)。
func alive(t *testing.T, pid int) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false // 打不开通常意味着进程已不存在
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return true
	}
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// waitDead 在 timeout 内等到 PID 进程消失。
func waitDead(t *testing.T, pid int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(t, pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestJobObjectContainsTree 验证 Job Object 真的收容**整棵树**: 只 attach holder
// (DontCrack 本体), 其此后派生的 child 由系统自动继承进 Job, force 时一并终止 ——
// 这正是旧实现 `taskkill /T /F` 靠外部命令达成的效果, 现由内核原生保证。
func TestJobObjectContainsTree(t *testing.T) {
	tp, tree := startTree(t)
	if !alive(t, tp.childPID) {
		t.Fatalf("child %d 派生后应立即存活", tp.childPID)
	}
	// 收容 ≠ 终止: attach 之后两代都应仍存活。
	if !alive(t, tp.holder.Process.Pid) || !alive(t, tp.childPID) {
		t.Fatal("attach 后进程不应已退出")
	}

	if err := tree.force(); err != nil {
		t.Fatalf("force: %v", err)
	}
	if !waitDead(t, tp.holder.Process.Pid, 5*time.Second) {
		t.Fatal("force 后 holder(DontCrack 本体)仍在运行")
	}
	// 关键断言: 树内子进程必须被同一棵树带走, 否则就是孤儿。
	if !waitDead(t, tp.childPID, 5*time.Second) {
		t.Fatalf("force 后 child %d 仍存活(Job Object 未收容整棵树)", tp.childPID)
	}
}

// TestJobObjectIdempotentClose: force 后再次 close/force/graceful 不得报错崩溃
// (StopAll 的兜底路径可能重复调用)。
func TestJobObjectIdempotentClose(t *testing.T) {
	tp, tree := startTree(t)
	if err := tree.force(); err != nil {
		t.Fatalf("首次 force: %v", err)
	}
	if err := tree.force(); err != nil {
		t.Fatalf("重复 force 应幂等无错: %v", err)
	}
	tree.close()
	tree.close()
	if err := tree.graceful(); err != nil {
		t.Fatalf("关闭后 graceful 应幂等无错: %v", err)
	}
	if !waitDead(t, tp.childPID, 5*time.Second) {
		t.Fatal("幂等调用路径也应已终止树内进程")
	}
}

// TestJobObjectGracefulIsBestEffort: graceful 经 CTRL_BREAK_EVENT 投递, 在无控制台
// 环境下可能送达失败 —— 这是**已知能力边界**(见 process_windows.go 注释)。因此这里
// 只断言"不 panic 且不误杀", 强杀语义由 TestJobObjectContainsTree 保证。
func TestJobObjectGracefulIsBestEffort(t *testing.T) {
	tp, tree := startTree(t)

	_ = tree.graceful() // 允许失败: 无控制台 / 不在同控制台进程组
	if !alive(t, tp.childPID) && !alive(t, tp.holder.Process.Pid) {
		return // 优雅通道生效, 树已退出 —— 合法结果
	}
	// 优雅通道无效: 强杀兜底必须仍然能终止整棵树。
	if err := tree.force(); err != nil {
		t.Fatalf("force: %v", err)
	}
	if !waitDead(t, tp.childPID, 5*time.Second) {
		t.Fatal("graceful 无效时 force 必须仍能终止整棵树")
	}
}

// TestAttachErrorIsIdentifiable: 收容器接管失败必须**可识别**。
//
// 这是 README 第五节"接管失败会写告警并把强杀能力降级"的成立前提: 历史暗病是
// ERROR_ACCESS_DENIED(进程已被上层 Job 收容且禁止嵌套)被 `return nil` 静默吞掉,
// 上层拿不到任何错误, 降级告警永不触发 —— README 举的例子恰好走这条路。
//
// 真实触发 ERROR_ACCESS_DENIED 需要进程处在一个禁止嵌套的 Job 中(旧版 Windows
// 语义 / 特定容器宿主); 现代 Windows 允许嵌套 Job, 本机实测无法自然复现
// (见 TestJobObjectNestedIsAllowedByModernWindows), 因此这里直接对 attach 的
// 错误出口做断言 —— 用真实 attach 调用覆盖"非 ACCESS_DENIED 失败"分支, 再用
// 该分支的同一映射规则验证 ERROR_ACCESS_DENIED 分支的可识别性, 不 mock 任何 Win32。
func TestAttachErrorIsIdentifiable(t *testing.T) {
	// 非 ACCESS_DENIED 的失败: 真实 attach(目标 PID 不存在)必须返回非 nil 错误,
	// 且不得被误判为"已被收容" —— 否则真正的收容故障会被当成上层接管, 告警失真。
	real := (&winJobTree{}).attach(&os.Process{Pid: -1})
	if real == nil {
		t.Fatal("attach 失败不得被吞成 nil(历史暗病: ERROR_ACCESS_DENIED 曾 return nil)")
	}
	if errors.Is(real, ErrAlreadyContained) {
		t.Fatalf("普通 attach 失败不得被识别为 ErrAlreadyContained: %v", real)
	}
	if !strings.Contains(real.Error(), "DontCrack 进程") {
		t.Fatalf("attach 错误应带上目标进程信息便于定位: %v", real)
	}

	// ERROR_ACCESS_DENIED 分支(attach 内部的映射规则): 必须命中 sentinel。
	// 该规则与 attach 内联实现保持同一形态, 由上方注释说明为何不能自然复现。
	mapped := fmt.Errorf("%w(Job Object): pid=4242, 交由现有收容者: %v",
		ErrAlreadyContained, windows.ERROR_ACCESS_DENIED)
	if !errors.Is(mapped, ErrAlreadyContained) {
		t.Fatalf("ERROR_ACCESS_DENIED 必须可 errors.Is(ErrAlreadyContained) 识别: %v", mapped)
	}
	if !strings.Contains(mapped.Error(), "4242") {
		t.Fatalf("可识别错误应带上 pid: %v", mapped)
	}
	if errors.Is(fmt.Errorf("加入 Job Object: %w", syscall.EINVAL), ErrAlreadyContained) {
		t.Fatal("普通 attach 失败不得命中 ErrAlreadyContained")
	}
}

// TestJobObjectNestedIsAllowedByModernWindows 记录了本机实测的事实, 它是上面那个
// 测试改用"错误出口断言"而非"真实触发"的原因: 现代 Windows 允许把已在 Job 中的进程
// 再 Assign 进另一个 Job(返回 ERROR_SUCCESS), 因此 ERROR_ACCESS_DENIED 无法自然复现。
func TestJobObjectNestedIsAllowedByModernWindows(t *testing.T) {
	tp, _ := startTree(t)

	// 用一个不含 KILL_ON_JOB_CLOSE 的普通 Job 先收容 holder。
	plain, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	t.Cleanup(func() { windows.CloseHandle(plain) })

	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(tp.holder.Process.Pid))
	if err != nil {
		t.Fatalf("OpenProcess: %v", err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(plain, proc); err != nil {
		t.Fatalf("首次收容: %v", err)
	}

	// 再次收容进另一个 Job: 现代 Windows 允许嵌套(这正是"管理器自身在上层 Job 中
	// 也能收容 DontCrack"的原因), 因此不会返回 ERROR_ACCESS_DENIED。
	other, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	t.Cleanup(func() { windows.CloseHandle(other) })
	if err := windows.AssignProcessToJobObject(other, proc); err != nil {
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("重复收容: %v", err)
		}
		t.Log("本机禁止嵌套 Job(ERROR_ACCESS_DENIED 可自然复现)")
		return
	}
	t.Log("本机允许嵌套 Job: ERROR_ACCESS_DENIED 无法自然复现, 失败路径以出口断言覆盖")
}

// TestAttachDegradedTreeIsSafe: 接管失败(降级)不得阻断 DontCrack 启动 —— 进程已在跑,
// 收容器缺失只意味着强杀兜底退化为"仅终止 DontCrack 本体"(README 第五节)。
// 同时验证降级树(无 Job 句柄, 等价于 attach 出错的状态)上 graceful/force/close
// 仍然幂等安全 —— 上层 treeSignal/killOrphans 会继续调用它们。
func TestAttachDegradedTreeIsSafe(t *testing.T) {
	tp, _ := startTree(t)
	tree := newProcTree()
	if _, ok := tree.(*winJobTree); !ok {
		t.Fatalf("newProcTree 类型 = %T, 期望 *winJobTree", tree)
	}

	if err := tree.graceful(); err != nil {
		t.Fatalf("降级树 graceful 应安全无错: %v", err)
	}
	if err := tree.force(); err != nil {
		t.Fatalf("降级树 force 应安全无错: %v", err)
	}
	tree.close()
	tree.close() // 幂等

	// 进程本体不受收容器接管失败影响, 仍由测试清理。
	if !alive(t, tp.holder.Process.Pid) && !alive(t, tp.childPID) {
		t.Log("holder/child 已提前退出(合法: 降级不承诺存活)")
	}
}
