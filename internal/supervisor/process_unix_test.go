//go:build unix

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: unix 进程组(Setpgid)进程树收容测试。
//
// 只验证"真的把进程纳入独立进程组并按组强杀"这一不变量, 不 mock 任何系统调用:
// 起一个常驻子进程, attach 进进程组, 再 force 后断言进程消失。
package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// spawnSleeper 启动一个常驻 bash 子进程(用于验证进程组强杀)。
func spawnSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sleeper")
	script := "#!/bin/bash\nwhile :; do sleep 3600 & wait; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("写常驻脚本: %v", err)
	}
	cmd := exec.Command(path)
	cmd.SysProcAttr = newSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动常驻子进程: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// waitExited 在 timeout 内等待进程真正退出(即完成 wait 回收), 返回是否已退出。
//
// 为什么必须回收而不是只看信号是否发得出去: 进程被 SIGKILL 后若没人 wait,
// 它会变成僵尸并**继续留在原进程组里**, 于是 kill(-pgid, 0) 照样成功、
// Signal(0) 照样成功 —— 用它们判断"进程组已消失"会永远等不到。CI 上这两个
// 用例各卡满 5s 超时, 根因就是这个: 断言用的是"信号可送达", 而不是"进程已回收"。
func waitExited(cmd *exec.Cmd, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestProcessGroupContainsAndKillsTree: attach 后 force 必须真的杀掉进程。
func TestProcessGroupContainsAndKillsTree(t *testing.T) {
	cmd := spawnSleeper(t)
	tree := newProcTree()
	if err := tree.attach(cmd.Process); err != nil {
		t.Fatalf("attach 失败: %v", err)
	}

	// attach 语义: Setpgid 下 pgid == pid, 进程组必须真实存在。
	pg := tree.(*pgTree)
	if pg.pgid != cmd.Process.Pid {
		t.Fatalf("pgid = %d, 期望等于 pid %d", pg.pgid, cmd.Process.Pid)
	}
	if err := syscall.Kill(-pg.pgid, 0); err != nil {
		t.Fatalf("进程组不存在: kill(-pgid, 0) = %v", err)
	}

	if err := tree.force(); err != nil {
		t.Fatalf("force 失败: %v", err)
	}
	if !waitExited(cmd, 5*time.Second) {
		t.Fatalf("force 后进程组仍存在(进程组强杀未生效)")
	}
}

// TestProcessGroupGracefulSendsSIGTERM: graceful 必须真的投递 SIGTERM 给进程组,
// 而不是空操作 —— 这是"先礼后兵"中"礼"的部分。
func TestProcessGroupGracefulSendsSIGTERM(t *testing.T) {
	cmd := spawnSleeper(t)
	tree := newProcTree()
	if err := tree.attach(cmd.Process); err != nil {
		t.Fatalf("attach 失败: %v", err)
	}
	if err := tree.graceful(); err != nil {
		t.Fatalf("graceful 失败: %v", err)
	}
	// bash 的 sleep 被 SIGTERM 打断, 整棵树应退出。
	if !waitExited(cmd, 5*time.Second) {
		t.Fatal("graceful(SIGTERM) 后进程仍未退出")
	}
}

// TestSweepTreeClearsResidual: sweepTree 在进程本体已死时也要能清理树内残留,
// 且不得 panic(树为空/进程组不存在都是合法输入)。
func TestSweepTreeClearsResidual(t *testing.T) {
	cmd := spawnSleeper(t)
	tree := newProcTree()
	if err := tree.attach(cmd.Process); err != nil {
		t.Fatalf("attach 失败: %v", err)
	}
	// 先杀掉本体, 留下树内成员(模拟 DontCrack 崩溃后的孤儿子进程)。
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	sweepTree(tree) // 必须安全返回

	// nil 树也必须安全(收容器 attach 失败的降级路径)。
	sweepTree(nil)
}
