// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: 分级线程安全日志。
package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level 日志级别。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel 解析级别字符串。
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info", "":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("非法日志级别 %q", s)
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "?"
}

// Logger 是带级别与可选前缀的线程安全日志器。
type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	level  Level
	prefix string
}

// New 创建日志器。
func New(out io.Writer, level Level) *Logger {
	if out == nil {
		out = os.Stderr
	}
	return &Logger{out: out, level: level}
}

// NewDefault 使用 stderr + info 创建日志器。
func NewDefault() *Logger {
	return New(os.Stderr, LevelInfo)
}

// Child 返回带前缀的派生日志器(用于服务日志, 形如 "[svc:web]")。
func (l *Logger) Child(prefix string) *Logger {
	if prefix != "" && !strings.HasPrefix(prefix, "[") {
		prefix = "[" + prefix + "] "
	} else if prefix != "" {
		prefix = prefix + " "
	}
	return &Logger{out: l.out, level: l.level, prefix: l.prefix + prefix}
}

// SetLevel 动态调整级别。
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Debugf 输出调试日志。
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }

// Infof 输出信息日志。
func (l *Logger) Infof(format string, args ...any) { l.logf(LevelInfo, format, args...) }

// Warnf 输出警告日志。
func (l *Logger) Warnf(format string, args ...any) { l.logf(LevelWarn, format, args...) }

// Errorf 输出错误日志。
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

func (l *Logger) logf(level Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %-5s %s%s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), level.String(), l.prefix, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level { // 级别判断与 SetLevel 同锁, 消除读取竞态
		return
	}
	_, _ = io.WriteString(l.out, line)
}
