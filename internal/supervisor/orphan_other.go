//go:build !linux

// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 非 Linux 平台的孤儿清扫退化实现。
package supervisor

// enableOrphanReaping 在非 Linux 平台是空操作。
//
// 能力边界(README 第五节同步说明):
//   - Windows 不需要它: 收容器是 Job Object, 进程加入 Job 后其派生子进程自动继承
//     成员身份, DontCrack 惨死后 TerminateJobObject 照样能收掉孙子进程;
//   - 其它 unix(如 macOS/BSD)没有 child subreaper 的等价物(没有 prctl), 因此
//     "DontCrack 惨死后其子进程被它自己的进程组带离本管理器视野"这一情形无法按
//     PPid 补扫 —— 只能依赖进程组广播, 该边界如实记为能力降级。
func enableOrphanReaping() error { return nil }

// sweepReparentedOrphans 在非 Linux 平台无依据可用, 恒为空。
func sweepReparentedOrphans(map[int]bool) []int { return nil }
