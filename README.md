# AutoGo iOS 常驻守护进程

基于 TrollStore 的后台守护进程，替代 Filza WebDAV (11111端口) 的不稳定通信。

## 解决的问题

- ❌ **旧方案**: Filza WebDAV 端口(11111)经常无故关闭，导致中控连接中断
- ✅ **新方案**: 独立守护进程，基于 launchd 开机自启，永久保持后台运行

## 通信接口（默认端口 15200）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/ping` | 心跳检测，返回 `pong` |
| GET | `/read?p=<base64path>` | 读取文件内容 |
| POST | `/write?p=<base64path>` | 写入文件（body=内容） |
| POST | `/mkdir?p=<base64path>` | 创建目录 |
| GET | `/ls?p=<base64path>` | 列出目录（JSON 数组） |
| GET | `/stat?p=<base64path>` | 文件信息（JSON） |
| POST | `/setup` | 安装开机自启服务 |

## 构建 IPA

### 方式一：GitHub Actions（推荐，无需 Mac）

1. 将代码推送到 GitHub 仓库
2. 在仓库页面点击 **Actions** 标签
3. 选择 **Build iOS IPA** → **Run workflow**
4. 等待构建完成，下载 `AutoGoDaemon-iOS` 产物

> **自动触发**: 每次 push 到 main/master 分支也会自动构建。

### 方式二：macOS 本地构建

```bash
# 安装签名工具
brew install ldid

# 编译
CGO_ENABLED=1 GOOS=ios GOARCH=arm64 \
  go build -ldflags="-s -w" -o AutoGoDaemon .

# TrollStore 签名
ldid -Sentitlements.plist AutoGoDaemon

# 打包 IPA
mkdir -p Payload/AutoGoDaemon.app
cp AutoGoDaemon Info.plist Payload/AutoGoDaemon.app/
zip -r AutoGoDaemon-1.0.0.ipa Payload
```

> ⚠️ **Windows 无法构建 iOS IPA**，因为 iOS 交叉编译需要 Xcode 工具链。

## 安装到 iOS 设备

1. 将 IPA 通过 AirDrop / 微信 发送到 iOS 设备
2. 在 iOS 上用 **TrollStore** 打开并安装
3. 打开一次 AutoGo Daemon 应用（闪退是正常的，守护进程已在后台运行）
4. 验证：访问 `http://设备IP:15200/ping`，应返回 `pong`

## 启用开机自启

通过中控端的 **安装守护** 按钮自动完成，或手动：

```bash
curl -X POST http://设备IP:15200/setup
```

这将自动：
- 复制守护进程到 `/usr/local/bin/autogod`
- 创建 `/Library/LaunchDaemons/com.autogo.daemon.plist`
- 加载 launchd 服务

**重启 iOS 设备后，守护进程会自动启动。**

## 中控端适配

中控端已自动适配双通道：
- **优先尝试守护进程** (15200端口)
- **自动降级到 Filza WebDAV** (11111端口，作为备用)
- 全程无需用户手动切换

## 技术细节

- **语言**: Go (交叉编译到 iOS arm64)
- **端口**: 15200 (TCP)
- **协议**: HTTP RESTful
- **路径编码**: Base64 URL Encoding
- **进程管理**: iOS launchd (KeepAlive + RunAtLoad)
- **依赖**: 仅标准库，无外部依赖

## 故障排除

| 问题 | 排查 |
|------|------|
| 无法连接 15200 | 检查是否已打开 App 一次；检查设备和电脑在同一局域网 |
| 写入失败 | 检查目标路径权限 (TrollStore 应用通常拥有完整权限) |
| 重启后未自启 | 确认已执行 `/setup`；检查 `/Library/LaunchDaemons/` 下 plist 是否存在 |
