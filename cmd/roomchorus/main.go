package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"roomchorus/internal/netutil"
	"roomchorus/internal/server"
)

func main() {
	port := flag.Int("port", 8787, "HTTP/WebSocket listening port")
	iface := flag.String("iface", "", "Preferred LAN network interface name (e.g. en0)")
	source := flag.String("source", "", "Optional auto-start source mode: system | input | test-tone")
	noHostMonitor := flag.Bool("no-host-monitor", false, "Disable synchronized local host speaker playback")
	noToken := flag.Bool("no-token", false, "Disable random join token verification")
	fixedToken := flag.String("token", "", "Optional fixed join token instead of random")
	allowRemoteHost := flag.Bool("allow-remote-host", false, "Allow non-loopback access to /host console")
	openConsole := flag.Bool("open", false, "Automatically open host console in default browser")
	flag.Parse()

	srv, err := server.New(server.Config{
		Port:                  *port,
		InterfaceName:         *iface,
		DisableToken:          *noToken,
		FixedToken:            *fixedToken,
		AllowRemoteHost:       *allowRemoteHost,
		DisableMonitorDefault: *noHostMonitor,
	})
	if err != nil {
		log.Fatalf("初始化 RoomChorus 服务失败: %v", err)
	}

	if err := srv.Start(); err != nil {
		log.Fatalf("启动网络服务失败: %v", err)
	}

	if *source != "" {
		if err := srv.StartBroadcast(*source); err != nil {
			_ = srv.Close(context.Background())
			log.Fatalf("自动启动音频广播 (%s) 失败: %v", *source, err)
		}
	}

	actualPort := srv.Port()
	hostURL := fmt.Sprintf("http://127.0.0.1:%d/host", actualPort)
	ipv4URL, mdnsURL := srv.JoinURLs()
	state := srv.HostState()

	fmt.Println("====================================================")
	fmt.Println("  RoomChorus · macOS 原生免驱局域网同步音频")
	fmt.Println("====================================================")
	fmt.Printf("主机控制台 : %s\n", hostURL)
	fmt.Printf("局域网收听 : %s\n", ipv4URL)
	fmt.Printf("mDNS 入口  : %s\n", mdnsURL)
	if state.TokenRequired {
		fmt.Printf("房间口令   : %s\n", state.JoinToken)
	} else {
		fmt.Println("房间口令   : 已关闭 (-no-token)")
	}
	if qr := netutil.TerminalQR(ipv4URL); qr != "" {
		fmt.Println("\n手机扫码收听（IPv4 局域网入口）：")
		fmt.Print(qr)
	}
	fmt.Println("按 Ctrl+C 停止服务并恢复系统音频路由。")

	if *openConsole {
		_ = exec.Command("/usr/bin/open", hostURL).Start()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\n正在停止广播并释放 CoreAudio / WebRTC 资源...")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		log.Printf("关闭服务时遇到警告: %v", err)
	}
}
