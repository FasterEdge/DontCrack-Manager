//go:build linux

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: Linux 侧的孤儿再托付(child subreaper)清扫。
package supervisor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// enableOrphanReaping 把本进程设为 child subreaper(Linux PR_SET_CHILD_SUBREAPER)。
//
// 为什么需要它: 本管理器的进程树收容器是"独立进程组", 但 DontCrack 自己也会把
// 子进程 setpgid 进**另一个**进程组(它要按组收敛孙进程)。两层进程组一嵌套,
// 组广播就打不到里面那层 —— DontCrack 被强杀后其子进程既不在本组的寻址范围,
// 又被内核重新托付给 init, 管理器的 `kill(-pgid)` 与 /proc 里的 PPid 都再也
// 找不到它, 于是"不留孤儿子进程"的承诺落空(线上表现为端口被残留进程占住,
// 重启后的服务永远绑不上)。
//
// subreaper 把这种"本该托付给 init"的孤儿改为托付给本进程, 于是 PPid 关系在
// 管理器里重新可观测, 清扫才有依据。这是 Linux 专有机制(见 orphan_other.go)。
func enableOrphanReaping() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_CHILD_SUBREAPER): %w", err)
	}
	return nil
}

// reparentedOrphans 返回被内核托付给本进程(PPid == 自身)的进程 PID。
func reparentedOrphans() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		if ppid, ok := ppidOf(pid); ok && ppid == self {
			out = append(out, pid)
		}
	}
	return out
}

// ppidOf 读 /proc/<pid>/stat 的 PPid 字段。
// 解析从最后一个 ')' 之后开始: comm 字段允许包含空格与括号, 不能按空白直接切。
func ppidOf(pid int) (int, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, false
	}
	// ')' 之后依次是 state 与 ppid。
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// sweepReparentedOrphans 强杀并回收被托付给本进程的孤儿, 返回实际清理的 PID。
//
// tracked 是"仍然由本进程用 cmd.Wait 回收"的 PID 集合(各服务的 DontCrack 本体),
// 必须排除: 对它们调 Wait4 会抢走 cmd.Wait 的退出状态。
func sweepReparentedOrphans(tracked map[int]bool) []int {
	orphans := reparentedOrphans()
	if len(orphans) == 0 {
		return nil
	}
	var swept []int
	for _, pid := range orphans {
		if tracked[pid] {
			continue
		}
		killAndReap(pid)
		swept = append(swept, pid)
	}
	return swept
}

// killAndReap 强杀单个孤儿(连同它自己的进程组)并回收, 不留僵尸。
//
// 进程组只在"它自己就是组长"时才广播: DontCrack 给子进程 setpgid 后组长就是
// 子进程自己, 组广播能顺带收掉它的后代; 若不是组长, 对 -pid 广播可能命中一个
// 毫不相干的同名进程组, 因此宁可退化为只杀这一个进程。
func killAndReap(pid int) {
	if pgid, err := unix.Getpgid(pid); err == nil && pgid == pid {
		_ = unix.Kill(-pid, unix.SIGKILL)
	}
	_ = unix.Kill(pid, unix.SIGKILL)

	// 必须回收: 不收就是僵尸, 既会被 e2e 的残留检查判为残留, 也白占 PID。
	// 超时后不再等待 —— 说明它已被其它父进程收走(ECHILD), 结果为 ESRCH 同样算成功。
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		if err != nil || wpid == pid {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
