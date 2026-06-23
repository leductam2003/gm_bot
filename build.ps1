# Build the Windows desktop app.
#
# -H windowsgui sets the GUI PE subsystem so Windows does NOT open a console
# window alongside the app — only the WebView2 UI shows. Startup/diagnostic
# logs still go to the logs/ folder next to the exe.
#
# Usage:  ./build.ps1            (or:  powershell -File build.ps1)
$env:CGO_ENABLED = "0"
go build -ldflags="-H windowsgui" -o zyper-bot.exe ./cmd/server
if ($LASTEXITCODE -eq 0) { Write-Host "Built zyper-bot.exe (no console window)." }
