// FasterEdge 开源项目 - Github: https://github.com/FasterEdge - Gitee: https://gitee.com/FasterEdge
// DontCrack-Manager: DontCrack 多进程根管理器。
//
// 单体的 DontCrack 一次只能管理一个进程; 本工具同时拉起并监管多个 DontCrack
// 实例, 作为无进程管理器环境(如嵌入式 Linux / 精简容器)的系统多进程根管理器。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FasterEdge/DontCrack-Manager/internal/config"
	"github.com/FasterEdge/DontCrack-Manager/internal/logging"
	"github.com/FasterEdge/DontCrack-Manager/internal/status"
	"github.com/FasterEdge/DontCrack-Manager/internal/supervisor"
)

// version 版本号(与 DontCrack 家族日期版本约定一致)。
const version = "1.0.20260901"

func main() {
	configPath := flag.String("config", "", "配置文件路径(必填)")
	checkOnly := flag.Bool("check", false, "仅校验配置, 校验通过后退出(exit 0)")
	showVersion := flag.Bool("version", false, "显示版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("DontCrack-Manager " + version)
		return
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "用法: dontcrack-manager -config <manager.yaml> [-check]")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}

	if *checkOnly {
		fmt.Printf("配置校验通过: %d 个服务(启用 %d), 聚合状态监听 %s\n",
			len(cfg.Services), enabledCount(cfg), cfg.Listen)
		for i := range cfg.Services {
			svc := &cfg.Services[i]
			fmt.Printf("  - %s: %s (port %d, restart=%s, deps=%v)\n",
				svc.Name, svc.Path, svc.Port, svc.Restart, svc.DependsOn)
		}
		return
	}

	level, _ := logging.ParseLevel(cfg.LogLevel)
	log := logging.New(os.Stderr, level)
	log.Infof("DontCrack-Manager %s 启动中", version)
	log.Infof("DontCrack 可执行文件: %s", cfg.DontCrackBinary)
	log.Infof("启用服务: %d / %d", enabledCount(cfg), len(cfg.Services))

	sup := supervisor.New(cfg, log)

	// 根上下文: 收到 SIGINT/SIGTERM 触发优雅停机。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup.Run(ctx)

	// 聚合状态 HTTP(可选)。
	var srv *status.Server
	if cfg.Listen != "" {
		srv = status.NewServer(cfg.Listen, cfg.Password, sup, func() error {
			log.Infof("收到 /shutdown 请求, 开始优雅停机")
			stop()
			return nil
		}, log)
		go func() {
			if err := srv.Serve(); err != nil {
				log.Errorf("聚合状态服务错误: %v", err)
			}
		}()
	}

	<-ctx.Done()
	log.Infof("收到停止信号, 开始优雅停机(宽限 %v)...", cfg.ShutdownGrace.Duration)
	sup.StopAll(cfg.ShutdownGrace.Duration)
	if srv != nil {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}
	log.Infof("已优雅关闭")
}

func enabledCount(cfg *config.Manager) int {
	n := 0
	for i := range cfg.Services {
		if cfg.Services[i].IsEnabled() {
			n++
		}
	}
	return n
}
