//go:build windows

package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/jchv/go-webview2"
)

// runDesktop starts a native WebView2 window on Windows.
// It blocks until the window is closed, returning true if the window ran successfully.
func runDesktop(url, dataDir string) bool {
	cacheDir := filepath.Join(dataDir, "webview2")
	_ = os.MkdirAll(cacheDir, 0o755)

	opts := webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  cacheDir,
		WindowOptions: webview2.WindowOptions{
			Title:  "网页呆呆鸟",
			Width:  1360,
			Height: 880,
			IconId: 1,
			Center: true,
		},
	}

	w := webview2.NewWithOptions(opts)
	if w == nil {
		log.Printf("WebView2 初始化失败，将回退至浏览器模式")
		return false
	}
	defer w.Destroy()

	w.Navigate(url)
	w.Run()
	return true
}
