//go:build unix

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: unix 进程树收容 —— 独立进程组(Setpgid)。
package supervisor

import (
	"os"
	"syscall"
)

// pgTree 是 unix 侧 procTree 实现: Setpgid 下进程组 ID == 进程 PID,
// 因此收容器只需记住 PID 即可寻址整棵树(按 -pgid 广播信号)。
type pgTree struct {
	pgid int
}

var _ procTree = (*pgTree)(nil)

// newProcTree 创建平台进程树收容器。
func newProcTree() procTree { return &pgTree{} }

// newSysProcAttr 让 DontCrack 以独立进程组启动(Setpgid): 若 DontCrack 被强杀/
// 崩溃, 其子进程会成为孤儿, 重启前按进程组清理, 保证"不留孤儿"(fail-closed)。
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// attach 接管进程树: Setpgid 下 pgid == pid, 无需额外系统调用。
func (t *pgTree) attach(p *os.Process) error {
	t.pgid = p.Pid
	return nil
}

// graceful 投递 SIGTERM 给 DontCrack 本体(由其先停子进程)。
func (t *pgTree) graceful() error {
	if t.pgid <= 0 {
		return nil
	}
	return syscall.Kill(-t.pgid, syscall.SIGTERM)
}

// force 强杀整棵树: kill -9 -pgid(连带 DontCrack 遗留的子进程)。
func (t *pgTree) force() error {
	if t.pgid <= 0 {
		return nil
	}
	return syscall.Kill(-t.pgid, syscall.SIGKILL)
}

// close 无需释放资源(进程组随进程消亡自然消失)。
func (t *pgTree) close() {}
