// AutoGo iOS 常驻守护进程 v1.3.0
//
// 架构：双模式运行
//   App 模式（从 .app bundle 启动）：安装 launchd 守护进程 → 退出
//   守护进程模式（launchd /usr/local/bin/autogod）：运行 HTTP + UDP 广播
//
// 端口:
//   15200 (TCP)   HTTP 文件读写接口 + 设备发现
//   15201 (UDP)   广播发现（辅助快速定位）
//
// API:
//   GET  /ping              → 心跳检测  → "pong"
//   GET  /info              → 设备信息  → JSON
//   GET  /read?p=<base64>   → 读取文件
//   POST /write?p=<base64>  → 写入文件
//   POST /mkdir?p=<base64>  → 创建目录
//   GET  /ls?p=<base64>     → 列出目录
//   GET  /stat?p=<base64>   → 文件信息
//   POST /setup             → 安装为系统服务

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	DAEMON_PORT      = 15200
	DISCOVERY_PORT   = 15201
	DAEMON_VERSION   = "1.3.0"
	BIN_DEST         = "/usr/local/bin/autogod"
	PLIST_DEST       = "/Library/LaunchDaemons/com.autogo.daemon.plist"
	LOG_DIR          = "/var/log"
	LOG_FILE         = "/var/log/autogod.log"
	BROADCAST_PERIOD = 5 * time.Second
)

// ==================== 主入口 ====================

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	exePath, _ := os.Executable()
	isDaemon := (exePath == BIN_DEST)

	if isDaemon {
		// ==================== 守护进程模式 ====================
		// 由 launchd 管理运行，不受 iOS App 生命周期限制
		runAsDaemon()
	} else {
		// ==================== App 模式 ====================
		// 从 .app bundle 启动，负责安装守护进程后立即退出
		// 避免 iOS SpringBoard watchdog 杀进程
		log.Printf("AutoGo Daemon v%s - App 模式 (路径: %s)", DAEMON_VERSION, exePath)
		log.Printf("正在安装系统守护进程...")
		err := installToLaunchd(exePath)
		if err != nil {
			log.Printf("❌ 安装失败: %v", err)
			// 安装失败时，尝试 fallback：直接以守护进程模式运行
			log.Printf("尝试 fallback 直接运行...")
			runAsDaemon()
			return
		}
		log.Printf("✅ 守护进程已安装（v%s），App 进程即将退出", DAEMON_VERSION)
		log.Printf("   守护进程由 launchd 管理，开机自启、闪退自动恢复")
		// 短暂等待确保 launchd 启动守护进程
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}
}

// ==================== 守护进程模式 ====================

func runAsDaemon() {
	// 顶层 panic 恢复 — 确保任何崩溃都能被捕获并记录
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			log.Printf("🔥🔥🔥 守护进程 PANIC: %v\n堆栈:\n%s", r, buf[:n])
			time.Sleep(3 * time.Second)
			os.Exit(1) // 非零退出码，launchd KeepAlive 会自动重启
		}
	}()

	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("AutoGo Daemon v%s - 守护进程模式", DAEMON_VERSION)
	log.Printf("PID: %d", os.Getpid())
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 日志双写（stdout + 文件）
	setupLogging()
	defer log.Printf("守护进程已停止")

	// 信号处理 — 优雅关闭
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// 构建路由
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)
	mux.HandleFunc("/info", handleInfo) // 新增：设备信息接口
	mux.HandleFunc("/read", handleRead)
	mux.HandleFunc("/write", handleWrite)
	mux.HandleFunc("/mkdir", handleMkdir)
	mux.HandleFunc("/ls", handleList)
	mux.HandleFunc("/stat", handleStat)
	mux.HandleFunc("/setup", handleSetup)
	mux.HandleFunc("/", handleRoot)

	// HTTP 服务 — 显式绑定 IPv4
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", DAEMON_PORT))
	if err != nil {
		log.Fatalf("监听端口 %d 失败: %v", DAEMON_PORT, err)
	}
	log.Printf("✅ HTTP 服务已启动: 0.0.0.0:%d", DAEMON_PORT)

	// 启动 UDP 广播发现（后台持续运行）
	go broadcastPresence()

	// 启动健康自检
	go healthMonitor()

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// 监听信号，优雅关闭
	go func() {
		sig := <-sigCh
		log.Printf("收到信号 %v，优雅关闭...", sig)
		srv.Close()
	}()

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务异常退出: %v", err)
		os.Exit(1)
	}
}

// setupLogging 配置日志双写
func setupLogging() {
	os.MkdirAll(LOG_DIR, 0755)
	f, err := os.OpenFile(LOG_FILE, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
}

// healthMonitor 定时自检 HTTP 服务是否正常
func healthMonitor() {
	defer func() { recover() }()
	time.Sleep(10 * time.Second) // 等待服务完全启动
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", DAEMON_PORT))
		if err != nil {
			log.Printf("⚠️ 健康检查失败: %v", err)
		} else {
			resp.Body.Close()
		}
	}
}

// ==================== launchd 自安装 ====================

func installToLaunchd(exePath string) error {
	// 1. 复制二进制到固定路径
	input, err := os.ReadFile(exePath)
	if err != nil {
		return fmt.Errorf("读取自身失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(BIN_DEST), 0755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}
	if err := os.WriteFile(BIN_DEST, input, 0755); err != nil {
		return fmt.Errorf("复制二进制失败: %w", err)
	}
	log.Printf("  ✓ 已复制: %s (%d bytes)", BIN_DEST, len(input))

	// 2. 创建 launchd plist
	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.autogo.daemon</string>
    <key>Program</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>StandardOutPath</key>
    <string>/var/log/autogod.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/autogod.err</string>
    <key>WorkingDirectory</key>
    <string>/var/root</string>
</dict>
</plist>`, BIN_DEST)

	os.MkdirAll(LOG_DIR, 0755)
	if err := os.MkdirAll(filepath.Dir(PLIST_DEST), 0755); err != nil {
		return fmt.Errorf("创建 plist 目录失败: %w", err)
	}
	if err := os.WriteFile(PLIST_DEST, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	log.Printf("  ✓ plist: %s", PLIST_DEST)

	// 3. 加载服务
	exec.Command("launchctl", "unload", PLIST_DEST).Run() // 先卸载旧版本
	output, err := exec.Command("launchctl", "load", PLIST_DEST).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl load 失败: %s (%w)", string(output), err)
	}
	log.Printf("  ✓ launchd 服务已加载 (KeepAlive+ThrottleInterval=5s)")
	log.Printf("")
	log.Printf("📱 安装完成！守护进程状态：")
	log.Printf("   • 当前运行中（launchd 管理）")
	log.Printf("   • 开机自启（RunAtLoad）")
	log.Printf("   • 崩溃自动恢复（KeepAlive）")
	log.Printf("   • 日志: %s", LOG_FILE)
	return nil
}

// ==================== UDP 广播发现 ====================

// broadcastPresence 向局域网广播设备信息
// 中控监听 15201 端口即可秒级发现设备
func broadcastPresence() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("UDP 广播 PANIC (已恢复): %v", r)
			go broadcastPresence() // 重启广播
		}
	}()

	// 准备广播消息
	hostname := getHostname()
	msg := fmt.Sprintf(`{"service":"AutoGoDaemon","version":"%s","port":%d,"hostname":"%s"}`,
		DAEMON_VERSION, DAEMON_PORT, hostname)

	for {
		ips := getLocalIPv4s()
		if len(ips) == 0 {
			time.Sleep(BROADCAST_PERIOD)
			continue
		}

		for _, ip := range ips {
			broadcastIP := getBroadcastAddr(ip)
			if broadcastIP == "" {
				continue
			}

			// 关键修复：使用 syscall.RawConn 设置 SO_BROADCAST
			// 普通 DialUDP 不会自动设置广播权限
			sendBroadcast(broadcastIP, []byte(msg))
		}
		time.Sleep(BROADCAST_PERIOD)
	}
}

// sendBroadcast 通过设置 SO_BROADCAST 的 UDP socket 发送广播
func sendBroadcast(broadcastAddr string, data []byte) {
	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", broadcastAddr, DISCOVERY_PORT))
	if err != nil {
		return
	}

	// 创建 UDP socket
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return
	}
	defer conn.Close()

	// 获取底层文件描述符，设置 SO_BROADCAST
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return
	}
	rawConn.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})

	conn.Write(data)
}

// getLocalIPv4s 获取本机所有启用的 IPv4 地址
func getLocalIPv4s() []net.IP {
	var ips []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			// 也保留回环接口上的非 127.x 地址（某些网络配置下有意义）
			// 但一般情况下跳过回环
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := n.IP.To4()
			if ip != nil && !ip.IsLoopback() {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

// getBroadcastAddr 计算子网广播地址
func getBroadcastAddr(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.255", ip4[0], ip4[1], ip4[2])
}

// getHostname 获取设备名
func getHostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "iOS"
	}
	return name
}

// ==================== API 处理函数 ====================

// handlePing 心跳检测
func handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("pong"))
}

// handleInfo 设备信息（供中控快速识别设备）
func handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ips := getLocalIPv4s()
	ipStrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		ipStrs = append(ipStrs, ip.String())
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service":  "AutoGoDaemon",
		"version":  DAEMON_VERSION,
		"port":     DAEMON_PORT,
		"hostname": getHostname(),
		"ips":      ipStrs,
		"uptime":   time.Now().Unix(),
	})
}

// handleRoot 根路径返回可用接口
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service": "AutoGo Daemon",
		"version": DAEMON_VERSION,
		"port":    DAEMON_PORT,
		"endpoints": []string{
			"GET  /ping",
			"GET  /info",
			"GET  /read?p=<base64path>",
			"POST /write?p=<base64path>",
			"POST /mkdir?p=<base64path>",
			"GET  /ls?p=<base64path>",
			"GET  /stat?p=<base64path>",
			"POST /setup",
		},
	})
}

// handleRead 读取文件
func handleRead(w http.ResponseWriter, r *http.Request) {
	path := getPathParam(r)
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

// handleWrite 写入文件
func handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := getPathParam(r)
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir error: %v", err), http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body error: %v", err), http.StatusBadRequest)
		return
	}
	if isTextContent(body) {
		content := strings.ReplaceAll(string(body), "\r\n", "\n")
		content = strings.ReplaceAll(content, "\r", "\n")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			http.Error(w, fmt.Sprintf("write error: %v", err), http.StatusInternalServerError)
			return
		}
	} else {
		if err := os.WriteFile(path, body, 0644); err != nil {
			http.Error(w, fmt.Sprintf("write error: %v", err), http.StatusInternalServerError)
			return
		}
	}
	w.Write([]byte("ok"))
}

// handleMkdir 创建目录
func handleMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := getPathParam(r)
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

// handleList 列出目录
func handleList(w http.ResponseWriter, r *http.Request) {
	path := getPathParam(r)
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("readdir error: %v", err), http.StatusNotFound)
		return
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(names)
}

type fileStat struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modtime"`
	IsDir   bool   `json:"isdir"`
}

// handleStat 获取文件信息
func handleStat(w http.ResponseWriter, r *http.Request) {
	path := getPathParam(r)
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("stat error: %v", err), http.StatusNotFound)
		return
	}
	s := fileStat{
		Name:    info.Name(),
		Size:    info.Size(),
		ModTime: info.ModTime().Format(time.RFC3339),
		IsDir:   info.IsDir(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

// handleSetup 手动安装为系统服务（通过 HTTP 调用）
func handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var result []string
	exePath, err := os.Executable()
	if err != nil {
		http.Error(w, fmt.Sprintf("无法获取当前路径: %v", err), http.StatusInternalServerError)
		return
	}
	input, err := os.ReadFile(exePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("读取自身失败: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(filepath.Dir(BIN_DEST), 0755); err != nil {
		http.Error(w, fmt.Sprintf("创建目标目录失败: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(BIN_DEST, input, 0755); err != nil {
		http.Error(w, fmt.Sprintf("写入目标文件失败: %v", err), http.StatusInternalServerError)
		return
	}
	result = append(result, fmt.Sprintf("二进制已复制到 %s", BIN_DEST))

	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.autogo.daemon</string>
    <key>Program</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>StandardOutPath</key>
    <string>/var/log/autogod.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/autogod.err</string>
</dict>
</plist>`, BIN_DEST)

	if err := os.MkdirAll(filepath.Dir(PLIST_DEST), 0755); err != nil {
		result = append(result, fmt.Sprintf("警告: 创建 plist 目录失败: %v", err))
	} else if err := os.WriteFile(PLIST_DEST, []byte(plistContent), 0644); err != nil {
		result = append(result, fmt.Sprintf("警告: 写入 plist 失败: %v", err))
	} else {
		result = append(result, fmt.Sprintf("launchd plist 已创建: %s", PLIST_DEST))
	}

	output, err := exec.Command("launchctl", "load", PLIST_DEST).CombinedOutput()
	if err != nil {
		result = append(result, fmt.Sprintf("launchctl load 输出: %s (err: %v)", string(output), err))
	} else {
		result = append(result, "launchd 服务已加载")
	}
	os.MkdirAll(LOG_DIR, 0755)
	result = append(result, "✅ 开机自启安装完成！")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"steps":  result,
	})
}

// ==================== 辅助函数 ====================

func getPathParam(r *http.Request) string {
	encoded := r.URL.Query().Get("p")
	if encoded == "" {
		return ""
	}
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return encoded
	}
	return string(decoded)
}

func isTextContent(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	checkLen := len(data)
	if checkLen > 512 {
		checkLen = 512
	}
	printable := 0
	for _, b := range data[:checkLen] {
		if b >= 32 && b < 127 || b == '\n' || b == '\r' || b == '\t' {
			printable++
		}
	}
	return float64(printable)/float64(checkLen) > 0.95
}
