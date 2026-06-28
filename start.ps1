$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot
$binDir = Join-Path $PSScriptRoot "bin"
$binPath = Join-Path $binDir "blackbox-minimax-bridge.exe"
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
go build -buildvcs=false -trimpath -ldflags="-s -w" -o $binPath .
& $binPath
