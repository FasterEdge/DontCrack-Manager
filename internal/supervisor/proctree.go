// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 进程树收容器抽象(平台无关接口)。
package supervisor

import (
	"errors"
	"os"
	"time"
)

// ErrAlreadyContained 是收容器接管失败的一个**可识别子类**: 目标进程已被另一个
// 进程树收容器接管, 且该收容器不允许嵌套。
//
// 上层据此把"接管失败"与"真的没接管上"分开处置: 前者说明强杀兜底已由现有收容器
// 承担, 后者才是 README 第五节所述的能力降级。历史暗病: windows 侧曾把这一类
// 失败 `return nil` 静默吞掉, 上层拿不到任何错误, 降级告警永不触发。
//
// 只有 windows 会产生它(AssignProcessToJobObject 的 ERROR_ACCESS_DENIED); unix 的
// 进程组可随时重复加入, attach 恒成功, 因此该 sentinel 在 unix 侧只是被定义而不会
// 被返回 —— 放在这个平台无关文件里, 是为了让上层的判定与测试在两个平台同构。
var ErrAlreadyContained = errors.New("进程已被其它进程树收容器接管(禁止嵌套)")

// procTree 抽象"进程树收容器": 把 DontCrack 及其派生子进程纳入同一可寻址范围,
// 使根管理器在强杀兜底时"不留孤儿"。
//
// 平台实现(见 process_unix.go / process_windows.go):
//   - unix: 独立进程组(Setpgid), 强杀按 -pgid 广播;
//   - windows: 每实例一个 Job Object(KILL_ON_JOB_CLOSE), 强杀走 TerminateJobObject。
//
// 方法语义与平台能力的对应关系:
//   - graceful: "请优雅退出"的等价物(unix SIGTERM 投给 DontCrack 本体, 由它先停子进程;
//     windows 投 CTRL_BREAK_EVENT 给其控制台进程组)。
//   - force: 无条件强制终止**整棵树**(unix 进程组 SIGKILL / windows TerminateJobObject)。
//   - close: 释放收容资源; windows 关闭 Job 句柄会连带终止仍在其中的进程(unix 为空操作)。
type procTree interface {
	// attach 在 DontCrack 启动成功后接管其进程树。
	attach(p *os.Process) error
	// graceful 请求优雅停止。
	graceful() error
	// force 强制终止整棵树(含派生子进程)。
	force() error
	// close 释放收容资源。
	close()
}

// sweepTree 清理树内残留(孤儿清理): 先请求优雅退出, 短暂宽限后强制终止整棵树,
// 再释放收容资源。DontCrack 本体已死时优雅请求会失败(ESRCH 等), 忽略即可 ——
// 组内只可能是孤儿, 强杀是唯一可靠手段。
func sweepTree(t procTree) {
	if t == nil {
		return
	}
	_ = t.graceful()
	sleepOrphanGrace()
	_ = t.force()
	t.close()
}

// orphanGrace 是孤儿清理的优雅宽限。killOrphans 只在 DontCrack 本体已死的场景
// 调用, 组内只可能是孤儿, 不涉及业务优雅停机, 因此宽限极短即可。
const orphanGrace = 300 * time.Millisecond

func sleepOrphanGrace() { time.Sleep(orphanGrace) }
