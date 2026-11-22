//go:build windows

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: Windows 进程树收容 —— Job Object + KILL_ON_JOB_CLOSE。
//
// 背景: Windows 没有 POSIX 进程组, 也没有可投递给任意进程的 SIGTERM。此前
// 退化为 `taskkill /T /F`(只杀树, 无优雅语义) —— 硬杀 DontCrack, 子进程失去
// 清理机会, 与 unix 侧"先 SIGTERM 宽限, 再进程组 SIGKILL"的语义不对等。
//
// 现在每个 DontCrack 实例配一个 Job Object(KILL_ON_JOB_CLOSE)作为收容器:
//   - 优雅: 该实例以 CREATE_NEW_PROCESS_GROUP 启动(独立控制台进程组),
//     graceful 走 GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT) —— DontCrack 侧
//     signal.Notify(os.Interrupt) 即收到等价于 SIGTERM 的中断, 由其自行先停子进程;
//   - 强杀: TerminateJobObject 一次性终止**整棵树**(含其派生的孤儿子进程),
//     这是 unix "kill -9 -pgid" 的等价物;
//   - close: 关闭 Job 句柄时 KILL_ON_JOB_CLOSE 兜底清掉残留成员。
//
// 能力边界(README 同步说明):
//   - CTRL_BREAK_EVENT 只能送达同控制台进程组的进程; 若 DontCrack 脱离控制台
//     (服务化/无控制台)启动, graceful 可能无效, 此时仍会按宽限期后强杀整棵树,
//     安全性由 Job Object 保证(不留孤儿), 优雅性则降级。
//   - 树内成员若自带 BREAKAWAY_OK 逃逸出 Job, 则不在强杀范围内 —— 这是 Job
//     Object 的设计边界, 文档如实说明。
package supervisor

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winJobTree 是 Windows 侧 procTree 实现。
type winJobTree struct {
	job  windows.Handle // Job Object 句柄
	pid  uint32         // DontCrack 进程 PID(控制台进程组 ID, 见 newSysProcAttr)
	dead bool           // 已强制终止/关闭, 后续操作幂等跳过
}

var _ procTree = (*winJobTree)(nil)

// newProcTree 创建平台进程树收容器(windows: Job Object)。
func newProcTree() procTree { return &winJobTree{} }

// newSysProcAttr 让 DontCrack 以独立控制台进程组启动: 这样它自己的控制台进程组
// ID 等于自身 PID, graceful 的 CTRL_BREAK_EVENT 可精确送达该实例(不会波及其它
// 服务), 且与 unix 的 Setpgid 语义对等(每实例一棵树, 互不干扰)。
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// attach 创建 Job Object 并把 DontCrack 加入其中。
// 必须在 cmd.Start() 成功之后调用(进程已存在才能被 Assign)。
func (t *winJobTree) attach(p *os.Process) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("创建 Job Object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// KILL_ON_JOB_CLOSE: 句柄关闭(含本进程异常退出未 close)时, 树内残留成员
	// 被一并终止 —— 这是"管理器挂了也不留孤儿"的兜底保证。
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("设置 Job Object 限制(KILL_ON_JOB_CLOSE): %w", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("打开 DontCrack 进程 %d: %w", p.Pid, err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		// 已在其它 Job 中(ERROR_ACCESS_DENIED): 例如被父进程 Job 继承收容。
		// 这表示"管理器自身已被某个 Job 收容且不允许嵌套", 交给现有收容者。
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil
		}
		return fmt.Errorf("加入 Job Object: %w", err)
	}
	t.job = job
	t.pid = uint32(p.Pid)
	return nil
}

// graceful 请求优雅停止: CTRL_BREAK_EVENT 送至该实例的控制台进程组。
// 与 DontCrack-Windows 的 signal.Notify(os.Interrupt) 对齐(os.Interrupt 在
// Windows 上即 CTRL_BREAK_EVENT), 由其自行先停子进程。
func (t *winJobTree) graceful() error {
	if t.dead || t.job == 0 {
		return nil
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, t.pid); err != nil {
		return err
	}
	return nil
}

// force 强制终止整棵树(TerminateJobObject), 是 unix 侧 "kill -9 -pgid" 的等价物。
func (t *winJobTree) force() error {
	if t.dead || t.job == 0 {
		return nil
	}
	t.dead = true // 幂等: 已终止过就不再重复(Job 终止后句柄仍有效, 重复调用无益)
	err := windows.TerminateJobObject(t.job, 1)
	// 终止后立即关闭句柄(KILL_ON_JOB_CLOSE 会带走任何漏网成员)。
	windows.CloseHandle(t.job)
	t.job = 0
	return err
}

// close 释放收容资源; 关闭 Job 句柄时 KILL_ON_JOB_CLOSE 会清掉残留成员。
func (t *winJobTree) close() {
	if t.job == 0 {
		return
	}
	windows.CloseHandle(t.job)
	t.job = 0
}
