# AutoGo iOS Daemon - 构建脚本 (仅限 macOS, Windows 无法交叉编译 iOS)
# 推荐使用 GitHub Actions 自动构建: .github/workflows/build.yml
# 本地 macOS 构建:
#   CGO_ENABLED=1 GOOS=ios GOARCH=arm64 go build -ldflags="-s -w" -o AutoGoDaemon .
#   ldid -Sentitlements.plist AutoGoDaemon
#   mkdir -p Payload/AutoGoDaemon.app
#   cp AutoGoDaemon Payload/AutoGoDaemon.app/
#   cp Info.plist Payload/AutoGoDaemon.app/
#   zip -r AutoGoDaemon-1.0.0.ipa Payload

Write-Host "======================================" -ForegroundColor Cyan
Write-Host "  AutoGo iOS Daemon - Build Info" -ForegroundColor Cyan
Write-Host "======================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "  iOS IPA 无法在 Windows 上构建。" -ForegroundColor Yellow
Write-Host "  原因: GOOS=ios GOARCH=arm64 需要 Xcode 工具链 (仅限 macOS)" -ForegroundColor Yellow
Write-Host ""
Write-Host "  推荐方式:" -ForegroundColor Green
Write-Host "    1. GitHub Actions: 推送代码到 GitHub 自动构建" -ForegroundColor White
Write-Host "       https://github.com/<你的仓库>/actions" -ForegroundColor White
Write-Host ""
Write-Host "    2. macOS 本地构建:" -ForegroundColor White
Write-Host "       brew install ldid" -ForegroundColor Gray
Write-Host "       CGO_ENABLED=1 GOOS=ios GOARCH=arm64 go build -ldflags=\"-s -w\" -o AutoGoDaemon ." -ForegroundColor Gray
Write-Host "       ldid -Sentitlements.plist AutoGoDaemon" -ForegroundColor Gray
Write-Host "       mkdir -p Payload/AutoGoDaemon.app" -ForegroundColor Gray
Write-Host "       cp AutoGoDaemon Info.plist Payload/AutoGoDaemon.app/" -ForegroundColor Gray
Write-Host "       zip -r AutoGoDaemon-1.0.0.ipa Payload" -ForegroundColor Gray
Write-Host ""
Write-Host "======================================" -ForegroundColor Cyan
