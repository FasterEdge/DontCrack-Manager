// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 收容器接管失败 → 上层告警链路测试。
//
// README 第五节承诺"收容器接管失败不会阻断启动, 会写告警并把强杀能力降级为仅终止
// DontCrack 本体"。此测试钉住整条链路: attach 返回可识别错误(ErrAlreadyContained)
// → spawn 必须写出**带 WARN 级别**的降级告警(而不是静默吞掉, 那是历史暗病),
// 且服务必须照常启动。
package supervisor

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/config"
	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
)

// syncBuf 是并发安全的缓冲区: DontCrack 输出由 scanLines 从多个 goroutine 写入。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// failingTree 是必然接管失败的进程树收容器(模拟 windows 侧 ERROR_ACCESS_DENIED
// 与任何其它 attach 失败)。其余方法为空操作 —— 正是降级后的真实形态。
type failingTree struct{}

func (failingTree) attach(*os.Process) error { return ErrAlreadyContained }
func (failingTree) graceful() error          { return nil }
func (failingTree) force() error             { return nil }
func (failingTree) close()                   {}

// TestSpawnWarnsOnAttachFailure 覆盖 README 第五节承诺的降级告警链路。
func TestSpawnWarnsOnAttachFailure(t *testing.T) {
	// 假 DontCrack: unix 用忽略全部 flag 的常驻脚本; windows 无 POSIX shell,
	// 退用测试二进制自身(启动成功即可, 不要求常驻 —— 本测试只断言 spawn 的告警与状态)。
	var binary string
	stayResident := false
	if runtime.GOOS == "windows" {
		binary = os.Args[0]
	} else {
		binary = writeStub(t)
		stayResident = true
	}

	var buf syncBuf
	log := logging.New(&buf, logging.LevelDebug)
	svcCfg := &config.Service{
		Name:          "web",
		Path:          "/opt/app/web",
		ListenAddress: "127.0.0.1",
		Port:          0, // 0 端口: 跳过心跳刷新, 不发起真实 HTTP 探测
	}
	svc := &Service{
		cfg: svcCfg,
		sup: &Supervisor{cfg: &config.Manager{DontCrackBinary: binary}, log: log},
		log: log.Child("[svc:web]"),
	}
	svc.desired.Store(true)

	// 走真实 spawn(参数构造、告警分支、状态登记全为生产代码), 只把收容器换成
	// 必然失败的那个 —— 这是唯一注入点。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.spawnWithTree(ctx, failingTree{}); err != nil {
		t.Fatalf("接管失败不得阻断启动: %v", err)
	}
	t.Cleanup(func() {
		svc.mu.Lock()
		cmd := svc.cmd
		svc.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	// 降级 ≠ 不启动: 进程必须真的起来了。
	svc.mu.Lock()
	cmd := svc.cmd
	state := svc.state
	tree := svc.tree
	svc.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("接管失败后 DontCrack 进程应已启动(降级不阻断启动)")
	}
	if state != StateRunning {
		t.Fatalf("state = %q, 期望 running", state)
	}
	if tree == nil {
		t.Fatal("降级后仍应登记收容器(其操作为空操作, 供 treeSignal/killOrphans 幂等调用)")
	}

	// 等 scanLines 落日志后读快照。
	var out string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out = buf.String()
		if strings.Contains(out, "接管失败") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "接管失败") {
		t.Fatalf("attach 失败必须写出告警(历史暗病: 静默吞掉), 实际日志:\n%s", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Fatalf("接管失败必须以 WARN 级别写出, 实际日志:\n%s", out)
	}
	if !strings.Contains(out, "降级") {
		t.Fatalf("告警必须说明强杀兜底能力降级, 实际日志:\n%s", out)
	}
	if !strings.Contains(out, "已被上层收容器接管") {
		t.Fatalf("告警应带上可识别的失败原因, 实际日志:\n%s", out)
	}

	// 降级树上后续操作必须安全(StopAll/killOrphans 会继续调用它)。
	svc.treeSignal(func(tr procTree) error { return tr.graceful() })
	svc.treeSignal(func(tr procTree) error { return tr.force() })
	svc.killOrphans()

	if stayResident {
		// unix 常驻存根: 降级不影响进程本体存活(强杀兜底降级为单进程终止而已)。
		if cmd.ProcessState != nil {
			t.Fatal("降级不应导致 DontCrack 本体退出")
		}
	}
}
