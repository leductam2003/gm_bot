# Package a downloadable build for the in-app updater.
#
# Produces  dist/zyper-bot-windows-v<version>.zip  containing zyper-bot.exe + web/.
# The in-app "Install & Restart" downloads exactly this asset, so a release MUST attach
# it (a bare git tag has no build to download). Data (db, vault.key, .env, logs) is NOT
# packaged — it lives next to each user's exe and is kept across updates.
#
# Usage:
#   ./release.ps1                 # build + zip only
#   ./release.ps1 -Publish        # build + zip, then create the GitHub release & upload
#                                 #   the asset (requires the `gh` CLI, authenticated)

param([switch]$Publish)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Read the version from internal/config/config.go (const Version = "x.y.z").
$verLine = Select-String -Path "internal/config/config.go" -Pattern 'Version\s*=\s*"([^"]+)"' | Select-Object -First 1
if (-not $verLine) { throw "could not find Version in internal/config/config.go" }
$version = $verLine.Matches[0].Groups[1].Value
$tag = "v$version"
Write-Host "Packaging $tag"

# Clean build of the GUI exe.
$env:CGO_ENABLED = "0"
go build -ldflags="-H windowsgui" -o zyper-bot.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

# Stage exe + web/ into dist/stage, then zip.
$stage = "dist/stage"
if (Test-Path "dist") { Remove-Item "dist" -Recurse -Force }
New-Item -ItemType Directory -Force -Path $stage | Out-Null
Copy-Item "zyper-bot.exe" $stage
Copy-Item "web" $stage -Recurse
# Ship only the runtime UI — drop any dev/tooling junk that leaked into web/ (e.g. .omc
# state, dotfiles) so a release never carries session artifacts.
Get-ChildItem (Join-Path $stage "web") -Force -Directory -Filter ".*" | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue

$zip = "dist/zyper-bot-windows-$tag.zip"
Compress-Archive -Path "$stage/*" -DestinationPath $zip -Force
Remove-Item $stage -Recurse -Force
Write-Host "Built $zip"

if ($Publish) {
  if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw "gh CLI not found - install GitHub CLI or upload $zip to the $tag release manually"
  }
  # Create the release if it doesn't exist, then upload (clobber to allow re-runs).
  gh release view $tag *> $null
  if ($LASTEXITCODE -ne 0) { gh release create $tag --title $tag --notes "zyper-bot $tag" }
  gh release upload $tag $zip --clobber
  Write-Host "Published $tag with asset $(Split-Path $zip -Leaf)"
} else {
  Write-Host "Attach $zip to the $tag GitHub release (or re-run with -Publish if you have gh)."
}
