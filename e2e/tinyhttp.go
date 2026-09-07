// 联调用极简 HTTP 探针服务器: 监听 :9090, GET /healthz 返回 200。
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	addr := flag.String("addr", ":9090", "监听地址")
	flag.Parse()
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: *addr}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		_ = srv.Close()
		os.Exit(0)
	}()
	fmt.Printf("tinyhttp listening on %s\n", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "listen error:", err)
		os.Exit(1)
	}
}
