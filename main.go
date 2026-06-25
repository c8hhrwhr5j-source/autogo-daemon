// AutoGo iOS 常驻守护进程
// 基于 TrollStore 安装，提供稳定的文件读写 HTTP 接口
// 端口: 15200
//
// 功能:
//   GET  /ping              → 心跳检测
//   GET  /read?p=<base64>   → 读取文件内容
//   POST /write?p=<base64>  → 写入文件（body=内容）
//   POST /mkdir?p=<base64>  → 创建目录
//   GET  /ls?p=<base64>     → 列出目录内容（JSON数组）
//   GET  /stat?p=<base64>   → 文件/目录信息（JSON）
//   POST /setup             → 安装为开机自启动服务

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
	"path/filepath"
	"strings"
	"time"
)

const (
	DAEMON_PORT      = 15200
	DISCOVERY_PORT   = 15201 // UDP 广播发现端口
	DAEMON_VERSION   = "1.2.0"
	BIN_DEST         = "/usr/local/bin/autogod"
	PLIST_DEST       = "/Library/LaunchDaemons/com.autogo.daemon.plist"
	BROADCAST_PERIOD = 3 * time.Second // UDP 广播间隔
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("AutoGo Daemon v%s 启动中...", DAEMON_VERSION)

	// 自动自安装：如果不在固定路径运行，则自动安装为 launchd 服务
	exePath, _ := os.Executable()
	if exePath != BIN_DEST {
		log.Printf("检测到首发启动 (路径: %s)，自动安装为系统服务...", exePath)
		err := installToLaunchd(exePath)
		if err != nil {
			log.Printf("⚠️ 自安装失败: %v (将继续尝试直接运行)", err)
		} else {
			log.Printf("✅ 系统服务已安装，launchd 将接管守护进程")
			// 不退出，继续提供 HTTP 服务直到被 launchd 接管
		}
	} else {
		log.Printf("由 launchd 管理运行中...")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)
	mux.HandleFunc("/read", handleRead)
	mux.HandleFunc("/write", handleWrite)
	mux.HandleFunc("/mkdir", handleMkdir)
	mux.HandleFunc("/ls", handleList)
	mux.HandleFunc("/stat", handleStat)
	mux.HandleFunc("/setup", handleSetup)

	// 捕获所有请求，返回可用接口信息
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
				"GET  /read?p=<base64path>",
				"POST /write?p=<base64path>",
				"POST /mkdir?p=<base64path>",
				"GET  /ls?p=<base64path>",
				"GET  /stat?p=<base64path>",
				"POST /setup",
			},
		})
	})

	addr := fmt.Sprintf(":%d", DAEMON_PORT)
	// 显式绑定 0.0.0.0 确保 IPv4 socket（iOS 上 ":port" 会默认创建 IPv6-only socket）
	listener, err := net.Listen("tcp", "0.0.0.0"+addr)
	if err != nil {
		log.Fatalf("监听端口 %d 失败: %v", DAEMON_PORT, err)
	}

	log.Printf("✅ AutoGo Daemon 已启动，端口: %d (IPv4)", DAEMON_PORT)
	log.Printf("   心跳: http://<设备IP>:%d/ping", DAEMON_PORT)

	// 启动 UDP 广播（让中控快速发现本设备，无需全子网扫描）
	go broadcastPresence()

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if err := srv.Serve(listener); err != nil {
		log.Fatalf("服务异常退出: %v", err)
	}
}

// installToLaunchd 将当前二进制安装为 launchd 系统服务
// launchd 会负责 KeepAlive，即使 App 闪退也能持续运行
func installToLaunchd(exePath string) error {
	// 1. 复制到固定路径
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
	log.Printf("  ✓ 已复制到 %s", BIN_DEST)

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
    <integer>1</integer>
    <key>StandardOutPath</key>
    <string>/var/log/autogod.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/autogod.err</string>
    <key>WorkingDirectory</key>
    <string>/var/root</string>
</dict>
</plist>`, BIN_DEST)

	os.MkdirAll("/var/log", 0755)
	if err := os.MkdirAll(filepath.Dir(PLIST_DEST), 0755); err != nil {
		return fmt.Errorf("创建 plist 目录失败: %w", err)
	}
	if err := os.WriteFile(PLIST_DEST, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	log.Printf("  ✓ plist 已创建: %s", PLIST_DEST)

	// 3. 卸载旧服务（如果存在）再加载
	exec.Command("launchctl", "unload", PLIST_DEST).Run()
	output, err := exec.Command("launchctl", "load", PLIST_DEST).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl 加载失败: %s (err: %w)", string(output), err)
	}
	log.Printf("  ✓ launchd 服务已加载")
	log.Printf("")
	log.Printf("📱 AutoGo Daemon 已安装为系统服务！")
	log.Printf("   手机重启后自动运行，无需手动操作")
	log.Printf("   日志文件: /var/log/autogod.log")
	return nil
}

// ==================== UDP 广播发现 ====================

// broadcastPresence 周期性地向局域网广播设备存在信息
// 中控监听 15201 端口即可快速发现设备，无需全子网 TCP 扫描
func broadcastPresence() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("UDP 广播 panic 恢复: %v", r)
		}
	}()

	for {
		ips := getLocalIPv4s()
		for _, ip := range ips {
			broadcastIP := getBroadcastAddr(ip)
			if broadcastIP == "" {
				continue
			}

			msg := fmt.Sprintf(`{"service":"AutoGoDaemon","version":"%s","port":%d,"hostname":"%s"}`,
				DAEMON_VERSION, DAEMON_PORT, getHostname())

			addr := &net.UDPAddr{IP: net.ParseIP(broadcastIP), Port: DISCOVERY_PORT}
			conn, err := net.DialUDP("udp", nil, addr)
			if err != nil {
				continue
			}
			conn.Write([]byte(msg))
			conn.Close()
		}
		time.Sleep(BROADCAST_PERIOD)
	}
}

// getLocalIPv4s 获取本机所有非回环 IPv4 地址
func getLocalIPv4s() []net.IP {
	var ips []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
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

// getBroadcastAddr 根据本机 IP 计算子网广播地址（假设 /24 子网）
func getBroadcastAddr(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.255", ip4[0], ip4[1], ip4[2])
}

// getHostname 获取设备主机名
func getHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "iOS"
	}
	return name
}

// ==================== API 处理函数 ====================

func handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("pong"))
}

func handleRead(w http.ResponseWriter, r *http.Request) {
	path := getPathParam(r)
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusNotFound)
		log.Printf("读取失败 %s: %v", path, err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
	log.Printf("读取成功 %s (%d bytes)", path, len(data))
}

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

	// 确保父目录存在
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir error: %v", err), http.StatusInternalServerError)
		log.Printf("创建父目录失败 %s: %v", path, err)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body error: %v", err), http.StatusBadRequest)
		return
	}

	// 判断是文本还是二进制，选择合适的写入方式
	if isTextContent(body) {
		// 文本文件：统一换行符为 \n
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

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok"))
	log.Printf("写入成功 %s (%d bytes)", path, len(body))
}

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
		log.Printf("创建目录失败 %s: %v", path, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok"))
	log.Printf("创建目录成功 %s", path)
}

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

// handleSetup 安装为开机自启动服务
// 将自己复制到 /usr/local/bin/autogod，并创建 launchd plist
func handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var result []string

	// 1. 获取当前可执行文件路径
	exePath, err := os.Executable()
	if err != nil {
		http.Error(w, fmt.Sprintf("无法获取当前路径: %v", err), http.StatusInternalServerError)
		return
	}

	// 2. 复制到固定路径
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

	// 3. 创建 launchd plist
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

	// 4. 尝试加载 plist
	output, err := exec.Command("launchctl", "load", PLIST_DEST).CombinedOutput()
	if err != nil {
		result = append(result, fmt.Sprintf("launchctl load 输出: %s (err: %v)", string(output), err))
	} else {
		result = append(result, "launchd 服务已加载")
	}

	// 5. 尝试创建 log 目录
	os.MkdirAll("/var/log", 0755)

	result = append(result, "✅ 开机自启安装完成！重启后会自动运行")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"steps":  result,
	})
}

// ==================== 辅助函数 ====================

// getPathParam 从请求中提取 base64 编码的路径参数
func getPathParam(r *http.Request) string {
	encoded := r.URL.Query().Get("p")
	if encoded == "" {
		return ""
	}
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		// 兼容未编码的原始路径
		return encoded
	}
	return string(decoded)
}

// isTextContent 判断内容是否可能是文本（粗略检测）
func isTextContent(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	// 检查前 512 字节，如果 95%% 以上是可打印字符，认为是文本
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
