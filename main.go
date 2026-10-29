package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const AppVersion = "1.9.11"

//go:embed web
var webFS embed.FS

func main() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}

	var (
		cfgPath       = flag.String("config", "config.json", "配置文件路径")
		addr          = flag.String("addr", "", "监听地址（默认 127.0.0.1:8080）")
		mockFlag      = flag.Bool("mock", false, "使用内置示例数据（无需飞书凭证），也可设置环境变量 MOCK=1")
		dataDir       = flag.String("data", "", "数据目录（选择配置与缓存，默认 data）")
		serverMode    = flag.Bool("server", false, "纯后台服务模式（不打开桌面独立窗口）")
		hostOpener    = flag.String("host-opener", "", "启动宿主机文件打开助手（如 127.0.0.1:18089）")
		hostOpenerDir = flag.String("host-opener-dir", "", "宿主机文件打开助手允许的下载根目录")
	)
	flag.Parse()

	if *hostOpener != "" {
		dir := *hostOpenerDir
		if dir == "" {
			dir = filepath.Join(os.Getenv("HOME"), "Documents", "FeishuTopics")
		}
		log.Fatal(ServeHostOpener(*hostOpener, dir))
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	cfg.ApplyEnv()
	if *addr != "" {
		cfg.Addr = *addr
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	cfg.Normalize()

	mock := *mockFlag
	if v := strings.ToLower(os.Getenv("MOCK")); v == "1" || v == "true" || v == "yes" {
		mock = true
	}
	if mock {
		cfg.DataDir = filepath.Join(cfg.DataDir, "mock")
	}

	cfg.DownloadDir = resolveDownloadDir(cfg, mock)
	auth := NewAuth(cfg, mock, filepath.Join(cfg.DataDir, "token.json"))
	var api API
	if mock {
		api = NewMockAPI(auth)
		log.Printf("模拟模式：使用内置示例数据（点击「用飞书登录」会走模拟登录）")
	} else {
		api = NewFeishuClient(cfg.BaseURL, cfg.AppID, cfg.AppSecret, auth, cfg.ResourceAuth)
		if cfg.Configured() {
			log.Printf("飞书模式（用户身份 OAuth）：app_id=%s redirect_uri=%s", cfg.AppID, cfg.RedirectURI)
		} else {
			log.Printf("警告：%v", ErrNotConfigured)
		}
	}

	sel, existed, err := LoadSelection(filepath.Join(cfg.DataDir, "selection.json"))
	if err != nil {
		log.Fatalf("加载群聊选择失败: %v", err)
	}
	if mock && !existed {
		_ = sel.Set([]string{"oc_mock_dev", "oc_mock_proj"})
	}

	cache := NewCache(filepath.Join(cfg.DataDir, "cache", "json"))
	svc := NewService(api, cache, cfg, auth)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	srv := NewServer(svc, sel, cfg, mock, sub)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatalf("监听地址 %s 失败: %v", cfg.Addr, err)
	}

	httpSrv := &http.Server{
		Handler:           logRequests(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if !cfg.DisableDownload {
		fmt.Printf("手动下载文件保存到：%s\n", cfg.DownloadDir)
	}
	appURL := fmt.Sprintf("http://%s", displayAddr(cfg.Addr))
	fmt.Printf("飞书话题浏览器已启动：%s\n", appURL)
	if !mock && cfg.Configured() {
		fmt.Printf("OAuth 回调地址（需在开发者后台「安全设置 → 重定向 URL」中添加）：%s\n", cfg.RedirectURI)
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// 桌面模式（Windows 下使用 Edge WebView2 原生独立窗口）
	if !*serverMode {
		if ran := runDesktop(appURL, cfg.DataDir); ran {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
			return
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-serverErr:
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func displayAddr(a string) string {
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		if strings.HasPrefix(a, ":") {
			return "localhost" + a
		}
		return a
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "127.0.0.1" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
